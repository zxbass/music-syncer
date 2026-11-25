package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/zxbass/music-syncer/internal/syncer"
)

func (a *App) migrateLikesCmd() *cobra.Command {
	var (
		from, to    string
		prefix      string
		dryRun      bool
		yes         bool
		limit       int
		searchBatch int
		reportPath  string
		threshold   float64
	)
	cmd := &cobra.Command{
		Use:   "migrate-likes",
		Short: "Move imported likes from native favorites to a playlist",
		Long: `Move liked tracks from the target's native favorites into the separate
"<source service> favorites" playlist used by current versions.

This is intended for libraries imported by older versions that wrote likes to
Spotify Liked Songs. The command matches source liked tracks, adds the missing
ones to the favorites playlist, and only removes them from native likes after
they are confirmed present in that playlist.

Matching likes that were already in the native library are removed too: the
command moves all matching source likes out of the native collection. Use
--dry-run to inspect the plan before changing anything.`,
		Example: `  music-syncer migrate-likes --dry-run
  music-syncer migrate-likes --yes --search-batch 100
  music-syncer migrate-likes --from spotify --to yandex --dry-run`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 0 {
				return errors.New("--limit cannot be negative")
			}
			if searchBatch < 0 {
				return errors.New("--search-batch cannot be negative")
			}
			if threshold < 0 || threshold > 1 {
				return errors.New("--threshold must be between 0 and 1")
			}

			src, dst, err := a.transferProviders(from, to)
			if err != nil {
				return err
			}
			store, err := a.openStore()
			if err != nil {
				return err
			}

			srcAcc, err := a.identify(cmd.Context(), src)
			if err != nil {
				return fmt.Errorf("check source account: %w", err)
			}
			dstAcc, err := a.identify(cmd.Context(), dst)
			if err != nil {
				return fmt.Errorf("check target account: %w", err)
			}

			playlistName := prefix + syncer.FavoritesPlaylistName(src.Name())
			fmt.Fprintf(a.out, "music-syncer %s\n", Version)
			fmt.Fprintf(a.out, "Source: %s\n", accountLabel(srcAcc))
			fmt.Fprintf(a.out, "Target: %s\n", accountLabel(dstAcc))
			fmt.Fprintf(a.out, "Favorites playlist: %s\n", playlistName)
			if dryRun {
				fmt.Fprintln(a.out, "Mode: dry run (no likes will be changed)")
			}
			if searchBatch > 0 {
				fmt.Fprintf(a.out, "Search batch: %d new catalog lookups per run\n", searchBatch)
			}
			fmt.Fprintln(a.out)
			fmt.Fprintf(a.out, "This moves matching source likes out of %s native liked songs into %q.\n",
				dst.Name().DisplayName(), playlistName)

			if !dryRun && !yes {
				ok, err := confirm(cmd, a.out, "Continue? [y/N] ")
				if err != nil {
					return err
				}
				if !ok {
					return errors.New("aborted")
				}
			}

			engine := &syncer.Engine{
				Source:  src,
				Target:  dst,
				Matcher: a.newMatcher(),
				Store:   store,
				Log:     a.log,
				Options: syncer.Options{
					Categories:   syncer.Categories{Likes: true},
					MigrateLikes: true,
					Prefix:       prefix,
					DryRun:       dryRun,
					Limit:        limit,
					SearchBatch:  searchBatch,
				},
			}
			if threshold > 0 {
				engine.Matcher.Threshold = threshold
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			report, runErr := engine.Run(ctx)
			if report != nil {
				printTransferReport(a.out, report)
			}
			if reportPath != "" && report != nil {
				if err := writeJSONFile(reportPath, report); err != nil {
					return fmt.Errorf("write report: %w", err)
				}
				fmt.Fprintf(a.out, "\nReport written to %s\n", reportPath)
			}
			if runErr != nil {
				return a.wrapTransferError(runErr)
			}
			if report != nil && len(report.Errors) > 0 {
				return fmt.Errorf("migration completed with %s%s",
					plural(len(report.Errors), "error", "errors"),
					reportHint(reportPath))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "source provider: yandex or spotify")
	cmd.Flags().StringVar(&to, "to", "", "target provider: yandex or spotify")
	cmd.Flags().StringVar(&prefix, "prefix", "", "prefix for the favorites playlist name")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the plan without changing likes or playlists")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	cmd.Flags().IntVar(&limit, "limit", 0, "limit source liked tracks (0 = no limit)")
	cmd.Flags().IntVar(&searchBatch, "search-batch", 0, "stop after N new catalog searches per run (0 = unlimited)")
	cmd.Flags().StringVar(&reportPath, "report", "", "write a JSON report to this path")
	cmd.Flags().Float64Var(&threshold, "threshold", 0, "override the match threshold (0.0-1.0)")
	return cmd
}

func confirm(cmd *cobra.Command, out io.Writer, prompt string) (bool, error) {
	fmt.Fprint(out, prompt)
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes", "д", "да":
		return true, nil
	default:
		return false, nil
	}
}
