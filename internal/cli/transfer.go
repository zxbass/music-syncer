package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/zxbass/music-syncer/internal/domain"
	"github.com/zxbass/music-syncer/internal/matcher"
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
		historySince string
		reportPath   string
		threshold    float64
	)
	cmd := &cobra.Command{
		Use:   "transfer",
		Short: "Transfer a library between Yandex Music and Spotify",
		Long: `Transfer playlists, liked tracks, followed artists and listening history.

Without --from/--to, music-syncer uses Yandex Music as the source and Spotify
as the target when both accounts are connected. One of them can be omitted and
is inferred from the connected accounts.

The default categories are playlists,likes,artists. Listening history is
optional because public music APIs cannot write playback history: it is
archived as a playlist instead.

Playlists are synced additively: existing tracks are kept and only missing
tracks are added. Use --mirror to remove tracks that are not in the source
playlist. Mirror removal is skipped automatically when any source track could
not be matched, to avoid deleting tracks by mistake.`,
		Example: `  music-syncer transfer --from yandex --to spotify --dry-run
  music-syncer transfer --from yandex --to spotify
  music-syncer transfer --from yandex --to spotify --only likes,artists
  music-syncer transfer --from spotify --to yandex --playlist "Road trip"
  music-syncer transfer --from yandex --to spotify --only history --history-since 90d
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
			since, err := parseSince(historySince)
			if err != nil {
				return err
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
				fmt.Fprintln(a.out, "Mirror: enabled for playlists (safe mode: skipped on unmatched tracks)")
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
					HistorySince: since,
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
	cmd.Flags().StringVar(&only, "only", "playlists,likes,artists", "comma-separated categories: playlists,likes,artists,history,all")
	cmd.Flags().StringArrayVar(&playlists, "playlist", nil, "only sync playlists matching a name, ID or substring (repeatable)")
	cmd.Flags().BoolVar(&allPlaylists, "all-playlists", false, "include generated/algorithmic playlists")
	cmd.Flags().StringVar(&prefix, "prefix", "", "prefix for created target playlist names")
	cmd.Flags().BoolVar(&mirror, "mirror", false, "remove target playlist tracks that are missing from the source")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the plan without changing anything")
	cmd.Flags().IntVar(&limit, "limit", 0, "limit source tracks per collection (0 = no limit)")
	cmd.Flags().StringVar(&historySince, "history-since", "", "history window: YYYY-MM-DD or 30d/4w/6m")
	cmd.Flags().StringVar(&reportPath, "report", "", "write a JSON report to this path")
	cmd.Flags().Float64Var(&threshold, "threshold", 0, "override the match threshold (0.0-1.0)")
	return cmd
}

func accountLabel(acc domain.Account) string {
	name := acc.DisplayName
	if name == "" {
		name = acc.Login
	}
	if name == "" {
		return fmt.Sprintf("%s (id %s)", acc.Provider.DisplayName(), acc.ID)
	}
	return fmt.Sprintf("%s — %s (id %s)", acc.Provider.DisplayName(), name, acc.ID)
}

func reportHint(path string) string {
	if path != "" {
		return ""
	}
	return "; use --report to save details"
}

func printTransferReport(w io.Writer, report *syncer.Report) {
	title := "Transfer complete"
	if report.DryRun {
		title = "Dry run complete"
	}
	fmt.Fprintf(w, "\n%s: %s → %s\n", title, report.Source.DisplayName(), report.Target.DisplayName())
	fmt.Fprintf(w, "Duration: %s\n", report.FinishedAt.Sub(report.StartedAt).Round(time.Second))

	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "\nPLAYLIST\tTRACKS\tADDED\tEXISTING\tUNMATCHED\tREMOVED\tSTATUS")
	for _, pl := range report.Playlists {
		status := "ok"
		switch {
		case pl.Error != "":
			status = "ERROR"
		case pl.Created:
			status = "created"
		case pl.WouldCreate && report.DryRun:
			status = "would create"
		case len(pl.Notes) > 0:
			status = "ok*"
		}
		name := pl.SourceName
		if name == "" {
			name = pl.TargetName
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%s\n",
			name, pl.Stats.Source, pl.Stats.Added, pl.Stats.Existing, pl.Stats.Unmatched, pl.Removed, status)
	}
	_ = tw.Flush()

	for _, pl := range report.Playlists {
		for _, note := range pl.Notes {
			fmt.Fprintf(w, "  note [%s]: %s\n", pl.SourceName, note)
		}
		if pl.Error != "" {
			fmt.Fprintf(w, "  error [%s]: %s\n", pl.SourceName, pl.Error)
		}
	}

	if report.Likes != nil {
		fmt.Fprintf(w, "\nLiked tracks: %d source, %d added, %d already liked, %d unmatched, %d failed\n",
			report.Likes.Source, report.Likes.Added, report.Likes.Existing, report.Likes.Unmatched, report.Likes.Failed)
	}
	if report.Artists != nil {
		fmt.Fprintf(w, "Artists: %d source, %d followed, %d already followed, %d unmatched, %d failed\n",
			report.Artists.Source, report.Artists.Added, report.Artists.Existing, report.Artists.Unmatched, report.Artists.Failed)
	}
	if report.History != nil {
		fmt.Fprintf(w, "History: %d events, %d unique tracks, %d added, %d already present, %d unmatched\n",
			report.History.Events, report.History.UniqueTracks, report.History.Stats.Added,
			report.History.Stats.Existing, report.History.Stats.Unmatched)
		for _, note := range report.History.Notes {
			fmt.Fprintf(w, "  note: %s\n", note)
		}
	}

	unmatched := collectUnmatched(report)
	if len(unmatched) > 0 {
		fmt.Fprintf(w, "\nUnmatched tracks (%d):\n", len(unmatched))
		const maxPrint = 20
		for i, u := range unmatched {
			if i >= maxPrint {
				fmt.Fprintf(w, "  … and %d more (use --report to save the full list)\n", len(unmatched)-maxPrint)
				break
			}
			fmt.Fprintf(w, "  - %s\n", u.Source.String())
			if u.Best != nil {
				fmt.Fprintf(w, "      best: %s [score %.2f, title %.2f, artist %.2f] %s\n",
					u.Best.Track.String(), u.Best.Score, u.Best.TitleScore, u.Best.ArtistScore,
					versionNote(*u.Best))
			} else {
				fmt.Fprintf(w, "      %s\n", u.Reason)
			}
		}
	}
	if len(report.UnmatchedArtists) > 0 {
		fmt.Fprintf(w, "\nUnmatched artists (%d):\n", len(report.UnmatchedArtists))
		const maxPrint = 10
		for i, u := range report.UnmatchedArtists {
			if i >= maxPrint {
				fmt.Fprintf(w, "  … and %d more\n", len(report.UnmatchedArtists)-maxPrint)
				break
			}
			fmt.Fprintf(w, "  - %s: %s\n", u.Source.Name, u.Reason)
		}
	}

	if len(report.Errors) > 0 {
		fmt.Fprintf(w, "\nErrors (%d):\n", len(report.Errors))
		const maxPrint = 10
		for i, e := range report.Errors {
			if i >= maxPrint {
				fmt.Fprintf(w, "  … and %d more\n", len(report.Errors)-maxPrint)
				break
			}
			fmt.Fprintf(w, "  - %s\n", e)
		}
	}
	if report.DryRun {
		fmt.Fprintln(w, "\nDry run: no changes were written.")
	}
}

func collectUnmatched(report *syncer.Report) []syncer.Unmatched {
	var result []syncer.Unmatched
	for _, pl := range report.Playlists {
		result = append(result, pl.Unmatched...)
	}
	result = append(result, report.UnmatchedLikes...)
	if report.History != nil {
		result = append(result, report.History.Unmatched...)
	}
	return result
}

func versionNote(c matcher.Candidate) string {
	if c.StrongVersionConflict {
		return "(different version)"
	}
	return ""
}
