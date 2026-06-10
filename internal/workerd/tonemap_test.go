package workerd

import "testing"

func TestRewriteTonemapChain(t *testing.T) {
	// The graph observed live on a dispatched 4K HDR -> 1080p SDR session.
	observed := "[0:0]scale=w=1920:h=1080:force_divisible_by=4[0];" +
		"[0]format=p010,tonemap=hable[1];[1]format=pix_fmts=nv12[2];[2]hwupload[3]"

	cases := []struct {
		name    string
		in      string
		want    string
		rewrote bool
	}{
		{
			name:    "observed graph",
			in:      observed,
			want:    "[0:0]hwupload,scale_vaapi=w=1920:h=1080,tonemap_vaapi=format=nv12[3]",
			rewrote: true,
		},
		{
			name: "0:v:0 input, no force_divisible_by, other curve",
			in: "[0:v:0]scale=w=1280:h=720[a];[a]format=p010,tonemap=mobius[b];" +
				"[b]format=pix_fmts=nv12[c];[c]hwupload[out]",
			want:    "[0:v:0]hwupload,scale_vaapi=w=1280:h=720,tonemap_vaapi=format=nv12[out]",
			rewrote: true,
		},
		{
			name: "subtitle overlay chain",
			in: observed +
				";[3]hwdownload[4];[4][0:2]overlay[5]",
		},
		{
			name: "expression width",
			in: "[0:0]scale=w=-2:h=1080[0];[0]format=p010,tonemap=hable[1];" +
				"[1]format=pix_fmts=nv12[2];[2]hwupload[3]",
		},
		{
			name: "missing tonemap",
			in: "[0:0]scale=w=1920:h=1080[0];[0]format=p010[1];" +
				"[1]format=pix_fmts=nv12[2];[2]hwupload[3]",
		},
		{
			name: "multiple inputs",
			in: "[0:0]scale=w=1920:h=1080[0];[1:0]format=p010,tonemap=hable[1];" +
				"[1]format=pix_fmts=nv12[2];[2]hwupload[3]",
		},
		{
			name: "already vaapi",
			in:   "[0:0]hwupload,scale_vaapi=w=1920:h=1080,tonemap_vaapi=format=nv12[3]",
		},
		{
			name: "tonemap with extra options",
			in: "[0:0]scale=w=1920:h=1080[0];[0]format=p010,tonemap=hable:desat=0[1];" +
				"[1]format=pix_fmts=nv12[2];[2]hwupload[3]",
		},
		{
			name: "scale with extra option",
			in: "[0:0]scale=w=1920:h=1080:flags=lanczos[0];[0]format=p010,tonemap=hable[1];" +
				"[1]format=pix_fmts=nv12[2];[2]hwupload[3]",
		},
		{
			name: "named pad input label",
			in: "[vin]scale=w=1920:h=1080[0];[0]format=p010,tonemap=hable[1];" +
				"[1]format=pix_fmts=nv12[2];[2]hwupload[3]",
		},
		{
			name: "broken linkage between chains",
			in: "[0:0]scale=w=1920:h=1080[0];[x]format=p010,tonemap=hable[1];" +
				"[1]format=pix_fmts=nv12[2];[2]hwupload[3]",
		},
		{
			name: "trailing filter after hwupload",
			in:   observed + ";[3]format=vaapi[4]",
		},
		{name: "empty", in: ""},
		{name: "not a labeled graph", in: "scale=w=1920:h=1080"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, rewrote := rewriteTonemapChain(c.in)
			if rewrote != c.rewrote {
				t.Fatalf("rewrote = %v, want %v (got %q)", rewrote, c.rewrote, got)
			}
			want := c.want
			if !c.rewrote {
				want = c.in // pass-through must be byte-identical
			}
			if got != want {
				t.Errorf("rewriteTonemapChain:\n got %q\nwant %q", got, want)
			}
		})
	}
}
