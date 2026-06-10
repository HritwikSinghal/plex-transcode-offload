package workerd

import (
	"strconv"
	"strings"
)

// PMS composes the transcoder argv on the MASTER, whose GPU (AMD) drives the
// filter choice: a 4K HDR/DV -> SDR session arrives with a SOFTWARE
// scale+tonemap chain that crawls at ~0.2x realtime on the worker's CPU. The
// worker's GPU (Intel, iHD) can run the whole chain natively, so when the
// dispatched graph has exactly the known software shape it is swapped for
// the VAAPI equivalent. tonemap_vaapi applies the driver's fixed HDR10->SDR
// mapping -- the master's curve choice (hable, ...) is not preserved, an
// acceptable trade-off against the CPU path.

// rewriteTonemapChain rewrites the known software tonemap filtergraph
//
//	[0:0]scale=w=W:h=H:force_divisible_by=N[a];[a]format=p010,tonemap=CURVE[b];
//	[b]format=pix_fmts=nv12[c];[c]hwupload[OUT]
//
// into its VAAPI-native form, preserving the input and final output labels:
//
//	[0:0]hwupload,scale_vaapi=w=W:h=H,tonemap_vaapi=format=nv12[OUT]
//
// (hwupload moves first: the session has no -hwaccel_output_format vaapi, so
// decoded frames land in system memory and the GPU chain must start by
// uploading them. force_divisible_by is dropped -- scale_vaapi does not
// support it, and the explicit W/H make it redundant.)
//
// Matching is deliberately conservative: a single linear pipeline from one
// video-input label, exactly the five filters above, numeric w/h, a bare
// tonemap curve. Anything else -- overlay/subtitle burn-in, extra chains,
// expression dimensions (w=-2), already-VAAPI graphs -- passes through
// unchanged (false).
func rewriteTonemapChain(filterComplex string) (string, bool) {
	parts := strings.Split(filterComplex, ";")
	chains := make([]fgChain, 0, len(parts))
	for _, s := range parts {
		c, ok := parseFGChain(s)
		if !ok {
			return filterComplex, false
		}
		chains = append(chains, c)
	}
	if !isVideoInputLabel(chains[0].in) {
		return filterComplex, false
	}
	// Linear pipeline only: each chain consumes exactly the previous
	// chain's output, so flattening preserves filter order and no
	// intermediate label is referenced elsewhere.
	var filters []string
	for i, c := range chains {
		if i > 0 && c.in != chains[i-1].out {
			return filterComplex, false
		}
		filters = append(filters, c.filters...)
	}
	if len(filters) != 5 {
		return filterComplex, false
	}
	w, h, ok := scaleDims(filters[0])
	if !ok || filters[1] != "format=p010" {
		return filterComplex, false
	}
	curve, isTonemap := strings.CutPrefix(filters[2], "tonemap=")
	// A curve with extra options (tonemap=hable:desat=0) is not the known
	// shape; refuse rather than guess.
	if !isTonemap || curve == "" || strings.ContainsAny(curve, ":=") {
		return filterComplex, false
	}
	if filters[3] != "format=pix_fmts=nv12" || filters[4] != "hwupload" {
		return filterComplex, false
	}
	in, out := chains[0].in, chains[len(chains)-1].out
	return "[" + in + "]hwupload,scale_vaapi=w=" + w + ":h=" + h +
		",tonemap_vaapi=format=nv12[" + out + "]", true
}

// fgChain is one ';'-separated filtergraph chain with exactly one input and
// one output label.
type fgChain struct {
	in, out string
	filters []string
}

// parseFGChain splits "[in]f1,f2[out]" into its parts. Chains with multiple
// input/output labels (overlay, split) or bracketed filter arguments leave
// brackets in the body and are refused.
func parseFGChain(s string) (fgChain, bool) {
	var c fgChain
	if !strings.HasPrefix(s, "[") {
		return c, false
	}
	end := strings.IndexByte(s, ']')
	if end < 0 {
		return c, false
	}
	c.in = s[1:end]
	rest := s[end+1:]
	if !strings.HasSuffix(rest, "]") {
		return c, false
	}
	open := strings.LastIndexByte(rest, '[')
	if open < 0 {
		return c, false
	}
	c.out = rest[open+1 : len(rest)-1]
	body := rest[:open]
	if c.in == "" || c.out == "" || body == "" || strings.ContainsAny(body, "[]") {
		return c, false
	}
	c.filters = strings.Split(body, ",")
	return c, true
}

// isVideoInputLabel matches a stream-specifier input label of a single video
// input: "0:0", "0:v" or "0:v:0". Named pad labels (intermediate links,
// multi-input graphs) do not match.
func isVideoInputLabel(label string) bool {
	parts := strings.Split(label, ":")
	switch len(parts) {
	case 2:
		return isUint(parts[0]) && (parts[1] == "v" || isUint(parts[1]))
	case 3:
		return isUint(parts[0]) && parts[1] == "v" && isUint(parts[2])
	}
	return false
}

func isUint(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// scaleDims extracts explicit numeric w/h from a software scale filter.
// Only w, h and force_divisible_by (dropped by the rewrite) are accepted;
// any other option or a non-numeric dimension (w=-2, iw/2) refuses the
// rewrite.
func scaleDims(filter string) (w, h string, ok bool) {
	args, isScale := strings.CutPrefix(filter, "scale=")
	if !isScale {
		return "", "", false
	}
	for _, kv := range strings.Split(args, ":") {
		k, v, found := strings.Cut(kv, "=")
		if !found {
			return "", "", false
		}
		switch k {
		case "w":
			w = v
		case "h":
			h = v
		case "force_divisible_by":
			// accepted but dropped; see rewriteTonemapChain.
		default:
			return "", "", false
		}
	}
	if !isPositiveInt(w) || !isPositiveInt(h) {
		return "", "", false
	}
	return w, h, true
}

func isPositiveInt(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0
}
