package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/zxbass/music-syncer/internal/domain"
	"github.com/zxbass/music-syncer/internal/syncer"
)

type playlistExport struct {
	Playlist domain.Playlist `json:"playlist"`
	Tracks   []domain.Track  `json:"tracks,omitempty"`
}

type exportSummary struct {
	Provider      string    `json:"provider"`
	AccountID     string    `json:"account_id"`
	AccountName   string    `json:"account_name,omitempty"`
	ExportedAt    time.Time `json:"exported_at"`
	Categories    string    `json:"categories"`
	Playlists     int       `json:"playlists"`
	Tracks        int       `json:"tracks"`
	LikedTracks   int       `json:"liked_tracks"`
	Artists       int       `json:"artists"`
	HistoryEvents int       `json:"history_events"`
	Notes         []string  `json:"notes,omitempty"`
}

func (a *App) exportCmd() *cobra.Command {
	var (
		providerFlag string
		only         string
		output       string
		limit        int
		historySince string
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export a library to JSON files",
		Long: `Export playlists, liked tracks, followed artists and listening history
to a local directory. The result is a readable backup and can be used to
inspect what a later transfer would see.`,
		Example: `  music-syncer export --provider yandex --output yandex-backup
  music-syncer export --provider spotify --only likes,artists --output spotify-backup`,
		RunE: func(cmd *cobra.Command, args []string) error {
			categories, err := syncer.ParseCategories(only)
			if err != nil {
				return err
			}
			if limit < 0 {
				return errors.New("--limit cannot be negative")
			}
			since, err := parseSince(historySince)
			if err != nil {
				return err
			}
			provider, err := a.providerFromFlag(providerFlag)
			if err != nil {
				return err
			}
			acc, err := a.identify(cmd.Context(), provider)
			if err != nil {
				return fmt.Errorf("check account: %w", err)
			}

			if output == "" {
				output = fmt.Sprintf("export-%s-%s", provider.Name(), time.Now().Format("20060102-150405"))
			}
			if info, err := os.Stat(output); err == nil && !info.IsDir() {
				return fmt.Errorf("output %s exists and is not a directory", output)
			}
			if err := os.MkdirAll(output, 0o755); err != nil {
				return fmt.Errorf("create output directory: %w", err)
			}

			summary := exportSummary{
				Provider:    string(provider.Name()),
				AccountID:   acc.ID,
				AccountName: acc.DisplayName,
				ExportedAt:  time.Now().UTC(),
				Categories:  categories.String(),
			}
			if summary.AccountName == "" {
				summary.AccountName = acc.Login
			}
			if provider.Name() == domain.Spotify {
				summary.Notes = append(summary.Notes,
					"Spotify exposes at most the last 50 listening history events through its API.")
			}

			if categories.Playlists {
				playlists, err := provider.Playlists(cmd.Context())
				if err != nil {
					return fmt.Errorf("load playlists: %w", err)
				}
				exports := make([]playlistExport, 0, len(playlists))
				skipped := 0
				for _, pl := range playlists {
					if pl.ReadOnly {
						exports = append(exports, playlistExport{Playlist: pl})
						summary.Playlists++
						skipped++
						continue
					}
					tracks, err := provider.PlaylistTracks(cmd.Context(), pl)
					if err != nil {
						return fmt.Errorf("load playlist %q: %w", pl.Name, err)
					}
					if limit > 0 && len(tracks) > limit {
						tracks = tracks[:limit]
					}
					exports = append(exports, playlistExport{Playlist: pl, Tracks: tracks})
					summary.Playlists++
					summary.Tracks += len(tracks)
					a.log.Info("exported playlist", "name", pl.Name, "tracks", len(tracks))
				}
				if skipped > 0 {
					summary.Notes = append(summary.Notes,
						fmt.Sprintf("%d read-only playlist(s) were exported without tracks", skipped))
				}
				if err := writeJSONFile(filepath.Join(output, "playlists.json"), exports); err != nil {
					return err
				}
			}

			if categories.Likes {
				if !provider.Capabilities().LikedTracks {
					return fmt.Errorf("%w: liked tracks", domain.ErrNotSupported)
				}
				tracks, err := provider.LikedTracks(cmd.Context(), limit)
				if err != nil {
					return fmt.Errorf("load liked tracks: %w", err)
				}
				if err := writeJSONFile(filepath.Join(output, "liked-tracks.json"), tracks); err != nil {
					return err
				}
				summary.LikedTracks = len(tracks)
			}

			if categories.Artists {
				if !provider.Capabilities().FollowedArtist {
					return fmt.Errorf("%w: followed artists", domain.ErrNotSupported)
				}
				artists, err := provider.LikedArtists(cmd.Context(), limit)
				if err != nil {
					return fmt.Errorf("load artists: %w", err)
				}
				if err := writeJSONFile(filepath.Join(output, "artists.json"), artists); err != nil {
					return err
				}
				summary.Artists = len(artists)
			}

			if categories.History {
				if !provider.Capabilities().HistoryRead {
					return fmt.Errorf("%w: listening history", domain.ErrNotSupported)
				}
				events, err := provider.History(cmd.Context(), since, limit)
				if err != nil {
					return fmt.Errorf("load history: %w", err)
				}
				if err := writeJSONFile(filepath.Join(output, "history.json"), events); err != nil {
					return err
				}
				summary.HistoryEvents = len(events)
			}

			if err := writeJSONFile(filepath.Join(output, "summary.json"), summary); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "Exported %s to %s\n", provider.Name().DisplayName(), output)
			fmt.Fprintf(a.out, "  playlists:     %d (%d tracks)\n", summary.Playlists, summary.Tracks)
			fmt.Fprintf(a.out, "  liked tracks:  %d\n", summary.LikedTracks)
			fmt.Fprintf(a.out, "  artists:       %d\n", summary.Artists)
			fmt.Fprintf(a.out, "  history:       %d events\n", summary.HistoryEvents)
			for _, note := range summary.Notes {
				fmt.Fprintf(a.out, "  note: %s\n", note)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&providerFlag, "provider", "", "provider: yandex or spotify (inferred if only one is connected)")
	cmd.Flags().StringVar(&only, "only", "all", "comma-separated categories: playlists,likes,artists,history,all")
	cmd.Flags().StringVar(&output, "output", "", "output directory (default export-<provider>-<timestamp>)")
	cmd.Flags().IntVar(&limit, "limit", 0, "limit tracks per collection (0 = no limit)")
	cmd.Flags().StringVar(&historySince, "history-since", "", "history window: YYYY-MM-DD or 30d/4w/6m")
	return cmd
}
