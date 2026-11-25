package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zxbass/music-syncer/internal/config"
	"github.com/zxbass/music-syncer/internal/domain"
)

func testApp() *App {
	cfg := config.Default()
	cfg.Yandex.Token = "yandex-token"
	cfg.Yandex.UID = "1"
	cfg.Spotify.ClientID = "client"
	cfg.Spotify.AccessToken = "spotify-token"
	cfg.Spotify.RefreshToken = "refresh"
	return &App{cfg: cfg}
}

func TestTransferProviderInference(t *testing.T) {
	app := testApp()
	src, dst, err := app.transferProviders("", "")
	if err != nil {
		t.Fatal(err)
	}
	if src.Name() != domain.Yandex || dst.Name() != domain.Spotify {
		t.Errorf("default direction = %s -> %s", src.Name(), dst.Name())
	}

	src, dst, err = app.transferProviders("spotify", "")
	if err != nil {
		t.Fatal(err)
	}
	if src.Name() != domain.Spotify || dst.Name() != domain.Yandex {
		t.Errorf("inferred direction = %s -> %s", src.Name(), dst.Name())
	}

	if _, _, err := app.transferProviders("yandex", "yandex"); err == nil {
		t.Error("expected an error for identical providers")
	}
	if _, _, err := app.transferProviders("deezer", ""); err == nil {
		t.Error("expected an error for an unknown provider")
	}
}

func TestTransferProviderRequiresBothAccounts(t *testing.T) {
	cfg := config.Default()
	cfg.Spotify.AccessToken = "token"
	app := &App{cfg: cfg}
	if _, _, err := app.transferProviders("", ""); err == nil {
		t.Error("expected an error when only one provider is connected")
	}
	if _, _, err := app.transferProviders("spotify", "yandex"); err == nil {
		t.Error("expected an error when the target is not connected")
	}
}

func TestParseSince(t *testing.T) {
	got, err := parseSince("2026-01-02")
	if err != nil {
		t.Fatal(err)
	}
	if got.Format("2006-01-02") != "2026-01-02" {
		t.Errorf("date = %v", got)
	}

	before := time.Now().AddDate(0, 0, -30)
	got, err = parseSince("30d")
	if err != nil {
		t.Fatal(err)
	}
	if got.Before(before.Add(-time.Minute)) || got.After(time.Now()) {
		t.Errorf("30d = %v", got)
	}

	if _, err := parseSince("nope"); err == nil {
		t.Error("expected an error")
	}
	if got, err := parseSince(""); err != nil || !got.IsZero() {
		t.Errorf("empty = %v, %v", got, err)
	}
}

func TestWriteJSONFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "data.json")
	if err := writeJSONFile(path, map[string]string{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["a"] != "b" {
		t.Errorf("decoded = %v", decoded)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Error("JSON file does not end with a newline")
	}
}

func TestAccountLabel(t *testing.T) {
	label := accountLabel(domain.Account{Provider: domain.Spotify, ID: "1", DisplayName: "Me"})
	if !strings.Contains(label, "Spotify") || !strings.Contains(label, "Me") {
		t.Errorf("label = %q", label)
	}
}
