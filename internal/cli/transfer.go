package cli

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/zxbass/music-syncer/internal/syncer"
)

func (a *App) transferCmd() *cobra.Command {
	var (
		from, to     string
		only         string
		playlists    []string
		allPlaylists bool
		prefix       string
		mirror       bool
		dryRun       bool
		limit        int
		reportPath   string
		threshold    float64
	)
	cmd := &cobra.Command{
		Use:   "transfer",
		Short: "Transfer a library between Yandex Music and Spotify",
		Long: `Transfer playlists, liked tracks and followed artists.

Without --from/--to, music-syncer uses Yandex Music as the source and Spotify
as the target when both accounts are connected. One of them can be omitted and
is inferred from the connected accounts.

The default categories are playlists,likes,artists. Playback history is
intentionally not transferred: it is read-only on both platforms and would
only add API load.

Playlists are synced additively: existing tracks are kept and only missing
tracks are added. Use --mirror to remove tracks that are not in the source
playlist. Mirror removal is skipped automatically when any source track could
not be matched, or when the source was truncated with --limit.

Liked tracks are not written to the target's native favorites. They are added
to a separate "<source service> favorites" playlist (for example,
"Yandex favorites" when transferring from Yandex Music to Spotify).`,
		Example: `  music-syncer transfer --from yandex --to spotify --dry-run
  music-syncer transfer --from yandex --to spotify
  music-syncer transfer --from yandex --to spotify --only likes,artists
  music-syncer transfer --from spotify --to yandex --playlist "Road trip"
  music-syncer transfer --from yandex --to spotify --mirror --report report.json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			categories, err := syncer.ParseCategories(only)
			if err != nil {
				return err
			}
			if limit < 0 {
				return errors.New("--limit cannot be negative")
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

			fmt.Fprintf(a.out, "Source: %s\n", accountLabel(srcAcc))
			fmt.Fprintf(a.out, "Target: %s\n", accountLabel(dstAcc))
			fmt.Fprintf(a.out, "Categories: %s\n", categories.String())
			if dryRun {
				fmt.Fprintln(a.out, "Mode: dry run (no changes will be written)")
			}
			if mirror {
				fmt.Fprintln(a.out, "Mirror: enabled for playlists (skipped on unmatched tracks or --limit truncation)")
			}
			if len(playlists) > 0 {
				fmt.Fprintf(a.out, "Playlist filters: %s\n", strings.Join(playlists, ", "))
			}
			fmt.Fprintln(a.out)

			engine := &syncer.Engine{
				Source:  src,
				Target:  dst,
				Matcher: a.newMatcher(),
				Store:   store,
				Log:     a.log,
				Options: syncer.Options{
					Categories:   categories,
					Playlists:    playlists,
					AllPlaylists: allPlaylists,
					Prefix:       prefix,
					Mirror:       mirror,
					DryRun:       dryRun,
					Limit:        limit,
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
				return runErr
			}
			if report != nil && len(report.Errors) > 0 {
				return fmt.Errorf("transfer completed with %s%s",
					plural(len(report.Errors), "error", "errors"),
					reportHint(reportPath))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "source provider: yandex or spotify")
	cmd.Flags().StringVar(&to, "to", "", "target provider: yandex or spotify")
	cmd.Flags().StringVar(&only, "only", "playlists,likes,artists", "comma-separated categories: playlists,likes,artists,all")
	cmd.Flags().StringArrayVar(&playlists, "playlist", nil, "only sync playlists matching a name, ID or substring (repeatable)")
	cmd.Flags().BoolVar(&allPlaylists, "all-playlists", false, "include generated/algorithmic playlists")
	cmd.Flags().StringVar(&prefix, "prefix", "", "prefix for created target playlist names")
	cmd.Flags().BoolVar(&mirror, "mirror", false, "remove target playlist tracks that are missing from the source")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the plan without changing anything")
	cmd.Flags().IntVar(&limit, "limit", 0, "limit source tracks per collection (0 = no limit)")
	cmd.Flags().StringVar(&reportPath, "report", "", "write a JSON report to this path")
	cmd.Flags().Float64Var(&threshold, "threshold", 0, "override the match threshold (0.0-1.0)")
	return cmd
}
