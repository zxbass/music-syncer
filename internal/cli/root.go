// Package cli implements the music-syncer command-line interface.
package cli

import (
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zxbass/music-syncer/internal/config"
)

// App holds shared command state.
type App struct {
	cfgPath string
	cfg     *config.Config
	log     *slog.Logger
	out     io.Writer
	errOut  io.Writer
	verbose bool
}

// NewRootCmd builds the command tree.
func NewRootCmd() *cobra.Command {
	app := &App{}
	root := &cobra.Command{
		Use:   "music-syncer",
		Short: "Transfer playlists, liked tracks and artists between Yandex Music and Spotify",
		Long: `music-syncer transfers a music library between Yandex Music and Spotify.

It can move playlists, liked tracks and followed artists in either direction.
Cross-service matching is done by title, artist, duration and ISRC, with a
conservative confidence threshold.

Yandex Music uses an unofficial API and a personal OAuth token. Spotify uses
the official Web API with PKCE.`,
		Example: `  music-syncer auth yandex
  music-syncer auth spotify --client-id YOUR_SPOTIFY_CLIENT_ID
  music-syncer transfer --from yandex --to spotify --dry-run
  music-syncer transfer --from yandex --to spotify --only playlists,likes,artists
  music-syncer transfer --from spotify --to yandex --only likes
  music-syncer migrate-likes --dry-run
  music-syncer playlists --provider spotify
  music-syncer export --provider yandex --output yandex-backup`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			path := strings.TrimSpace(app.cfgPath)
			if path == "" {
				var err error
				path, err = config.DefaultPath()
				if err != nil {
					return err
				}
			}
			app.cfgPath = path
			cfg, err := config.Load(path)
			if err != nil {
				if !isConfigOptional(cmd) {
					return err
				}
				cmd.PrintErrln("warning: ignoring invalid config:", err)
				cfg = config.Default()
			}
			app.cfg = cfg
			app.out = cmd.OutOrStdout()
			app.errOut = cmd.ErrOrStderr()
			level := slog.LevelInfo
			if app.verbose {
				level = slog.LevelDebug
			}
			handler := slog.NewTextHandler(app.errOut, &slog.HandlerOptions{
				Level: level,
				ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
					if attr.Key == slog.TimeKey {
						return slog.Attr{}
					}
					return attr
				},
			})
			app.log = slog.New(handler)
			if err := config.WarnIfPermissionsAreLoose(path); err != nil {
				// The file may not exist yet; only warn when it does.
				if _, statErr := os.Stat(path); statErr == nil {
					cmd.PrintErrln("warning:", err)
				}
			}
			return nil
		},
	}

	root.PersistentFlags().StringVar(&app.cfgPath, "config", "", "config file (default: $XDG_CONFIG_HOME/music-syncer/config.yaml)")
	root.PersistentFlags().BoolVar(&app.verbose, "verbose", false, "enable debug logging")

	root.AddCommand(
		app.authCmd(),
		app.transferCmd(),
		app.migrateLikesCmd(),
		app.playlistsCmd(),
		app.exportCmd(),
		app.matchCmd(),
		app.cacheCmd(),
		app.versionCmd(),
	)
	return root
}

func isConfigOptional(cmd *cobra.Command) bool {
	switch cmd.CommandPath() {
	case "music-syncer version", "music-syncer cache clear":
		return true
	default:
		return false
	}
}

// Execute runs the CLI.
func Execute() error {
	return NewRootCmd().Execute()
}
