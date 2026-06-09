// Package config defines the JSON config schemas of the three prt roles and
// their loaders. Loaders apply defaults, then validate; a missing required
// field or an unknown key is an error (configs are nix-rendered, so an
// unknown key is always a typo).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Environment variables of the shim (injected into the PMS unit by the
// master NixOS module).
const (
	// EnvConf points at the shim JSON config (a nix-store path).
	EnvConf = "PRT_CONF"
	// EnvTokenFile points at the bearer token file. The token is NOT part of
	// ShimConfig; read it with authtok.LoadToken(os.Getenv(EnvTokenFile)).
	EnvTokenFile = "PRT_TOKEN_FILE"
)

// ShimConfig is the shim's JSON config, loaded from the file named by
// $PRT_CONF.
type ShimConfig struct {
	// MasterdURL is the shim-local base URL of prt-masterd.
	MasterdURL string `json:"masterd_url"` // default "http://127.0.0.1:32499"
	// SpawnBudgetMS bounds the total remote-attempt time on the spawn hot
	// path; once exceeded the shim execs the local fallback.
	SpawnBudgetMS int `json:"spawn_budget_ms"` // default 1500
}

// MasterdConfig is the prt-masterd JSON config (--config).
type MasterdConfig struct {
	Listen           string   `json:"listen"`             // default ":32499"
	PMSURL           string   `json:"pms_url"`            // default "http://127.0.0.1:32400"
	AdvertiseURL     string   `json:"advertise_url"`      // required; masterd's LAN-reachable base URL, e.g. "http://10.0.50.138:32499"
	TranscodeRoot    string   `json:"transcode_root"`     // required; session target dirs must be under it
	MediaRoots       []string `json:"media_roots"`        // required; allowlisted RO roots of the media server
	CodecsDir        string   `json:"codecs_dir"`         // required; master's Codecs dir
	Workers          []string `json:"workers"`            // required; workerd base URLs
	TokenFile        string   `json:"token_file"`         // required
	SessionTTLSec    int      `json:"session_ttl_sec"`    // default 600
	ProbeIntervalSec int      `json:"probe_interval_sec"` // default 10
}

// WorkerdConfig is the prt-workerd JSON config (--config).
type WorkerdConfig struct {
	Listen         string `json:"listen"`          // default ":32500"
	ProxyListen    string `json:"proxy_listen"`    // default "127.0.0.1:32401"
	MasterURL      string `json:"master_url"`      // required; masterd base URL for codec-cache sync / provisioning
	DataDir        string `json:"data_dir"`        // default "/var/lib/prt"
	EAERoot        string `json:"eae_root"`        // default "/run/prt-eae/shared"
	MaxJobs        int    `json:"max_jobs"`        // default 3
	TokenFile      string `json:"token_file"`      // required
	TranscoderPath string `json:"transcoder_path"` // required; the real "Plex Transcoder"
	PlexDir        string `json:"plex_dir"`        // required; worker plexRaw store path ("Plex Transcoder" + lib/)
	DriversDir     string `json:"drivers_dir"`     // default "<data_dir>/drivers"
	PushParallel   int    `json:"push_parallel"`   // default 4
	PushQueueCap   int    `json:"push_queue_cap"`  // default 64
}

// LoadShim loads and validates the shim config from the file named by the
// PRT_CONF environment variable.
func LoadShim() (ShimConfig, error) {
	var cfg ShimConfig
	path := os.Getenv(EnvConf)
	if path == "" {
		return cfg, fmt.Errorf("config: %s is not set", EnvConf)
	}
	if err := loadJSON(path, &cfg); err != nil {
		return ShimConfig{}, err
	}
	if cfg.MasterdURL == "" {
		cfg.MasterdURL = "http://127.0.0.1:32499"
	}
	if cfg.SpawnBudgetMS == 0 {
		cfg.SpawnBudgetMS = 1500
	}
	if cfg.SpawnBudgetMS < 0 {
		return ShimConfig{}, fmt.Errorf("config %s: spawn_budget_ms must be positive", path)
	}
	return cfg, nil
}

