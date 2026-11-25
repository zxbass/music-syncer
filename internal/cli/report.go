package cli

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/zxbass/music-syncer/internal/domain"
	"github.com/zxbass/music-syncer/internal/matcher"
	"github.com/zxbass/music-syncer/internal/syncer"
)

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
	fmt.Fprintln(tw, "\nPLAYLIST\tTRACKS\tADDED\tEXISTING\tUNMATCHED\tDEFERRED\tREMOVED\tSTATUS")
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
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n",
			name, pl.Stats.Source, pl.Stats.Added, pl.Stats.Existing, pl.Stats.Unmatched,
			pl.Stats.Deferred, pl.Removed, status)
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
		playlist := ""
		if report.LikesPlaylist != nil {
			playlist = fmt.Sprintf(" → %q", report.LikesPlaylist.TargetName)
			switch {
			case report.LikesPlaylist.Created:
				playlist += " (created)"
			case report.LikesPlaylist.WouldCreate:
				playlist += " (would create)"
			}
		}
		fmt.Fprintf(w, "\nLiked tracks%s: %d source, %d added, %d already present, %d unmatched, %d deferred, %d failed\n",
			playlist, report.Likes.Source, report.Likes.Added, report.Likes.Existing,
			report.Likes.Unmatched, report.Likes.Deferred, report.Likes.Failed)
	}
	if report.Artists != nil {
		fmt.Fprintf(w, "Artists: %d source, %d followed, %d already followed, %d unmatched, %d deferred, %d failed\n",
			report.Artists.Source, report.Artists.Added, report.Artists.Existing,
			report.Artists.Unmatched, report.Artists.Deferred, report.Artists.Failed)
	}

	if report.BatchLimitReached {
		fmt.Fprintf(w, "\nSearch batch limit reached (%d new catalog lookups). Progress is saved; rerun the same command to continue.\n",
			report.SearchBatch)
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
	return result
}

func versionNote(c matcher.Candidate) string {
	if c.StrongVersionConflict {
		return "(different version)"
	}
	return ""
}
