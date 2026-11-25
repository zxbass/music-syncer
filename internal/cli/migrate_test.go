package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveNativeLikeScopeRequiresChoice(t *testing.T) {
	if _, err := resolveNativeLikeScope(false, "", "", ""); err == nil {
		t.Fatal("expected an error without a scope")
	}
}

func TestResolveNativeLikeScopeAll(t *testing.T) {
	scope, err := resolveNativeLikeScope(true, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !scope.All {
		t.Error("expected all-native scope")
	}
	if _, err := resolveNativeLikeScope(true, "report.json", "", ""); err == nil {
		t.Error("expected --all-native conflict error")
	}
}

func TestResolveNativeLikeScopeTimes(t *testing.T) {
	scope, err := resolveNativeLikeScope(false, "", "2026-11-25T19:00:00Z", "2026-11-25T19:20:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if scope.Since == nil || scope.Until == nil {
		t.Fatalf("scope = %+v", scope)
	}
	if got := scope.Since.UTC().Format(time.RFC3339); got != "2026-11-25T19:00:00Z" {
		t.Errorf("since = %s", got)
	}
	if _, err := resolveNativeLikeScope(false, "", "2026-11-25T19:20:00Z", "2026-11-25T19:00:00Z"); err == nil {
		t.Error("expected reversed-window error")
	}
}

func TestResolveNativeLikeScopeFromReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	data := []byte(`{"started_at":"2026-11-25T19:00:00Z","finished_at":"2026-11-25T19:20:00Z","dry_run":false}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	scope, err := resolveNativeLikeScope(false, path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if scope.Since == nil || scope.Until == nil {
		t.Fatalf("scope = %+v", scope)
	}
	if got := scope.Until.UTC().Format(time.RFC3339); got != "2026-11-25T19:20:00Z" {
		t.Errorf("until = %s", got)
	}

	dry := filepath.Join(t.TempDir(), "dry.json")
	if err := os.WriteFile(dry, []byte(`{"started_at":"2026-11-25T19:00:00Z","finished_at":"2026-11-25T19:20:00Z","dry_run":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveNativeLikeScope(false, dry, "", ""); err == nil {
		t.Error("expected an error for a dry-run report")
	}
}
