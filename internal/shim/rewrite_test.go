package shim

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/HritwikSinghal/plex-transcode-offload/internal/protocol"
)

func TestRewriteArgsAllClasses(t *testing.T) {
	const (
		sessionDir = "/transcode/Sessions/s-abc"
		plexDir    = "/nix/store/aaa-plexmediaserver-1.2.3/lib/plexmediaserver"
		advertise  = "http://10.0.50.138:32499"
	)
	rw := newRewriter(sessionDir, plexDir, advertise, "secret-token",
		time.Now().Add(time.Hour))

	args := []string{
		"-codec:0", "h264",
		"-i", "/mnt/media/Movies/A.mkv",
		"-filter_complex",
		"[0:2]subtitles=f=/mnt/media/Movies/A.en.ass:fontsdir=/tmp/fonts:si=0[sub]",
		"-i", "sub.srt",
		"-progressurl", "http://127.0.0.1:32400/video/:/transcode/session/s-abc/0/progress",
		sessionDir + "/media-%05d.ts",
		plexDir + "/Resources/x.txt",
	}
	got := rw.rewriteArgs(args)

	want := []string{
		"-codec:0", "h264",
		"-i", "{{MEDIA:0}}",
		"-filter_complex",
		"[0:2]subtitles=f={{MEDIA:1}}:fontsdir={{MEDIA:2}}:si=0[sub]",
		"-i", "{{MEDIA:3}}",
		"-progressurl", "{{PMS}}/video/:/transcode/session/s-abc/0/progress",
		"{{OUTDIR}}/media-%05d.ts",
		"{{PLEXDIR}}/Resources/x.txt",
	}
	if len(got) != len(want) {
		t.Fatalf("rewriteArgs() returned %d args, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	wantMedia := map[string]struct {
		path string
		mode protocol.MediaMode
	}{
		"0": {"/mnt/media/Movies/A.mkv", protocol.MediaModeStream},
		"1": {"/mnt/media/Movies/A.en.ass", protocol.MediaModeSpool},
		"2": {"/tmp/fonts", protocol.MediaModeSpool},
		"3": {sessionDir + "/sub.srt", protocol.MediaModeSpool}, // relative input resolved against the session dir
	}
	if len(rw.media) != len(wantMedia) {
		t.Fatalf("media map has %d entries, want %d: %v", len(rw.media), len(wantMedia), rw.media)
	}
	for k, w := range wantMedia {
		ref, ok := rw.media[k]
		if !ok {
			t.Errorf("media[%q] missing", k)
			continue
		}
		if ref.Mode != w.mode {
			t.Errorf("media[%q].Mode = %q, want %q", k, ref.Mode, w.mode)
		}
		if !strings.HasPrefix(ref.URL, advertise+"/v1/media?") {
			t.Errorf("media[%q].URL = %q, want prefix %q", k, ref.URL, advertise+"/v1/media?")
			continue
		}
		u, err := url.Parse(ref.URL)
		if err != nil {
			t.Errorf("media[%q].URL parse: %v", k, err)
			continue
		}
		q := u.Query()
		if q.Get("path") != w.path {
			t.Errorf("media[%q] path = %q, want %q", k, q.Get("path"), w.path)
		}
		if q.Get("sig") == "" || q.Get("exp") == "" {
			t.Errorf("media[%q].URL lacks sig/exp: %q", k, ref.URL)
		}
	}
}
