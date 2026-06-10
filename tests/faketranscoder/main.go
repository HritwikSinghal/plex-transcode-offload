// Command faketranscoder is the end-to-end test stand-in for the real
// "Plex Transcoder". prt-workerd spawns it with cwd = the job out dir and a
// substituted argv; it behaves like the real transcoder's segment plane:
// chunk files are written to the cwd and seglist/progress POSTs go to the
// gating-proxy relay URLs found in argv (the {{PMS}} substitution).
//
// Modes (FAKE_MODE env, forwarded via JobRequest.Env):
//   - "" / "ok": GET the -i media URL (signed masterd URL), write chunks
//     0+1, POST progress, POST a seglist announcing 0+1, write chunk 2,
//     POST a seglist announcing 2, exit 0.
//   - "exit3":   exit 3 immediately (exit-code propagation case).
//   - "sleep":   wait for SIGTERM (or 120s); on SIGTERM write the file named
//     by FAKE_TERM_FILE and exit 0 (implicit-cancel case).
//
// Failure exit codes (make e2e failures diagnosable from the exit event):
// 7 = sleep mode misbehaved, 8 = media fetch failed, 9 = POST failed or
// argv lacked the expected URLs.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// httpClient bounds every request so a stuck gate fails the test instead of
// hanging it forever.
var httpClient = &http.Client{Timeout: 60 * time.Second}

func main() {
	switch os.Getenv("FAKE_MODE") {
	case "exit3":
		os.Exit(3)
	case "sleep":
		sleepUntilTerm()
	default:
		runOK()
	}
}

func fatalf(code int, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "faketranscoder: "+format+"\n", args...)
	os.Exit(code)
}

// sleepUntilTerm blocks until SIGTERM, then records the signal delivery in
// FAKE_TERM_FILE (which lives OUTSIDE the job dir: LOST jobs have their dir
// removed) and exits 0.
func sleepUntilTerm() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	select {
	case <-ch:
		if f := os.Getenv("FAKE_TERM_FILE"); f != "" {
			if err := os.WriteFile(f, []byte("sigterm\n"), 0o644); err != nil {
				fatalf(7, "write term file: %v", err)
			}
		}
		os.Exit(0)
	case <-time.After(120 * time.Second):
		fatalf(7, "no SIGTERM within 120s")
	}
}

func runOK() {
	seglist := argContaining("/seglist")
	progress := argContaining("/progress")
	if seglist == "" || progress == "" {
		fatalf(9, "argv lacks seglist/progress URLs: %q", os.Args)
	}
	if media := argAfter("-i"); strings.HasPrefix(media, "http://") || strings.HasPrefix(media, "https://") {
		fetchMedia(media)
	}
	writeChunk("media-00000.ts")
	writeChunk("media-00001.ts")
	post(progress, "frame=42 fps=24.0 progress=continue")
	post(seglist, "media-00000.ts,0.000,2.002\nmedia-00001.ts,2.002,2.002\n")
	writeChunk("media-00002.ts")
	post(seglist, "media-00002.ts,4.004,2.002\n")
}

// chunkPayload must stay in sync with its twin in tests/e2e_test.go: the
// fake PMS verifies received seglists against this exact content.
func chunkPayload(name string) string {
	return "prt-e2e-chunk:" + name + "\n"
}

// writeChunk writes one complete chunk file into the cwd (the worker out
// dir); the close generates the IN_CLOSE_WRITE the segment pusher watches.
func writeChunk(name string) {
	if err := os.WriteFile(name, []byte(chunkPayload(name)), 0o644); err != nil {
		fatalf(9, "write %s: %v", name, err)
	}
}

// post sends one transcoder callback; like the real transcoder it BLOCKS on
// the response (the gating proxy holds seglists until chunks are ACKed).
func post(url, body string) {
	resp, err := httpClient.Post(url, "text/plain", strings.NewReader(body))
	if err != nil {
		fatalf(9, "POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		fatalf(9, "POST %s: status %d", url, resp.StatusCode)
	}
}

// fetchMedia streams the signed masterd media URL like ffmpeg opening -i.
func fetchMedia(url string) {
	resp, err := httpClient.Get(url)
	if err != nil {
		fatalf(8, "GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || n == 0 {
		fatalf(8, "GET %s: status %d, %d bytes, err %v", url, resp.StatusCode, n, err)
	}
}

func argContaining(sub string) string {
	for _, a := range os.Args[1:] {
		if strings.Contains(a, sub) {
			return a
		}
	}
	return ""
}

func argAfter(flag string) string {
	args := os.Args[1:]
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
