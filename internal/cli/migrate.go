package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zxbass/music-syncer/internal/syncer"
)

func (a *App) migrateLikesCmd() *cobra.Command {
	var (
		from, to     string
		prefix       string
		dryRun       bool
		yes          bool
		limit        int
		searchBatch  int
		reportPath   string
		threshold    float64
		addedAfter   string
		addedBefore  string
		importReport string
		allNative    bool
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

By default the command requires a time scope. Use --import-report with the old
transfer report, or --added-after/--added-before. Only native likes whose
Spotify added_at falls inside that window are removed, so likes that existed
before the import stay untouched. Use --all-native to move every matching
native like instead.`,
		Example: `  music-syncer migrate-likes --import-report old-report.json --dry-run
  music-syncer migrate-likes --added-after 2026-11-25T19:00:00Z --added-before 2026-11-25T19:20:00Z --dry-run
  music-syncer migrate-likes --import-report old-report.json --yes --search-batch 100
  music-syncer migrate-likes --all-native --dry-run`,
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

			scope, err := resolveNativeLikeScope(allNative, importReport, addedAfter, addedBefore)
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

			playlistName := prefix + syncer.FavoritesPlaylistName(src.Name())
			fmt.Fprintf(a.out, "music-syncer %s\n", Version)
			fmt.Fprintf(a.out, "Source: %s\n", accountLabel(srcAcc))
			fmt.Fprintf(a.out, "Target: %s\n", accountLabel(dstAcc))
			fmt.Fprintf(a.out, "Favorites playlist: %s\n", playlistName)
			fmt.Fprintf(a.out, "Native likes scope: %s\n", scope.Description)
			if dryRun {
				fmt.Fprintln(a.out, "Mode: dry run (no likes will be changed)")
			}
			if searchBatch > 0 {
				fmt.Fprintf(a.out, "Search batch: %d new catalog lookups per run\n", searchBatch)
			}
			fmt.Fprintln(a.out)
			if scope.All {
				fmt.Fprintf(a.out, "This moves ALL matching source likes out of %s native liked songs into %q.\n",
					dst.Name().DisplayName(), playlistName)
			} else {
				fmt.Fprintf(a.out, "This moves source likes inside the selected window out of %s native liked songs into %q; older likes are kept.\n",
					dst.Name().DisplayName(), playlistName)
			}

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
					Categories:           syncer.Categories{Likes: true},
					MigrateLikes:         true,
					NativeLikesSince:     scope.Since,
					NativeLikesUntil:     scope.Until,
					RemoveAllNativeLikes: scope.All,
					Prefix:               prefix,
					DryRun:               dryRun,
					Limit:                limit,
					SearchBatch:          searchBatch,
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
	cmd.Flags().StringVar(&importReport, "import-report", "", "old transfer report.json used as the native-like added_at window")
	cmd.Flags().StringVar(&addedAfter, "added-after", "", "only remove native likes added at/after this time")
	cmd.Flags().StringVar(&addedBefore, "added-before", "", "only remove native likes added at/before this time")
	cmd.Flags().BoolVar(&allNative, "all-native", false, "remove all matching native likes, including pre-existing ones")
	cmd.Flags().IntVar(&limit, "limit", 0, "limit source liked tracks (0 = no limit)")
	cmd.Flags().IntVar(&searchBatch, "search-batch", 0, "stop after N new catalog searches per run (0 = unlimited)")
	cmd.Flags().StringVar(&reportPath, "report", "", "write a JSON report to this path")
	cmd.Flags().Float64Var(&threshold, "threshold", 0, "override the match threshold (0.0-1.0)")
	return cmd
}

type nativeLikeScope struct {
	All         bool
	Since       *time.Time
	Until       *time.Time
	Description string
}

func resolveNativeLikeScope(all bool, importReport, addedAfter, addedBefore string) (nativeLikeScope, error) {
	if all {
		if importReport != "" || addedAfter != "" || addedBefore != "" {
			return nativeLikeScope{}, errors.New("--all-native cannot be combined with --import-report or --added-after/--added-before")
		}
		return nativeLikeScope{All: true, Description: "all matching native liked tracks"}, nil
	}
	if importReport != "" {
		if addedAfter != "" || addedBefore != "" {
			return nativeLikeScope{}, errors.New("--import-report cannot be combined with --added-after/--added-before")
		}
		window, err := readTransferWindow(importReport)
		if err != nil {
			return nativeLikeScope{}, err
		}
		return nativeLikeScope{
			Since: &window.StartedAt,
			Until: &window.FinishedAt,
			Description: fmt.Sprintf("native likes added %s — %s (from %s)",
				window.StartedAt.Format(time.RFC3339), window.FinishedAt.Format(time.RFC3339), importReport),
		}, nil
	}
	if addedAfter == "" && addedBefore == "" {
		return nativeLikeScope{}, errors.New("choose a native-like scope: --import-report, --added-after/--added-before, or --all-native")
	}

	scope := nativeLikeScope{}
	if addedAfter != "" {
		t, err := parseTimeValue(addedAfter)
		if err != nil {
			return nativeLikeScope{}, fmt.Errorf("--added-after: %w", err)
		}
		scope.Since = &t
	}
	if addedBefore != "" {
		t, err := parseTimeValue(addedBefore)
		if err != nil {
			return nativeLikeScope{}, fmt.Errorf("--added-before: %w", err)
		}
		scope.Until = &t
	}
	if scope.Since != nil && scope.Until != nil && scope.Since.After(*scope.Until) {
		return nativeLikeScope{}, errors.New("--added-after is after --added-before")
	}
	switch {
	case scope.Since != nil && scope.Until != nil:
		scope.Description = fmt.Sprintf("native likes added %s — %s",
			scope.Since.Format(time.RFC3339), scope.Until.Format(time.RFC3339))
	case scope.Since != nil:
		scope.Description = fmt.Sprintf("native likes added after %s", scope.Since.Format(time.RFC3339))
	default:
		scope.Description = fmt.Sprintf("native likes added before %s", scope.Until.Format(time.RFC3339))
	}
	return scope, nil
}

type transferWindow struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	DryRun     bool      `json:"dry_run"`
}

func readTransferWindow(path string) (transferWindow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return transferWindow{}, fmt.Errorf("read import report %s: %w", path, err)
	}
	var window transferWindow
	if err := json.Unmarshal(data, &window); err != nil {
		return transferWindow{}, fmt.Errorf("parse import report %s: %w", path, err)
	}
	if window.DryRun {
		return transferWindow{}, fmt.Errorf("import report %s is from a dry run", path)
	}
	if window.StartedAt.IsZero() || window.FinishedAt.IsZero() {
		return transferWindow{}, fmt.Errorf("import report %s has no started_at/finished_at", path)
	}
	if window.StartedAt.After(window.FinishedAt) {
		return transferWindow{}, fmt.Errorf("import report %s has an invalid time window", path)
	}
	return window, nil
}

func parseTimeValue(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("parse time %q: use RFC3339, e.g. 2026-11-25T19:00:00Z", value)
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
