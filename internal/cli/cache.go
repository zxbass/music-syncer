package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func (a *App) cacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect or clear the match cache",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "stats",
			Short: "Show cached matches and playlist links",
			RunE: func(cmd *cobra.Command, args []string) error {
				store, err := a.openStore()
				if err != nil {
					return err
				}
				stats := store.Stats()
				fmt.Fprintf(a.out, "State file: %s\n", store.Path())
				fmt.Fprintf(a.out, "  track matches:   %d\n", stats.Matches)
				fmt.Fprintf(a.out, "  playlist links:  %d\n", stats.Playlists)
				fmt.Fprintf(a.out, "  artist links:    %d\n", stats.Artists)
				return nil
			},
		},
		&cobra.Command{
			Use:   "clear",
			Short: "Delete cached matches and links",
			Long: `Delete the local state file. The next transfer will search for every
track again and may create new playlists if names no longer disambiguate the
previous target playlists. Playlists and likes on both services are not
touched.`,
			RunE: func(cmd *cobra.Command, args []string) error {
				store, err := a.openStore()
				if err != nil {
					return err
				}
				if err := store.Clear(); err != nil {
					return err
				}
				fmt.Fprintln(a.out, "Match cache cleared.")
				return nil
			},
		},
	)
	return cmd
}
