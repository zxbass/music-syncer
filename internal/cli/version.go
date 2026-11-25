package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// Build information, overridden with -ldflags.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

func (a *App) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(a.out, "music-syncer %s\n", Version)
			fmt.Fprintf(a.out, "  commit:  %s\n", Commit)
			fmt.Fprintf(a.out, "  built:   %s\n", Date)
			fmt.Fprintf(a.out, "  go:      %s\n", runtime.Version())
			fmt.Fprintf(a.out, "  platform:%s/%s\n", runtime.GOOS, runtime.GOARCH)
		},
	}
}
