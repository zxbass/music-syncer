package cli

import (
	"runtime/debug"
	"testing"
)

func TestBuildDetailsFromModuleVersion(t *testing.T) {
	details := buildDetails{Version: "dev", Commit: "none", Date: "unknown"}
	details.merge(&debug.BuildInfo{Main: debug.Module{Version: "v0.3.1"}})
	details.normalize()
	if details.Version != "v0.3.1" {
		t.Errorf("version = %q, want v0.3.1", details.Version)
	}
}

func TestBuildDetailsFromVCS(t *testing.T) {
	details := buildDetails{Version: "dev", Commit: "none", Date: "unknown"}
	details.merge(&debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abcdef1234567890"},
			{Key: "vcs.time", Value: "2026-11-26T00:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	})
	details.normalize()
	if details.Version != "dev+abcdef1-dirty" {
		t.Errorf("version = %q, want dev+abcdef1-dirty", details.Version)
	}
	if details.Commit != "abcdef1234567890" {
		t.Errorf("commit = %q", details.Commit)
	}
	if details.Date != "2026-11-26T00:00:00Z" {
		t.Errorf("date = %q", details.Date)
	}
}

func TestBuildDetailsDoNotDoubleDirtySuffix(t *testing.T) {
	details := buildDetails{Version: "dev", Commit: "none", Date: "unknown"}
	details.merge(&debug.BuildInfo{
		Main: debug.Module{Version: "v0.3.0+dirty"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.modified", Value: "true"},
		},
	})
	details.normalize()
	if details.Version != "v0.3.0+dirty" {
		t.Errorf("version = %q, want v0.3.0+dirty", details.Version)
	}
}

func TestBuildDetailsKeepLDFlags(t *testing.T) {
	details := buildDetails{Version: "v0.3.1", Commit: "66ddfe0", Date: "built"}
	details.merge(&debug.BuildInfo{
		Main: debug.Module{Version: "v0.1.0"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abcdef1234567890"},
			{Key: "vcs.time", Value: "2026-11-26T00:00:00Z"},
		},
	})
	details.normalize()
	if details.Version != "v0.3.1" || details.Commit != "66ddfe0" || details.Date != "built" {
		t.Errorf("details = %+v, want ldflags values preserved", details)
	}
}
