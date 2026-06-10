package workerd

import (
	"bytes"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// maxAnnounceBody bounds a buffered seglist/manifest POST body (they are
// kilobytes in practice).
const maxAnnounceBody = 16 << 20

// proxyHandler is the worker-local gating proxy (cfg.proxy_listen,
// loopback, NO auth): the transcoder's PMS callbacks hit
// /relay/<job_id>/<pms-path> here and are forwarded to that job's masterd
// relay base. Announces (seglist/manifest) are HELD until every chunk they
// reference is ACKed -- the chunk-before-announce invariant; everything
// else (progress, ...) forwards immediately. Request and response travel
// verbatim so PMS's in-band throttling survives.
func (d *daemon) proxyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, rest, ok := splitRelayPath(r.URL.EscapedPath())
		if !ok {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "expected /relay/<job_id>/<pms-path>")
			return
		}
		j := d.getJob(id)
		if j == nil {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "unknown job "+id)
			return
		}
		if isAnnouncePath(r.URL.Path) {
			d.relayAnnounce(w, r, j, rest)
			return
		}
		d.relay(w, r, j, rest, r.Body, r.ContentLength)
	})
}

// splitRelayPath splits "/relay/<id>/<rest>"; rest stays escaped for
// verbatim forwarding.
func splitRelayPath(escapedPath string) (id, rest string, ok bool) {
	p, found := strings.CutPrefix(escapedPath, "/relay/")
	if !found {
		return "", "", false
	}
	id, rest, found = strings.Cut(p, "/")
	if !found || id == "" || rest == "" {
		return "", "", false
	}
	return id, rest, true
}

func isAnnouncePath(path string) bool {
	return strings.Contains(path, "seglist") || strings.Contains(path, "/manifest")
}

// chunkRefRE matches the chunk-file names a seglist (HLS CSV) or manifest
// (DASH MPD) can reference: media-NNNNN.ts/.vtt, chunk-*, init-*,
// sub-chunk-*. '$' and '%' are included so a DASH SegmentTemplate pattern
// matches as ONE token (then skipped by the gate's existence check) instead
// of leaking a partial, real-looking name.
var chunkRefRE = regexp.MustCompile(`(?:sub-chunk|chunk|init|media)-[A-Za-z0-9_$%.-]*[A-Za-z0-9]`)

// referencedChunks extracts the deduplicated chunk names referenced by an
// announce body, in order of first appearance.
func referencedChunks(body []byte) []string {
	matches := chunkRefRE.FindAll(body, -1)
	seen := make(map[string]struct{}, len(matches))
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		s := string(m)
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		names = append(names, s)
	}
	return names
}

// relayAnnounce buffers an announce, waits for the ACKs of every referenced
// chunk, then forwards it verbatim.
func (d *daemon) relayAnnounce(w http.ResponseWriter, r *http.Request, j *job, rest string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAnnounceBody))
	if err != nil {
		writeError(w, http.StatusBadGateway, "BAD_GATEWAY", "read announce body: "+err.Error())
		return
	}
	if err := j.gateWait(r.Context(), referencedChunks(body)); err != nil {
		writeError(w, http.StatusBadGateway, "GATE_ABORTED", err.Error())
		return
	}
	d.relay(w, r, j, rest, bytes.NewReader(body), int64(len(body)))
}

// hopHeaders are connection-scoped and never forwarded (RFC 9110 7.6.1).
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// relay forwards a request to the job's masterd relay base
// (req.master_pms) and copies the PMS response back verbatim.
func (d *daemon) relay(w http.ResponseWriter, r *http.Request, j *job, rest string, body io.Reader, length int64) {
	target := strings.TrimSuffix(j.req.MasterPMS, "/") + "/" + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "BAD_GATEWAY", err.Error())
		return
	}
	req.Header = r.Header.Clone()
	for _, h := range hopHeaders {
		req.Header.Del(h)
	}
	// The hop to masterd crosses the LAN: shared bearer token, replacing
	// anything the transcoder may have sent.
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.ContentLength = length

	resp, err := d.client.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "BAD_GATEWAY", err.Error())
		return
	}
	defer resp.Body.Close()
	out := w.Header()
	for k, vs := range resp.Header {
		if !isHopHeader(k) {
			out[k] = vs
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func isHopHeader(name string) bool {
	for _, h := range hopHeaders {
		if strings.EqualFold(name, h) {
			return true
		}
	}
	return false
}
