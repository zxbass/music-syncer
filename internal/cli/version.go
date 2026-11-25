package cli

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// Build information, overridden with -ldflags.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

type buildDetails struct {
	Version string
	Commit  string
	Date    string
}

func effectiveBuildDetails() buildDetails {
	details := buildDetails{Version: Version, Commit: Commit, Date: Date}
	if info, ok := debug.ReadBuildInfo(); ok {
		details.merge(info)
	}
	details.normalize()
	return details
}

// merge fills missing build details from Go's embedded module and VCS
// metadata. Plain `go install` does not pass -ldflags, but the module version
// and VCS revision are still available at runtime.
func (d *buildDetails) merge(info *debug.BuildInfo) {
	if info == nil {
		return
	}
	versionFromInfo := false
	if (d.Version == "" || d.Version == "dev") &&
		info.Main.Version != "" && info.Main.Version != "(devel)" {
		d.Version = info.Main.Version
		versionFromInfo = true
	}

	modified := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if d.Commit == "" || d.Commit == "none" {
				d.Commit = setting.Value
			}
		case "vcs.time":
			if d.Date == "" || d.Date == "unknown" {
				d.Date = setting.Value
			}
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}

	if versionFromInfo && modified &&
		!strings.HasSuffix(d.Version, "-dirty") && !strings.HasSuffix(d.Version, "+dirty") {
		d.Version += "-dirty"
	}
	if (d.Version == "" || d.Version == "dev") && d.Commit != "" && d.Commit != "none" {
		short := d.Commit
		if len(short) > 7 {
			short = short[:7]
		}
		d.Version = "dev+" + short
		if modified && !strings.HasSuffix(d.Version, "-dirty") {
			d.Version += "-dirty"
		}
	}
}

func (d *buildDetails) normalize() {
	if d.Version == "" {
		d.Version = "dev"
	}
	if d.Commit == "" {
		d.Commit = "none"
	}
	if d.Date == "" {
		d.Date = "unknown"
	}
}

func (a *App) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			details := effectiveBuildDetails()
			fmt.Fprintf(a.out, "music-syncer %s\n", details.Version)
			fmt.Fprintf(a.out, "  commit:  %s\n", details.Commit)
			fmt.Fprintf(a.out, "  built:   %s\n", details.Date)
			fmt.Fprintf(a.out, "  go:      %s\n", runtime.Version())
			fmt.Fprintf(a.out, "  platform:%s/%s\n", runtime.GOOS, runtime.GOARCH)
		},
	}
}
