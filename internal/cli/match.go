package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/zxbass/music-syncer/internal/domain"
)

func (a *App) matchCmd() *cobra.Command {
	var (
		toFlag    string
		fromFlag  string
		title     string
		artists   []string
		album     string
		duration  int
		isrc      string
		limit     int
		jsonOut   bool
		threshold float64
	)
	cmd := &cobra.Command{
		Use:   "match",
		Short: "Test cross-service track matching without writing anything",
		Long: `Search the target catalog for a track and print the matcher scores.
This is useful for understanding why a track was not transferred, or for
tuning the match threshold in the config file.`,
		Example: `  music-syncer match --to spotify --title "Кино" --artist "Виктор Цой" --duration 265
  music-syncer match --to yandex --title "Blinding Lights" --artist "The Weeknd" --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if title == "" {
				return errors.New("--title is required")
			}
			if duration < 0 {
				return errors.New("--duration cannot be negative")
			}
			target, err := a.providerFromFlag(toFlag)
			if err != nil {
				return err
			}
			srcProvider := domain.Name("manual")
			if fromFlag != "" {
				srcProvider, err = domain.ParseName(fromFlag)
				if err != nil {
					return err
				}
			}
			src := domain.Track{
				Provider:   srcProvider,
				Title:      title,
				Artists:    artists,
				Album:      album,
				DurationMS: duration * 1000,
				ISRC:       isrc,
			}
			if limit <= 0 {
				limit = 10
			}
			candidates, err := target.SearchTracks(cmd.Context(), src, limit)
			if err != nil {
				return err
			}
			m := a.newMatcher()
			if threshold != 0 {
				if threshold < 0 || threshold > 1 {
					return errors.New("--threshold must be between 0 and 1")
				}
				m.Threshold = threshold
			}
			ranked := m.Rank(src, candidates)
			if jsonOut {
				data, err := json.MarshalIndent(ranked, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(a.out, string(data))
				return nil
			}
			if len(ranked) == 0 {
				fmt.Fprintln(a.out, "No candidates found.")
				return nil
			}
			tw := tabwriter.NewWriter(a.out, 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "#\tSCORE\tTITLE\tARTIST\tALBUM\tDURATION\tID\tACCEPT")
			for i, c := range ranked {
				accepted := ""
				if m.Accept(c, src) {
					accepted = "yes"
				}
				fmt.Fprintf(tw, "%d\t%.3f\t%s\t%s\t%s\t%s\t%s\t%s\n",
					i+1, c.Score, c.Track.Title, c.Track.ArtistLine(), c.Track.Album,
					formatDuration(c.Track.DurationMS), c.Track.ID, accepted)
			}
			_ = tw.Flush()
			best := ranked[0]
			if m.Accept(best, src) {
				fmt.Fprintf(a.out, "\nSelected: %s (confidence %.0f%%)\n", best.Track.String(), best.Score*100)
			} else {
				fmt.Fprintf(a.out, "\nNo acceptable match (best %.0f%% < threshold %.0f%%).\n",
					best.Score*100, m.Threshold*100)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&toFlag, "to", "", "target provider: yandex or spotify")
	cmd.Flags().StringVar(&fromFlag, "from", "", "optional source provider label")
	cmd.Flags().StringVar(&title, "title", "", "track title to search for")
	cmd.Flags().StringArrayVar(&artists, "artist", nil, "artist name (repeatable)")
	cmd.Flags().StringVar(&album, "album", "", "album name (used as a small tie-breaker)")
	cmd.Flags().IntVar(&duration, "duration", 0, "track duration in seconds")
	cmd.Flags().StringVar(&isrc, "isrc", "", "ISRC (tried first when supported)")
	cmd.Flags().IntVar(&limit, "limit", 10, "number of candidates to request")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print JSON")
	cmd.Flags().Float64Var(&threshold, "threshold", 0, "override the match threshold (0.0-1.0)")
	return cmd
}
