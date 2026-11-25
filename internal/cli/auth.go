package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zxbass/music-syncer/internal/domain"
	"github.com/zxbass/music-syncer/internal/spotify"
	"github.com/zxbass/music-syncer/internal/yandex"
)

func (a *App) authCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Connect and inspect music accounts",
	}
	cmd.AddCommand(a.authYandexCmd(), a.authSpotifyCmd(), a.authStatusCmd(), a.authLogoutCmd())
	return cmd
}

func (a *App) authYandexCmd() *cobra.Command {
	var tokenFlag string
	cmd := &cobra.Command{
		Use:   "yandex",
		Short: "Connect Yandex Music (device flow or a pasted token)",
		Long: `Connect Yandex Music.

By default music-syncer starts the Yandex OAuth device flow: it prints a URL
and a short code, and waits for you to confirm the login on any device.

You can also pass an existing Yandex Music token with --token or the
YANDEX_MUSIC_TOKEN environment variable. This is an unofficial API; use it at
your own risk.`,
		Example: `  music-syncer auth yandex
  music-syncer auth yandex --token y0_Ag...`,
		RunE: func(cmd *cobra.Command, args []string) error {
			token := strings.TrimSpace(tokenFlag)
			if token == "" {
				token = strings.TrimSpace(os.Getenv("YANDEX_MUSIC_TOKEN"))
			}
			if token == "" {
				ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
				defer cancel()
				var err error
				token, err = yandex.LoginDevice(ctx, yandex.DeviceAuthConfig{
					In:  cmd.InOrStdin(),
					Out: a.out,
				})
				if err != nil {
					return err
				}
			}

			client := yandex.New(token, yandex.WithLogger(a.log))
			provider := yandex.NewProvider(client, "", a.log)
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			acc, err := provider.Account(ctx)
			if err != nil {
				return fmt.Errorf("validate Yandex Music token: %w", err)
			}

			a.cfg.Yandex.Token = token
			a.cfg.Yandex.UID = acc.ID
			a.cfg.Yandex.DisplayName = acc.DisplayName
			if err := a.saveConfig(); err != nil {
				return err
			}
			fmt.Fprintln(a.out, "Connected.")
			printAccount(a.out, acc)
			fmt.Fprintf(a.out, "Token stored in %s (mode 0600).\n", a.cfgPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&tokenFlag, "token", "", "existing Yandex Music OAuth token")
	return cmd
}

func (a *App) authSpotifyCmd() *cobra.Command {
	var (
		clientID    string
		redirectURI string
		manual      bool
		noBrowser   bool
	)
	cmd := &cobra.Command{
		Use:   "spotify",
		Short: "Connect Spotify with OAuth (PKCE)",
		Long: `Connect Spotify using OAuth Authorization Code with PKCE.

Create an app at https://developer.spotify.com/dashboard and add this exact
redirect URI to its settings:

  http://127.0.0.1:8085/callback

Then run this command with the app's client ID. A client secret is not needed.
Development-mode Spotify apps have restrictions (the app owner needs Premium,
and some endpoints have lower limits); see the README for details.`,
		Example: `  music-syncer auth spotify --client-id 0123456789abcdef
  music-syncer auth spotify --client-id 0123456789abcdef --manual`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(clientID) == "" {
				clientID = a.cfg.Spotify.ClientID
			}
			clientID = strings.TrimSpace(clientID)
			if clientID == "" {
				return errors.New("Spotify client ID is required.\n\n" +
					"1. Create an app at https://developer.spotify.com/dashboard\n" +
					"2. Add redirect URI " + spotify.DefaultRedirectURI + "\n" +
					"3. Run: music-syncer auth spotify --client-id YOUR_CLIENT_ID")
			}
			if redirectURI == "" {
				redirectURI = spotify.DefaultRedirectURI
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
			defer cancel()
			token, err := spotify.Login(ctx, spotify.AuthConfig{
				ClientID:    clientID,
				RedirectURI: redirectURI,
				Manual:      manual,
				NoBrowser:   noBrowser,
				In:          cmd.InOrStdin(),
				Out:         a.out,
			})
			if err != nil {
				return err
			}

			client := spotify.New(clientID, token, spotify.WithLogger(a.log))
			provider := spotify.NewProvider(client, a.log)
			acc, err := provider.Account(ctx)
			if err != nil {
				return fmt.Errorf("validate Spotify account: %w", err)
			}

			a.cfg.Spotify.ClientID = clientID
			a.cfg.Spotify.AccessToken = token.AccessToken
			a.cfg.Spotify.RefreshToken = token.RefreshToken
			a.cfg.Spotify.Expiry = token.Expiry
			a.cfg.Spotify.UserID = acc.ID
			a.cfg.Spotify.DisplayName = acc.DisplayName
			if err := a.saveConfig(); err != nil {
				return err
			}
			fmt.Fprintln(a.out, "Connected.")
			printAccount(a.out, acc)
			fmt.Fprintf(a.out, "Tokens stored in %s (mode 0600).\n", a.cfgPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&clientID, "client-id", "", "Spotify app client ID")
	cmd.Flags().StringVar(&redirectURI, "redirect-uri", "", "OAuth redirect URI (default "+spotify.DefaultRedirectURI+")")
	cmd.Flags().BoolVar(&manual, "manual", false, "print the URL and paste the callback URL manually (for SSH/headless hosts)")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "do not open a browser automatically")
	return cmd
}

func (a *App) authStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show connected accounts",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			for _, name := range []domain.Name{domain.Yandex, domain.Spotify} {
				switch name {
				case domain.Yandex:
					if !a.cfg.Yandex.Connected() {
						fmt.Fprintln(a.out, "Yandex Music: not connected (run `music-syncer auth yandex`)")
						continue
					}
				case domain.Spotify:
					if !a.cfg.Spotify.Connected() {
						fmt.Fprintln(a.out, "Spotify: not connected (run `music-syncer auth spotify`)")
						continue
					}
				}
				provider, err := a.openProvider(name)
				if err != nil {
					fmt.Fprintf(a.out, "%s: error: %v\n", name.DisplayName(), err)
					continue
				}
				acc, err := a.identify(ctx, provider)
				if err != nil {
					fmt.Fprintf(a.out, "%s: error: %v\n", name.DisplayName(), err)
					continue
				}
				printAccount(a.out, acc)
			}
			return nil
		},
	}
}

func (a *App) authLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout <yandex|spotify>",
		Short: "Remove stored credentials for a provider",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := domain.ParseName(args[0])
			if err != nil {
				return err
			}
			switch name {
			case domain.Yandex:
				a.cfg.Yandex.Token = ""
				a.cfg.Yandex.UID = ""
				a.cfg.Yandex.DisplayName = ""
			case domain.Spotify:
				a.cfg.Spotify.AccessToken = ""
				a.cfg.Spotify.RefreshToken = ""
				a.cfg.Spotify.Expiry = time.Time{}
				a.cfg.Spotify.UserID = ""
				a.cfg.Spotify.DisplayName = ""
			}
			if err := a.saveConfig(); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "Disconnected %s.\n", name.DisplayName())
			return nil
		},
	}
}
