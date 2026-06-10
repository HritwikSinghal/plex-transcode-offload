package masterd

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/HritwikSinghal/plex-transcode-offload/internal/protocol"
)

// newRelayProxy builds the reverse proxy behind /relay/{id}/{rest...}: the
// {id} segment is stripped and {rest} plus the original query is forwarded
// verbatim to the local PMS, streaming bodies both ways and copying status
// and headers back (seglist/manifest/progress callbacks; in-band throttling
// rides on the PMS response, so nothing may be buffered or rewritten).
func newRelayProxy(pms *url.URL, logger *log.Logger) http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = pms.Scheme
			pr.Out.URL.Host = pms.Host
			pr.Out.URL.Path = "/" + pr.In.PathValue("rest")
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = pms.Host
			// Our bearer token is masterd auth, not PMS auth: do not leak it.
			pr.Out.Header.Del("Authorization")
		},
		// Flush immediately: some PMS responses are long-polled and the
		// transcoder blocks on them (throttling).
		FlushInterval: -1,
		ErrorLog:      logger,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Printf("relay %s %s: %v", r.Method, r.URL.Path, err)
			writeJSON(w, http.StatusBadGateway, protocol.ErrorBody{
				Error:   "BAD_GATEWAY",
				Message: "PMS relay failed: " + err.Error(),
			})
		},
	}
}

// handleRelay serves GET and POST /relay/{id}/{rest...} (shared bearer
// auth; the path values are read by the proxy's Rewrite).
func (s *server) handleRelay(w http.ResponseWriter, r *http.Request) {
	s.relay.ServeHTTP(w, r)
}
