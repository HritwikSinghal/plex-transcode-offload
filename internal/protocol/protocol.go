// Package protocol defines the wire types and shared constants of the PRT v2
// HTTP protocol spoken between prt-shim, prt-masterd and prt-workerd.
//
// This package is the single source of truth for the contract; the three role
// implementations must not redefine any of it.
package protocol

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Well-known ports (defaults; the daemons' listen addresses are configurable).
const (
	// PortMasterd is the prt-masterd listen port on the master.
	PortMasterd = 32499
	// PortWorkerd is the prt-workerd listen port on the worker.
	PortWorkerd = 32500
	// PortWorkerProxy is the worker-local gating proxy that intercepts the
	// transcoder's PMS callbacks (seglist/manifest/progress). Loopback only.
	PortWorkerProxy = 32401
)

// argv placeholder constants. The shim substitutes these INTO the dispatched
// argv; the worker substitutes them OUT to worker-local values.
const (
	// PlaceholderOutDir marks the transcoder output directory. The worker
	// replaces it with its local job out dir (tmpfs).
	PlaceholderOutDir = "{{OUTDIR}}"
	// PlaceholderPMS replaces the literal URL prefix PMSURLPrefix in any argv
	// token. The worker rewrites it to its local gating proxy.
	PlaceholderPMS = "{{PMS}}"
	// PMSURLPrefix is the literal master-local PMS base URL that
	// PlaceholderPMS stands in for.
	PMSURLPrefix = "http://127.0.0.1:32400"
)

// ExitWorkerLost is the shim exit code for "worker lost mid-job": the events
// lifeline died without a terminal event. PMS restarts the session; the next
// spawn health-checks and falls back local.
const ExitWorkerLost = 75

// NDJSON stream liveness. Both ends of an events stream write a heartbeat
// every HeartbeatInterval; an end that has seen no traffic for PeerTimeout
// must treat the peer as lost.
const (
	HeartbeatInterval = 5 * time.Second
	PeerTimeout       = 10 * time.Second
)

// mediaPlaceholderPrefix and -Suffix delimit MediaPlaceholder tokens.
const (
	mediaPlaceholderPrefix = "{{MEDIA:"
	mediaPlaceholderSuffix = "}}"
)

// MediaPlaceholder returns the argv placeholder for media input n,
// e.g. MediaPlaceholder(0) == "{{MEDIA:0}}". The decimal index keys the
// JobRequest.Media map.
func MediaPlaceholder(n int) string {
	return fmt.Sprintf("%s%d%s", mediaPlaceholderPrefix, n, mediaPlaceholderSuffix)
}

// ParseMediaPlaceholder reports whether s is exactly a media placeholder and,
// if so, its index. Only canonical forms produced by MediaPlaceholder match
// (non-negative decimal index, no sign, no spaces).
func ParseMediaPlaceholder(s string) (int, bool) {
	inner, ok := strings.CutPrefix(s, mediaPlaceholderPrefix)
	if !ok {
		return 0, false
	}
	inner, ok = strings.CutSuffix(inner, mediaPlaceholderSuffix)
	if !ok {
		return 0, false
	}
	if inner == "" || strings.TrimLeft(inner, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(inner)
	if err != nil { // overflow
		return 0, false
	}
	return n, true
}

// MediaMode selects how the worker materializes a media reference.
type MediaMode string

const (
	// MediaModeStream substitutes the masterd URL directly; ffmpeg streams
	// and Range-seeks it. Used for the (large) main input.
	MediaModeStream MediaMode = "stream"
	// MediaModeSpool downloads the file to the job spool dir during
	// PREPARING and substitutes the local path. Used for small aux files
	// (subtitles, fonts) referenced inside filter strings.
	MediaModeSpool MediaMode = "spool"
)

// MediaRef is one media input of a job: a signed masterd URL plus the mode.
type MediaRef struct {
	URL  string    `json:"url"`
	Mode MediaMode `json:"mode"`
}

// SessionInfo identifies the masterd segment-push session of a job.
type SessionInfo struct {
	ID        string `json:"id"`         // PMS session id
	PushURL   string `json:"push_url"`   // http://<master>:32499/v1/sessions/<job_id>
	PushToken string `json:"push_token"` // minted per session by masterd
}

// JobRequest is the POST /v1/jobs payload: the full, self-contained context
// of one transcode job (workers are stateless).
type JobRequest struct {
	JobID     string              `json:"job_id"`
	PlexBuild string              `json:"plex_build"`
	Argv      []string            `json:"argv"`  // with placeholders substituted in
	Media     map[string]MediaRef `json:"media"` // key = decimal index matching {{MEDIA:n}}
	Env       map[string]string   `json:"env"`
	Session   SessionInfo         `json:"session"`
	MasterPMS string              `json:"master_pms"` // masterd relay base: http://<master>:32499/relay/<job_id>
}

// JobState is a worker-side job lifecycle state.
type JobState string

const (
	JobPending   JobState = "PENDING"
	JobPreparing JobState = "PREPARING"
	JobRunning   JobState = "RUNNING"
	JobDraining  JobState = "DRAINING"
	JobCompleted JobState = "COMPLETED"
	JobCancelled JobState = "CANCELLED"
	JobFailed    JobState = "FAILED"
	JobLost      JobState = "LOST"
)

// Event types carried on the NDJSON events stream.
const (
	EventState     = "state"
	EventStderr    = "stderr"
	EventExit      = "exit"
	EventHeartbeat = "heartbeat"
)

// Event is one NDJSON line on a GET /v1/jobs/{id}/events stream.
type Event struct {
	Type  string   `json:"type"`            // "state" | "stderr" | "exit" | "heartbeat"
	State JobState `json:"state,omitempty"` // for type=state
	Line  string   `json:"line,omitempty"`  // for type=stderr
	Code  *int     `json:"code,omitempty"`  // for type=exit
}

// Health is the GET /v1/health response of prt-workerd.
type Health struct {
	Status      string          `json:"status"` // "ok" | "degraded"
	PlexBuild   string          `json:"plex_build"`
	ActiveJobs  int             `json:"active_jobs"`
	MaxJobs     int             `json:"max_jobs"`
	VaapiOK     bool            `json:"vaapi_ok"`
	CodecsReady map[string]bool `json:"codecs_ready"`
}

// Health.Status values.
const (
	HealthOK       = "ok"
	HealthDegraded = "degraded"
)

// RegisterSessionRequest is the POST /v1/sessions payload (shim -> masterd).
// TargetDir must resolve under the masterd transcode root.
type RegisterSessionRequest struct {
	JobID     string `json:"job_id"`
	TargetDir string `json:"target_dir"`
}

// RegisterSessionResponse returns the per-session segment-push token plus
// masterd's advertised base URL.
type RegisterSessionResponse struct {
	PushToken string `json:"push_token"`
	// AdvertiseURL is masterd's LAN-reachable base URL (e.g.
	// "http://10.0.50.138:32499"). The shim uses it to build the session
	// push URL, signed media URLs and the relay base sent to the worker.
	AdvertiseURL string `json:"advertise_url"`
}

// WorkerStatus is one worker entry of a WorkersResponse: masterd's cached
// view of a single workerd.
type WorkerStatus struct {
	URL     string `json:"url"`
	Healthy bool   `json:"healthy"`
	Health  Health `json:"health"`
}

// WorkersResponse is the GET /v1/workers response of prt-masterd, consumed
// by the shim to pick a worker.
type WorkersResponse struct {
	Workers []WorkerStatus `json:"workers"`
}

// ErrorBody is the JSON body of every non-2xx response.
type ErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// ErrorBody.Error codes with contract-fixed HTTP statuses.
const (
	// ErrVersionMismatch: worker plex_build differs from the job's (HTTP 409).
	ErrVersionMismatch = "VERSION_MISMATCH"
	// ErrAtCapacity: worker is at max_jobs (HTTP 429).
	ErrAtCapacity = "AT_CAPACITY"
	// ErrNotReady: codec cache cold for this build (HTTP 503).
	ErrNotReady = "NOT_READY"
)

// CodecFile is one entry of a CodecsManifest.
type CodecFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// CodecsManifest is the GET /v1/codecs/{build}/manifest response: the file
// list (with content hashes) of the master's Codecs dir for one build.
type CodecsManifest struct {
	Build string      `json:"build"`
	Files []CodecFile `json:"files"`
}
