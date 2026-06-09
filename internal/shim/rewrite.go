package shim

import (
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/HritwikSinghal/plex-transcode-offload/internal/authtok"
	"github.com/HritwikSinghal/plex-transcode-offload/internal/protocol"
)

// PlaceholderPlexDir marks an argv path under the master's Plex install dir
// (the directory holding "Plex Transcoder", i.e. lib/plexmediaserver of
// wrappedPlexRaw). Mechanism for design class (c): the shim strips its own
// install-dir prefix (derived from argv[0]) and substitutes this placeholder;
// prt-workerd replaces it with its configured plex_dir -- the two nodes run
// byte-identical builds at DIFFERENT nix store paths, so a literal master
// path would never resolve on the worker. The constant lives here rather
// than in internal/protocol because protocol is frozen for this wave;
// workerd must substitute the same literal.
const PlaceholderPlexDir = "{{PLEXDIR}}"

// rewriter performs the argv rewrite of design section 3.7: the four path
// classes become placeholders the worker substitutes with local values, and
// every referenced master file becomes a signed masterd media URL.
type rewriter struct {
	sessionDir string // PMS session dir == shim cwd (abs, clean)
	plexDir    string // master plex install dir (abs, clean)
	advertise  string // masterd LAN base URL, no trailing slash
	token      string // shared bearer token == media HMAC secret
	expiry     time.Time
	media      map[string]protocol.MediaRef
	byPath     map[string]string // mode+path -> placeholder (dedupe)
}

func newRewriter(sessionDir, plexDir, advertiseURL, token string, expiry time.Time) *rewriter {
	return &rewriter{
		sessionDir: filepath.Clean(sessionDir),
		plexDir:    filepath.Clean(plexDir),
		advertise:  strings.TrimRight(advertiseURL, "/"),
		token:      token,
		expiry:     expiry,
		media:      map[string]protocol.MediaRef{},
		byPath:     map[string]string{},
	}
}

// rewriteArgs rewrites the full transcoder argv (argv[0] excluded),
// populating rw.media as a side effect.
func (rw *rewriter) rewriteArgs(args []string) []string {
	out := make([]string, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-i" && i+1 < len(args) {
			out[i] = a
			i++
			out[i] = rw.rewriteInput(args[i])
			continue
		}
		out[i] = rw.rewriteToken(a)
	}
	return out
}

// rewriteInput handles a value following -i: class (a) main media input
// (stream) or class (b) session-dir aux input (spool).
func (rw *rewriter) rewriteInput(v string) string {
	if strings.Contains(v, protocol.PMSURLPrefix) {
		return strings.ReplaceAll(v, protocol.PMSURLPrefix, protocol.PlaceholderPMS)
	}
	if strings.Contains(v, "://") {
		return v // non-PMS URL input: already network-reachable, pass through
	}
	abs := rw.abs(v)
	mode := protocol.MediaModeStream
	if rw.underSessionDir(abs) {
		// Extracted subs etc. in the session dir are small: spool them so
		// filters get a real local file on the worker.
		mode = protocol.MediaModeSpool
	}
	return rw.addMedia(abs, mode)
}

// rewriteToken handles every non-input argv token: class (d) PMS URLs,
// class (b) filter-string file refs, session-dir output paths ({{OUTDIR}}),
// and class (c) plex-install-dir paths.
func (rw *rewriter) rewriteToken(a string) string {
	a = strings.ReplaceAll(a, protocol.PMSURLPrefix, protocol.PlaceholderPMS)
	a = rw.rewriteFilterRefs(a)
	if strings.HasPrefix(a, rw.sessionDir+"/") {
		// Absolute output paths under the session dir; the worker remaps
		// them onto its local out dir (relative outputs need no rewrite:
		// the worker runs the transcoder with cwd = out dir).
		return protocol.PlaceholderOutDir + a[len(rw.sessionDir):]
	}
	if strings.HasPrefix(a, rw.plexDir+"/") {
		return PlaceholderPlexDir + a[len(rw.plexDir):]
	}
	return a
}

