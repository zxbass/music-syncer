package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSaveLoadAndPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.yaml")
	cfg := Default()
	cfg.Spotify.ClientID = "client"
	cfg.Spotify.AccessToken = "access"
	cfg.Spotify.RefreshToken = "refresh"
	cfg.Spotify.Expiry = time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	cfg.Yandex.Token = "yandex"
	cfg.Yandex.UID = "123"
	cfg.Match.Threshold = 0.8

	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Spotify.ClientID != "client" || loaded.Yandex.UID != "123" || loaded.Match.Threshold != 0.8 {
		t.Errorf("loaded = %+v", loaded)
	}
	if loaded.Yandex.Token != "yandex" {
		t.Errorf("token not persisted")
	}
}

func TestLoadMissingAndEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Match.Threshold != DefaultMatchThreshold {
		t.Errorf("threshold = %v", cfg.Match.Threshold)
	}

	t.Setenv("YANDEX_MUSIC_TOKEN", "from-env")
	t.Setenv("SPOTIFY_CLIENT_ID", "spotify-env")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Yandex.Token != "from-env" || cfg.Spotify.ClientID != "spotify-env" {
		t.Errorf("env not applied: %+v", cfg)
	}
}

func TestDefaultPathFromEnv(t *testing.T) {
	want := filepath.Join(t.TempDir(), "custom.yaml")
	t.Setenv("MUSIC_SYNCER_CONFIG", want)
	got, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("DefaultPath = %q, want %q", got, want)
	}
}

func TestPermissionsWarning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses ACLs instead of POSIX permission bits")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := Save(path, Default()); err != nil {
		t.Fatal(err)
	}
	if err := WarnIfPermissionsAreLoose(path); err != nil {
		t.Errorf("0600 config warned: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WarnIfPermissionsAreLoose(path); err == nil {
		t.Error("expected a warning for a world-readable config")
	}
	if err := WarnIfPermissionsAreLoose(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Errorf("missing file returned an error: %v", err)
	}
}

func TestMaskSecret(t *testing.T) {
	if got := MaskSecret(""); got != "(not set)" {
		t.Errorf("empty = %q", got)
	}
	if got := MaskSecret("short"); got != "****" {
		t.Errorf("short = %q", got)
	}
	if got := MaskSecret("abcdefghijklmnop"); got != "abcd…mnop" {
		t.Errorf("long = %q", got)
	}
}
