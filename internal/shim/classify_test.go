package shim

import "testing"

func TestClassifyLocal(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		local bool
	}{
		{
			name: "segmented hls transcode goes remote",
			args: []string{
				"-codec:0", "h264", "-i", "/mnt/media/Movies/A.mkv",
				"-segment_format", "mpegts",
				"-manifest_name", "http://127.0.0.1:32400/video/:/transcode/session/abc/0/seglist",
				"media-%05d.ts",
			},
			local: false,
		},
		{
			name: "dash with manifest url goes remote",
			args: []string{
				"-i", "/mnt/media/Movies/A.mkv",
				"-manifest_name", "http://127.0.0.1:32400/video/:/transcode/session/abc/0/manifest",
				"chunk-stream0-%05d.m4s",
			},
			local: false,
		},
		{
			name:  "loudness analysis stays local",
			args:  []string{"-i", "/mnt/media/Music/a.flac", "-filter_complex", "[0:0]loudness=...", "media-%05d.ts"},
			local: true,
		},
		{
			name:  "single-file output stays local",
			args:  []string{"-i", "/mnt/media/Music/a.flac", "-codec:0", "aac", "out.aac"},
			local: true,
		},
		{
			name: "live tv stays local",
			args: []string{
				"-i", "http://127.0.0.1:32400/livetv/sessions/xyz/0/index.m3u8",
				"media-%05d.ts",
			},
			local: true,
		},
		{
			name:  "pipe input stays local",
			args:  []string{"-i", "pipe:0", "media-%05d.ts"},
			local: true,
		},
		{
			name:  "no input stays local",
			args:  []string{"-version"},
			local: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyLocal(tc.args); got != tc.local {
				t.Errorf("classifyLocal() = %v, want %v", got, tc.local)
			}
		})
	}
}
