package protocol

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestJobRequestRoundTrip(t *testing.T) {
	req := JobRequest{
		JobID:     "9b2f6e1c",
		PlexBuild: "1.43.0.10000-abcdef123",
		Argv: []string{
			"-i", MediaPlaceholder(0),
			"-progressurl", PlaceholderPMS + "/video/:/transcode/session/x/progress",
			PlaceholderOutDir + "/media-%05d.ts",
		},
		Media: map[string]MediaRef{
			"0": {URL: "http://10.0.50.1:32499/v1/media?path=%2Fmnt%2Fm.mkv&exp=1&sig=ab", Mode: MediaModeStream},
		},
		Env: map[string]string{"X_PLEX_TOKEN": "tok"},
		Session: SessionInfo{
			ID:        "pms-session-1",
			PushURL:   "http://10.0.50.1:32499/v1/sessions/9b2f6e1c",
			PushToken: "push-tok",
		},
		MasterPMS: "http://10.0.50.1:32499/relay/9b2f6e1c",
	}

	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got JobRequest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(req, got) {
		t.Errorf("round trip mismatch:\n in: %+v\nout: %+v", req, got)
	}

	// Wire field names are the contract; spot-check the json tags.
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal map: %v", err)
	}
	for _, key := range []string{"job_id", "plex_build", "argv", "media", "env", "session", "master_pms"} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("wire key %q missing in %s", key, raw)
		}
	}
	sess, ok := asMap["session"].(map[string]any)
	if !ok {
		t.Fatalf("session not an object")
	}
	for _, key := range []string{"id", "push_url", "push_token"} {
		if _, ok := sess[key]; !ok {
			t.Errorf("session wire key %q missing", key)
		}
	}
}

func TestRegisterSessionResponseRoundTrip(t *testing.T) {
	resp := RegisterSessionResponse{
		PushToken:    "push-tok",
		AdvertiseURL: "http://10.0.50.138:32499",
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got RegisterSessionResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(resp, got) {
		t.Errorf("round trip mismatch:\n in: %+v\nout: %+v", resp, got)
	}

	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal map: %v", err)
	}
	for _, key := range []string{"push_token", "advertise_url"} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("wire key %q missing in %s", key, raw)
		}
	}
}

func TestWorkersResponseRoundTrip(t *testing.T) {
	resp := WorkersResponse{
		Workers: []WorkerStatus{
			{
				URL:     "http://10.0.50.52:32500",
				Healthy: true,
				Health: Health{
					Status:      HealthOK,
					PlexBuild:   "1.43.0.10000-abcdef123",
					ActiveJobs:  1,
					MaxJobs:     3,
					VaapiOK:     true,
					CodecsReady: map[string]bool{"1.43.0.10000-abcdef123": true},
				},
			},
			{URL: "http://10.0.50.53:32500", Healthy: false},
		},
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got WorkersResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(resp, got) {
		t.Errorf("round trip mismatch:\n in: %+v\nout: %+v", resp, got)
	}

	// Wire field names are the contract; spot-check the json tags.
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal map: %v", err)
	}
	workers, ok := asMap["workers"].([]any)
	if !ok || len(workers) != 2 {
		t.Fatalf("workers key missing or wrong shape in %s", raw)
	}
	first, ok := workers[0].(map[string]any)
	if !ok {
		t.Fatalf("workers[0] not an object")
	}
	for _, key := range []string{"url", "healthy", "health"} {
		if _, ok := first[key]; !ok {
			t.Errorf("worker wire key %q missing in %s", key, raw)
		}
	}
}

func TestEventRoundTrip(t *testing.T) {
	code := 75
	cases := []struct {
		name string
		ev   Event
		want string
	}{
		{"state", Event{Type: EventState, State: JobRunning}, `{"type":"state","state":"RUNNING"}`},
		{"stderr", Event{Type: EventStderr, Line: "frame=1"}, `{"type":"stderr","line":"frame=1"}`},
		{"exit", Event{Type: EventExit, Code: &code}, `{"type":"exit","code":75}`},
		{"heartbeat", Event{Type: EventHeartbeat}, `{"type":"heartbeat"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.ev)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(raw) != tc.want {
				t.Errorf("wire form = %s, want %s", raw, tc.want)
			}
			var got Event
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual(tc.ev, got) {
				t.Errorf("round trip mismatch: in %+v out %+v", tc.ev, got)
			}
		})
	}
}

func TestExitCodeZeroIsEncoded(t *testing.T) {
	// code=0 (run-to-completion jobs) must survive: pointer, not omitempty int.
	zero := 0
	raw, err := json.Marshal(Event{Type: EventExit, Code: &zero})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `{"type":"exit","code":0}` {
		t.Errorf("exit 0 wire form = %s", raw)
	}
}

func TestMediaPlaceholder(t *testing.T) {
	if got := MediaPlaceholder(0); got != "{{MEDIA:0}}" {
		t.Errorf("MediaPlaceholder(0) = %q", got)
	}
	if got := MediaPlaceholder(17); got != "{{MEDIA:17}}" {
		t.Errorf("MediaPlaceholder(17) = %q", got)
	}
}

func TestParseMediaPlaceholder(t *testing.T) {
	cases := []struct {
		in string
		n  int
		ok bool
	}{
		{"{{MEDIA:0}}", 0, true},
		{"{{MEDIA:42}}", 42, true},
		{"{{MEDIA:}}", 0, false},
		{"{{MEDIA:-1}}", 0, false},
		{"{{MEDIA:1x}}", 0, false},
		{"{{MEDIA: 1}}", 0, false},
		{"{{MEDIA:1}}/suffix", 0, false},
		{"prefix{{MEDIA:1}}", 0, false},
		{"{{OUTDIR}}", 0, false},
		{"", 0, false},
		{"{{MEDIA:99999999999999999999}}", 0, false}, // overflow
	}
	for _, tc := range cases {
		n, ok := ParseMediaPlaceholder(tc.in)
		if n != tc.n || ok != tc.ok {
			t.Errorf("ParseMediaPlaceholder(%q) = (%d, %v), want (%d, %v)", tc.in, n, ok, tc.n, tc.ok)
		}
	}
}

func TestPlaceholderRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 10, 12345} {
		got, ok := ParseMediaPlaceholder(MediaPlaceholder(n))
		if !ok || got != n {
			t.Errorf("round trip %d -> (%d, %v)", n, got, ok)
		}
	}
}
