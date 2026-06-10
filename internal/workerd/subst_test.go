package workerd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSubstituteArgv(t *testing.T) {
	media := map[string]string{
		"{{MEDIA:0}}": "http://master:32499/v1/media?path=%2Fmnt%2Fa.mkv&sig=s",
		"{{MEDIA:1}}": "/var/lib/prt/jobs/j1/spool/m1.srt",
	}
	in := []string{
		"-i", "{{MEDIA:0}}",
		"-filter_complex", "[0:2]subtitles=f={{MEDIA:1}}",
		"-progressurl", "{{PMS}}/video/:/transcode/session/abc/progress",
		"-segment_list", "{{PMS}}/video/:/transcode/session/abc/u1/seglist",
		"{{OUTDIR}}/media-%05d.ts",
	}
	got, err := substituteArgv(in, "/var/lib/prt/jobs/j1/out", "http://127.0.0.1:32401/relay/j1", media)
	if err != nil {
		t.Fatalf("substituteArgv: %v", err)
	}
	want := []string{
		"-i", "http://master:32499/v1/media?path=%2Fmnt%2Fa.mkv&sig=s",
		"-filter_complex", "[0:2]subtitles=f=/var/lib/prt/jobs/j1/spool/m1.srt",
		"-progressurl", "http://127.0.0.1:32401/relay/j1/video/:/transcode/session/abc/progress",
		"-segment_list", "http://127.0.0.1:32401/relay/j1/video/:/transcode/session/abc/u1/seglist",
		"/var/lib/prt/jobs/j1/out/media-%05d.ts",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("substituteArgv:\n got %q\nwant %q", got, want)
	}
}

func TestSubstituteArgvUnresolvedMedia(t *testing.T) {
	_, err := substituteArgv([]string{"-i", "{{MEDIA:3}}"}, "/out", "http://p", nil)
	if err == nil {
		t.Fatal("expected error for unresolved media placeholder")
	}
}

func TestBuildEnvOverrides(t *testing.T) {
	reqEnv := map[string]string{
		"X_PLEX_TOKEN":         "tok",
		"FFMPEG_EXTERNAL_LIBS": "/master/Library/Codecs/abc-linux-x86_64/",
		"EAE_ROOT":             "/master/eae",
		"HOME":                 "/master/home",
		"LD_LIBRARY_PATH":      "/master/plex/lib",
		"LIBVA_DRIVERS_PATH":   "/master/drivers/dri",
		"LIBVA_DRIVER_NAME":    "radeonsi",
	}
	got := buildEnv(reqEnv, "/data/jobs/j1", "/data/codecs/build1", "/run/prt-eae/shared", "/plex/lib", "")
	want := []string{
		"EAE_ROOT=/run/prt-eae/shared",
		"FFMPEG_EXTERNAL_LIBS=/data/codecs/build1/",
		"HOME=/data/jobs/j1",
		"LD_LIBRARY_PATH=/plex/lib",
		"X_PLEX_TOKEN=tok",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildEnv (software):\n got %q\nwant %q", got, want)
	}

	got = buildEnv(reqEnv, "/data/jobs/j1", "/data/codecs/build1", "/run/prt-eae/shared", "/plex/lib", "/data/drivers/bundle/dri")
	want = []string{
		"EAE_ROOT=/run/prt-eae/shared",
		"FFMPEG_EXTERNAL_LIBS=/data/codecs/build1/",
		"HOME=/data/jobs/j1",
		"LD_LIBRARY_PATH=/plex/lib",
		"LIBVA_DRIVERS_PATH=/data/drivers/bundle/dri",
		"LIBVA_DRIVER_NAME=iHD",
		"X_PLEX_TOKEN=tok",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildEnv (hw):\n got %q\nwant %q", got, want)
	}
}

func TestPlexBuildFromDir(t *testing.T) {
	// Store-path parse (the dir need not exist).
	got, err := plexBuildFromDir("/nix/store/abc123-plexmediaserver-1.43.0.10166-deadbeef/lib/plexmediaserver")
	if err != nil {
		t.Fatalf("plexBuildFromDir: %v", err)
	}
	if want := "1.43.0.10166-deadbeef"; got != want {
		t.Errorf("plexBuildFromDir = %q, want %q", got, want)
	}

	// VERSION file wins.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("9.9.9.9-cafe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = plexBuildFromDir(dir)
	if err != nil {
		t.Fatalf("plexBuildFromDir(VERSION): %v", err)
	}
	if want := "9.9.9.9-cafe"; got != want {
		t.Errorf("plexBuildFromDir(VERSION) = %q, want %q", got, want)
	}

	if _, err := plexBuildFromDir("/opt/somewhere/else"); err == nil {
		t.Error("expected error for underivable plex dir")
	}
}

func TestBuildsMatch(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.43.0.10166-deadbeef", "1.43.0.10166-deadbeef", true},
		{"1.43.0.10166-deadbeef", "1.43.0.10166-cafef00d", true}, // suffix-lenient
		{"1.43.0.10166-deadbeef", "1.43.1.10200-deadbeef", false},
		{"1.43.0.10166", "1.43.0.10166-deadbeef", true},
		{"", "1.43.0.10166", false},
	}
	for _, c := range cases {
		if got := buildsMatch(c.a, c.b); got != c.want {
			t.Errorf("buildsMatch(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