// rewriteFilterRefs substitutes file references embedded in filter strings
// (libass needs real local files): the subtitles= option list (positional
// filename, filename=/f=, fontsdir=) plus any standalone fontsdir= key.
func (rw *rewriter) rewriteFilterRefs(s string) string {
	if !strings.Contains(s, "subtitles=") && !strings.Contains(s, "fontsdir=") {
		return s
	}
	s = rw.rewriteSubtitlesOpts(s)
	s = rw.rewriteKeyValues(s, "fontsdir=")
	return s
}

// rewriteSubtitlesOpts rewrites the option list of every subtitles= filter
// occurrence: the positional first option (a bare path) and the values of
// the filename=, f= and fontsdir= options become spool placeholders; other
// options (si=, s=, ...) pass through.
func (rw *rewriter) rewriteSubtitlesOpts(s string) string {
	const key = "subtitles="
	var b strings.Builder
	for {
		idx := strings.Index(s, key)
		if idx < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:idx+len(key)])
		s = s[idx+len(key):]
		first := true
		for {
			val, rest := cutFilterValue(s)
			k, v, hasEq := strings.Cut(val, "=")
			switch {
			case hasEq && (k == "filename" || k == "f" || k == "fontsdir"):
				b.WriteString(k + "=")
				b.WriteString(rw.spoolRef(v))
			case !hasEq && first && val != "":
				b.WriteString(rw.spoolRef(val))
			default:
				b.WriteString(val)
			}
			first = false
			s = rest
			if !strings.HasPrefix(s, ":") {
				break // option list continues only across ':'
			}
			b.WriteString(":")
			s = s[1:]
		}
	}
}

// rewriteKeyValues substitutes the value after every occurrence of key with
// a spool placeholder. Values that are already placeholders (substituted by
// rewriteSubtitlesOpts) are left alone.
func (rw *rewriter) rewriteKeyValues(s, key string) string {
	var b strings.Builder
	for {
		idx := strings.Index(s, key)
		if idx < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:idx+len(key)])
		s = s[idx+len(key):]
		val, rest := cutFilterValue(s)
		// "{{MEDIA" without the colon: cutFilterValue stops at the ':'
		// inside an already-substituted placeholder.
		if val == "" || strings.HasPrefix(val, "{{MEDIA") {
			b.WriteString(val)
		} else {
			b.WriteString(rw.spoolRef(val))
		}
		s = rest
	}
}

// cutFilterValue splits a filter-option value off s: the value ends at the
// first unescaped filter metacharacter (option/filter/graph separators and
// link-label brackets). Backslash escapes one character (libass paths embed
// "\:" for colons).
func cutFilterValue(s string) (val, rest string) {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // skip the escaped character
		case ':', ',', ';', '[', ']':
			return s[:i], s[i:]
		}
	}
	return s, ""
}

// unescapeFilter removes filter-string backslash escapes, recovering the
// real filesystem path.
func unescapeFilter(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// spoolRef turns a filter-embedded file reference into a spool placeholder.
func (rw *rewriter) spoolRef(v string) string {
	return rw.addMedia(rw.abs(unescapeFilter(v)), protocol.MediaModeSpool)
}

// addMedia registers abs as media input (deduplicated per mode+path) and
// returns its argv placeholder.
func (rw *rewriter) addMedia(abs string, mode protocol.MediaMode) string {
	key := string(mode) + "\x00" + abs
	if ph, ok := rw.byPath[key]; ok {
		return ph
	}
	n := len(rw.media)
	ph := protocol.MediaPlaceholder(n)
	rw.media[strconv.Itoa(n)] = protocol.MediaRef{
		URL:  rw.signedMediaURL(abs),
		Mode: mode,
	}
	rw.byPath[key] = ph
	return ph
}

// signedMediaURL builds the masterd file-server URL for abs. Only the
// path+query is signed -- masterd verifies against r.URL.Path, which never
// includes scheme/host -- and the advertise base is prepended afterwards.
func (rw *rewriter) signedMediaURL(abs string) string {
	signed := authtok.SignURL(rw.token, "/v1/media?path="+url.QueryEscape(abs), rw.expiry)
	return rw.advertise + signed
}

// abs resolves p against the session dir (the shim cwd) when relative.
func (rw *rewriter) abs(p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(rw.sessionDir, p)
	}
	return filepath.Clean(p)
}

func (rw *rewriter) underSessionDir(abs string) bool {
	return abs == rw.sessionDir || strings.HasPrefix(abs, rw.sessionDir+"/")
}