// LoadMasterd loads and validates the masterd config from path.
func LoadMasterd(path string) (MasterdConfig, error) {
	var cfg MasterdConfig
	if err := loadJSON(path, &cfg); err != nil {
		return MasterdConfig{}, err
	}
	if cfg.Listen == "" {
		cfg.Listen = ":32499"
	}
	if cfg.PMSURL == "" {
		cfg.PMSURL = "http://127.0.0.1:32400"
	}
	if cfg.SessionTTLSec == 0 {
		cfg.SessionTTLSec = 600
	}
	if cfg.ProbeIntervalSec == 0 {
		cfg.ProbeIntervalSec = 10
	}

	var errs []error
	if cfg.AdvertiseURL == "" {
		errs = append(errs, errors.New("advertise_url is required"))
	} else if !isHTTPURL(cfg.AdvertiseURL) {
		errs = append(errs, errors.New("advertise_url must start with http:// or https://"))
	}
	if cfg.TranscodeRoot == "" {
		errs = append(errs, errors.New("transcode_root is required"))
	}
	if len(cfg.MediaRoots) == 0 {
		errs = append(errs, errors.New("media_roots is required and must be non-empty"))
	}
	if cfg.CodecsDir == "" {
		errs = append(errs, errors.New("codecs_dir is required"))
	}
	if len(cfg.Workers) == 0 {
		errs = append(errs, errors.New("workers is required and must be non-empty"))
	}
	if cfg.TokenFile == "" {
		errs = append(errs, errors.New("token_file is required"))
	}
	if cfg.SessionTTLSec < 0 {
		errs = append(errs, errors.New("session_ttl_sec must be positive"))
	}
	if cfg.ProbeIntervalSec < 0 {
		errs = append(errs, errors.New("probe_interval_sec must be positive"))
	}
	if err := joinConfigErrs(path, errs); err != nil {
		return MasterdConfig{}, err
	}
	return cfg, nil
}

// LoadWorkerd loads and validates the workerd config from path.
func LoadWorkerd(path string) (WorkerdConfig, error) {
	var cfg WorkerdConfig
	if err := loadJSON(path, &cfg); err != nil {
		return WorkerdConfig{}, err
	}
	if cfg.Listen == "" {
		cfg.Listen = ":32500"
	}
	if cfg.ProxyListen == "" {
		cfg.ProxyListen = "127.0.0.1:32401"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "/var/lib/prt"
	}
	if cfg.EAERoot == "" {
		cfg.EAERoot = "/run/prt-eae/shared"
	}
	if cfg.MaxJobs == 0 {
		cfg.MaxJobs = 3
	}
	if cfg.DriversDir == "" {
		cfg.DriversDir = filepath.Join(cfg.DataDir, "drivers")
	}
	if cfg.PushParallel == 0 {
		cfg.PushParallel = 4
	}
	if cfg.PushQueueCap == 0 {
		cfg.PushQueueCap = 64
	}

	var errs []error
	if cfg.MasterURL == "" {
		errs = append(errs, errors.New("master_url is required"))
	} else if !isHTTPURL(cfg.MasterURL) {
		errs = append(errs, errors.New("master_url must start with http:// or https://"))
	}
	if cfg.TokenFile == "" {
		errs = append(errs, errors.New("token_file is required"))
	}
	if cfg.TranscoderPath == "" {
		errs = append(errs, errors.New("transcoder_path is required"))
	}
	if cfg.PlexDir == "" {
		errs = append(errs, errors.New("plex_dir is required"))
	}
	if cfg.MaxJobs < 0 {
		errs = append(errs, errors.New("max_jobs must be positive"))
	}
	if cfg.PushParallel < 0 {
		errs = append(errs, errors.New("push_parallel must be positive"))
	}
	if cfg.PushQueueCap < 0 {
		errs = append(errs, errors.New("push_queue_cap must be positive"))
	}
	if err := joinConfigErrs(path, errs); err != nil {
		return WorkerdConfig{}, err
	}
	return cfg, nil
}

// isHTTPURL reports whether s carries an explicit http or https scheme.
func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// loadJSON decodes one JSON object from path into v, rejecting unknown
// fields and trailing garbage.
func loadJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	if dec.More() {
		return fmt.Errorf("config %s: trailing data after JSON object", path)
	}
	return nil
}

func joinConfigErrs(path string, errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("config %s: %w", path, errors.Join(errs...))
}
