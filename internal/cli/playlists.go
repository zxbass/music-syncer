package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func (a *App) playlistsCmd() *cobra.Command {
	var (
		providerFlag string
		jsonOut      bool
	)
	cmd := &cobra.Command{
		Use:   "playlists",
		Short: "List playlists of a connected provider",
		Example: `  music-syncer playlists --provider yandex
  music-syncer playlists --provider spotify --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			provider, err := a.providerFromFlag(providerFlag)
			if err != nil {
				return err
			}
			if _, err := a.identify(cmd.Context(), provider); err != nil {
				return err
			}
			playlists, err := provider.Playlists(cmd.Context())
			if err != nil {
				return err
			}
			if jsonOut {
				data, err := json.MarshalIndent(playlists, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(a.out, string(data))
				return nil
			}
			if len(playlists) == 0 {
				fmt.Fprintln(a.out, "No playlists found.")
				return nil
			}
			tw := tabwriter.NewWriter(a.out, 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tKIND\tTRACKS\tNAME\tFLAGS")
			for _, pl := range playlists {
				flags := ""
				if pl.Generated {
					flags += "generated "
				}
				if pl.ReadOnly {
					flags += "read-only "
				}
				if pl.Special != "" {
					flags += pl.Special
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", pl.ID, pl.Kind, pl.TrackCount, pl.Name, flags)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&providerFlag, "provider", "", "provider: yandex or spotify (inferred if only one is connected)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print JSON")
	return cmd
}
