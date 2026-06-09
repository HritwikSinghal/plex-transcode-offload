package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadShimDefaults(t *testing.T) {
	t.Setenv(EnvConf, writeConfig(t, `{}`))
	cfg, err := LoadShim()
	if err != nil {
		t.Fatalf("LoadShim: %v", err)
	}
	if cfg.MasterdURL != "http://127.0.0.1:32499" {
		t.Errorf("MasterdURL default = %q", cfg.MasterdURL)
	}
	if cfg.SpawnBudgetMS != 1500 {
		t.Errorf("SpawnBudgetMS default = %d", cfg.SpawnBudgetMS)
	}
}

func TestLoadShimExplicit(t *testing.T) {
	t.Setenv(EnvConf, writeConfig(t, `{"masterd_url":"http://m:1","spawn_budget_ms":2000}`))
	cfg, err := LoadShim()
	if err != nil {
		t.Fatalf("LoadShim: %v", err)
	}
	if cfg.MasterdURL != "http://m:1" || cfg.SpawnBudgetMS != 2000 {
		t.Errorf("got %+v", cfg)
	}
}

func TestLoadShimEnvUnset(t *testing.T) {
	t.Setenv(EnvConf, "")
	if _, err := LoadShim(); err == nil {
		t.Error("expected error with PRT_CONF unset")
	}
}

func TestLoadShimMissingFile(t *testing.T) {
	t.Setenv(EnvConf, filepath.Join(t.TempDir(), "nope.json"))
	if _, err := LoadShim(); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLoadShimUnknownField(t *testing.T) {
	t.Setenv(EnvConf, writeConfig(t, `{"masterd_uri":"typo"}`))
	if _, err := LoadShim(); err == nil {
		t.Error("expected error for unknown field")
	}
}

const masterdRequired = `"transcode_root":"/transcode","media_roots":["/mnt/media"],` +
	`"codecs_dir":"/codecs","workers":["http://w:32500"],"token_file":"/run/token"`

func TestLoadMasterdDefaults(t *testing.T) {
	cfg, err := LoadMasterd(writeConfig(t, `{`+masterdRequired+`}`))
	if err != nil {
		t.Fatalf("LoadMasterd: %v", err)
	}
	if cfg.Listen != ":32499" {
		t.Errorf("Listen default = %q", cfg.Listen)
	}
	if cfg.PMSURL != "http://127.0.0.1:32400" {
		t.Errorf("PMSURL default = %q", cfg.PMSURL)
	}
	if cfg.SessionTTLSec != 600 {
		t.Errorf("SessionTTLSec default = %d", cfg.SessionTTLSec)
	}
	if cfg.ProbeIntervalSec != 10 {
		t.Errorf("ProbeIntervalSec default = %d", cfg.ProbeIntervalSec)
	}
	if cfg.TranscodeRoot != "/transcode" || len(cfg.MediaRoots) != 1 || len(cfg.Workers) != 1 {
		t.Errorf("required fields not loaded: %+v", cfg)
	}
}

func TestLoadMasterdMissingRequired(t *testing.T) {
	_, err := LoadMasterd(writeConfig(t, `{"listen":":1"}`))
	if err == nil {
		t.Fatal("expected error for missing required fields")
	}
	for _, want := range []string{"transcode_root", "media_roots", "codecs_dir", "workers", "token_file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestLoadMasterdEmptyLists(t *testing.T) {
	body := `{"transcode_root":"/t","media_roots":[],"codecs_dir":"/c","workers":[],"token_file":"/tok"}`
	if _, err := LoadMasterd(writeConfig(t, body)); err == nil {
		t.Error("expected error for empty media_roots/workers")
	}
}

const workerdRequired = `"token_file":"/run/token","transcoder_path":"/nix/store/x/Plex Transcoder",` +
	`"plex_dir":"/nix/store/x"`

func TestLoadWorkerdDefaults(t *testing.T) {
	cfg, err := LoadWorkerd(writeConfig(t, `{`+workerdRequired+`}`))
	if err != nil {
		t.Fatalf("LoadWorkerd: %v", err)
	}
	if cfg.Listen != ":32500" {
		t.Errorf("Listen default = %q", cfg.Listen)
	}
	if cfg.ProxyListen != "127.0.0.1:32401" {
		t.Errorf("ProxyListen default = %q", cfg.ProxyListen)
	}
	if cfg.DataDir != "/var/lib/prt" {
		t.Errorf("DataDir default = %q", cfg.DataDir)
	}
	if cfg.EAERoot != "/run/prt-eae/shared" {
		t.Errorf("EAERoot default = %q", cfg.EAERoot)
	}
	if cfg.MaxJobs != 3 {
		t.Errorf("MaxJobs default = %d", cfg.MaxJobs)
	}
	if cfg.DriversDir != "/var/lib/prt/drivers" {
		t.Errorf("DriversDir default = %q", cfg.DriversDir)
	}
	if cfg.PushParallel != 4 {
		t.Errorf("PushParallel default = %d", cfg.PushParallel)
	}
	if cfg.PushQueueCap != 64 {
		t.Errorf("PushQueueCap default = %d", cfg.PushQueueCap)
	}
}

func TestLoadWorkerdDriversDirFollowsDataDir(t *testing.T) {
	cfg, err := LoadWorkerd(writeConfig(t, `{`+workerdRequired+`,"data_dir":"/srv/prt"}`))
	if err != nil {
		t.Fatalf("LoadWorkerd: %v", err)
	}
	if cfg.DriversDir != "/srv/prt/drivers" {
		t.Errorf("DriversDir = %q, want /srv/prt/drivers", cfg.DriversDir)
	}
}

func TestLoadWorkerdExplicitDriversDir(t *testing.T) {
	cfg, err := LoadWorkerd(writeConfig(t, `{`+workerdRequired+`,"drivers_dir":"/opt/drv"}`))
	if err != nil {
		t.Fatalf("LoadWorkerd: %v", err)
	}
	if cfg.DriversDir != "/opt/drv" {
		t.Errorf("DriversDir = %q", cfg.DriversDir)
	}
}

func TestLoadWorkerdMissingRequired(t *testing.T) {
	_, err := LoadWorkerd(writeConfig(t, `{}`))
	if err == nil {
		t.Fatal("expected error for missing required fields")
	}
	for _, want := range []string{"token_file", "transcoder_path", "plex_dir"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestLoadWorkerdNegativeValues(t *testing.T) {
	if _, err := LoadWorkerd(writeConfig(t, `{`+workerdRequired+`,"max_jobs":-1}`)); err == nil {
		t.Error("expected error for negative max_jobs")
	}
}

func TestLoadJSONTrailingGarbage(t *testing.T) {
	if _, err := LoadMasterd(writeConfig(t, `{`+masterdRequired+`} {"x":1}`)); err == nil {
		t.Error("expected error for trailing data")
	}
}
