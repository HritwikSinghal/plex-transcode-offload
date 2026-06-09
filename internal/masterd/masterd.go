// Package masterd is the master-side daemon (:32499): segment receiver,
// media/codecs file server, PMS relay, and worker health cache.
package masterd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/HritwikSinghal/plex-transcode-offload/internal/authtok"
	"github.com/HritwikSinghal/plex-transcode-offload/internal/config"
	"github.com/HritwikSinghal/plex-transcode-offload/internal/protocol"
)

// Run executes the masterd role with its CLI args (e.g. --config <path>)
// and returns the process exit code.
func Run(args []string) int {
	fs := flag.NewFlagSet("prt-masterd", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to the masterd JSON config")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "prt-masterd: --config is required")
		return 2
	}
	cfg, err := config.LoadMasterd(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "prt-masterd: %v\n", err)
		return 1
	}
	token, err := authtok.LoadToken(cfg.TokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "prt-masterd: %v\n", err)
		return 1
	}
	srv, err := newServer(cfg, token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "prt-masterd: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.sessions.runReaper(ctx)
	go srv.runProber(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	srv.log.Printf("listening on %s (advertise %s)", cfg.Listen, cfg.AdvertiseURL)

	select {
	case <-ctx.Done():
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shCtx)
		srv.log.Printf("shut down")
		return 0
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			srv.log.Printf("serve: %v", err)
			return 1
		}
		return 0
	}
}

// server holds the shared state of one prt-masterd instance.
type server struct {
	cfg   config.MasterdConfig
	token string // shared bearer token (also the media URL signing secret)

	transcodeRoot string   // cleaned cfg.TranscodeRoot
	mediaRoots    []string // cleaned cfg.MediaRoots

	sessions *sessionStore
	codecs   *codecCache
	workers  *workerCache
	relay    http.Handler

	log *log.Logger
}

// newServer validates derived state and wires the subsystems. It does not
// start the reaper or prober goroutines (Run does; tests need not).
func newServer(cfg config.MasterdConfig, token string) (*server, error) {
	pms, err := url.Parse(cfg.PMSURL)
	if err != nil {
		return nil, fmt.Errorf("masterd: bad pms_url %q: %w", cfg.PMSURL, err)
	}
	s := &server{
		cfg:           cfg,
		token:         token,
		transcodeRoot: filepath.Clean(cfg.TranscodeRoot),
		sessions:      newSessionStore(time.Duration(cfg.SessionTTLSec) * time.Second),
		codecs:        newCodecCache(cfg.CodecsDir),
		workers:       newWorkerCache(cfg.Workers),
		log:           log.New(os.Stderr, "prt-masterd: ", log.LstdFlags|log.Lmsgprefix),
	}
	for _, r := range cfg.MediaRoots {
		s.mediaRoots = append(s.mediaRoots, filepath.Clean(r))
	}
	s.relay = newRelayProxy(pms, s.log)
	return s, nil
}

// routes builds the masterd mux. Auth is per-route:
//   - shared bearer token: sessions register/close, codecs, relay, workers;
//   - per-session push token: segment PUTs (checked in the handler);
//   - HMAC-signed query: GET /v1/media.
func (s *server) routes() http.Handler {
	auth := func(h http.HandlerFunc) http.Handler {
		return authtok.Middleware(s.token, h)
	}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/sessions", auth(s.handleRegisterSession))
	mux.Handle("DELETE /v1/sessions/{id}", auth(s.handleDeleteSession))
	mux.Handle("PUT /v1/sessions/{id}/files/{relpath...}", http.HandlerFunc(s.handlePutFile))
	mux.Handle("GET /v1/media", http.HandlerFunc(s.handleMedia))
	mux.Handle("GET /v1/codecs/{build}/manifest", auth(s.handleCodecsManifest))
	mux.Handle("GET /v1/codecs/{build}/files/{name}", auth(s.handleCodecsFile))
	mux.Handle("GET /relay/{id}/{rest...}", auth(s.handleRelay))
	mux.Handle("POST /relay/{id}/{rest...}", auth(s.handleRelay))
	mux.Handle("GET /v1/workers", auth(s.handleWorkers))
	return mux
}

// writeJSON writes v as the JSON response body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a protocol.ErrorBody response.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, protocol.ErrorBody{Error: code, Message: msg})
}

// isUnder reports whether cleaned path p lies STRICTLY under cleaned root.
func isUnder(root, p string) bool {
	if root == "/" {
		return p != "/" && strings.HasPrefix(p, "/")
	}
	return strings.HasPrefix(p, root+"/")
}

// atOrUnder reports whether cleaned path p is root itself or lies under it.
func atOrUnder(root, p string) bool {
	return p == root || isUnder(root, p)
}
