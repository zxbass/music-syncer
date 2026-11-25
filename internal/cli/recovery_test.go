package cli

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestVersionWorksWithInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("broken: [yaml"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := NewRootCmd()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"--config", cfgPath, "version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("version failed with invalid config: %v", err)
	}
}

func TestCacheClearWorksWithInvalidConfigAndState(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(cfgPath, []byte("broken: [yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := NewRootCmd()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"--config", cfgPath, "cache", "clear"})
	if err := root.Execute(); err != nil {
		t.Fatalf("cache clear failed: %v", err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state file was not removed: %v", err)
	}
}
