package shim

import (
	"regexp"
	"strings"
)

// segmentTemplateRe matches the printf-style segment numbering template that
// every segmented transcode carries in its output argv (media-%05d.ts,
// chunk-stream0-%05d.m4s, ...).
var segmentTemplateRe = regexp.MustCompile(`%0\d+d`)

// classifyLocal decides whether a job must run locally (design section 6).
// The bias is deliberately conservative: anything that is not clearly a
// segmented video transcode goes local -- dispatch overhead would exceed the
// work for photo/audio jobs, and live/loudness shapes cannot be offloaded.
//
// LOCAL when any of:
//   - any arg contains "loudness" (loudness analysis jobs);
//   - live TV / DVR shapes (livetv session URLs, /dvr paths, realtime -re);
//   - a pipe input ("-" or pipe:) -- not reproducible on the worker;
//   - no -i input at all;
//   - no segmented-output pattern: neither a %0Nd segment template nor a
//     seglist/manifest callback URL appears in the argv.
func classifyLocal(args []string) bool {
	hasTemplate := false
	hasSegURL := false
	hasInput := false
	for i, a := range args {
		la := strings.ToLower(a)
		if strings.Contains(la, "loudness") {
			return true
		}
		if strings.Contains(la, "livetv") || strings.Contains(la, "/dvr") || a == "-re" {
			return true
		}
		if a == "-i" {
			if i+1 >= len(args) {
				return true // malformed argv: not ours to second-guess
			}
			v := args[i+1]
			if v == "-" || strings.HasPrefix(v, "pipe:") {
				return true
			}
			hasInput = true
		}
		if segmentTemplateRe.MatchString(a) {
			hasTemplate = true
		}
		if strings.Contains(la, "/seglist") || strings.Contains(la, "/manifest") {
			hasSegURL = true
		}
	}
	if !hasInput {
		return true
	}
	return !hasTemplate && !hasSegURL
}
