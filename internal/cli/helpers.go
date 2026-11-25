package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zxbass/music-syncer/internal/config"
	"github.com/zxbass/music-syncer/internal/domain"
	"github.com/zxbass/music-syncer/internal/matcher"
	"github.com/zxbass/music-syncer/internal/spotify"
	"github.com/zxbass/music-syncer/internal/store"
	"github.com/zxbass/music-syncer/internal/yandex"
)

func (a *App) saveConfig() error {
	if a.cfgPath == "" {
		return errors.New("config path is not initialized")
	}
	return config.Save(a.cfgPath, a.cfg)
}

func (a *App) statePath() string {
	if p := strings.TrimSpace(os.Getenv("MUSIC_SYNCER_STATE")); p != "" {
		return p
	}
	return filepath.Join(config.Dir(a.cfgPath), "state.json")
}

func (a *App) openStore() (*store.Store, error) {
	return store.Open(a.statePath())
}

func (a *App) newMatcher() *matcher.Matcher {
	threshold := config.DefaultMatchThreshold
	if a.cfg != nil && a.cfg.Match.Threshold > 0 {
		threshold = a.cfg.Match.Threshold
	}
	m := matcher.New(threshold)
	if a.cfg != nil && a.cfg.Match.DurationToleranceMS > 0 {
		m.DurationOK = a.cfg.Match.DurationToleranceMS
	}
	return m
}

func (a *App) newSpotifyClient() *spotify.Client {
	token := spotify.Token{
		AccessToken:  a.cfg.Spotify.AccessToken,
		RefreshToken: a.cfg.Spotify.RefreshToken,
		Expiry:       a.cfg.Spotify.Expiry,
	}
	return spotify.New(a.cfg.Spotify.ClientID, token,
		spotify.WithLogger(a.log),
		spotify.WithTokenUpdate(a.persistSpotifyToken),
	)
}

func (a *App) persistSpotifyToken(token spotify.Token) {
	a.cfg.Spotify.AccessToken = token.AccessToken
	a.cfg.Spotify.RefreshToken = token.RefreshToken
	a.cfg.Spotify.Expiry = token.Expiry
	if err := a.saveConfig(); err != nil {
		a.log.Error("could not persist refreshed Spotify token", "error", err)
	}
}

func (a *App) openProvider(name domain.Name) (domain.Provider, error) {
	switch name {
	case domain.Yandex:
		if !a.cfg.Yandex.Connected() {
			return nil, errors.New("Yandex Music is not connected; run `music-syncer auth yandex`")
		}
		client := yandex.New(a.cfg.Yandex.Token, yandex.WithLogger(a.log))
		return yandex.NewProvider(client, a.cfg.Yandex.UID, a.log), nil
	case domain.Spotify:
		if !a.cfg.Spotify.Connected() {
			return nil, errors.New("Spotify is not connected; run `music-syncer auth spotify`")
		}
		return spotify.NewProvider(a.newSpotifyClient(), a.log), nil
	default:
		return nil, fmt.Errorf("unknown provider %q", name)
	}
}

func (a *App) connectedProviders() []domain.Name {
	var names []domain.Name
	if a.cfg.Yandex.Connected() {
		names = append(names, domain.Yandex)
	}
	if a.cfg.Spotify.Connected() {
		names = append(names, domain.Spotify)
	}
	return names
}

// providerFromFlag opens the provider named by flag. When flag is empty and
// exactly one provider is connected, that provider is used.
func (a *App) providerFromFlag(flag string) (domain.Provider, error) {
	name, err := a.providerNameFromFlag(flag)
	if err != nil {
		return nil, err
	}
	return a.openProvider(name)
}

func (a *App) providerNameFromFlag(flag string) (domain.Name, error) {
	flag = strings.TrimSpace(flag)
	if flag == "" {
		connected := a.connectedProviders()
		switch len(connected) {
		case 0:
			return "", errors.New("no provider is connected; run `music-syncer auth` first")
		case 1:
			return connected[0], nil
		default:
			return "", errors.New("multiple providers are connected; specify --provider yandex or --provider spotify")
		}
	}
	return domain.ParseName(flag)
}

func (a *App) transferProviders(from, to string) (domain.Provider, domain.Provider, error) {
	var fromName, toName domain.Name
	var err error
	if from != "" {
		if fromName, err = domain.ParseName(from); err != nil {
			return nil, nil, err
		}
	}
	if to != "" {
		if toName, err = domain.ParseName(to); err != nil {
			return nil, nil, err
		}
	}
	if fromName == "" && toName == "" {
		// Default to the main use case when both accounts are connected.
		if a.cfg.Yandex.Connected() && a.cfg.Spotify.Connected() {
			fromName, toName = domain.Yandex, domain.Spotify
		} else {
			return nil, nil, errors.New("specify --from and --to (for example: --from yandex --to spotify)")
		}
	} else if fromName == "" {
		fromName = otherProvider(toName)
	} else if toName == "" {
		toName = otherProvider(fromName)
	}
	if fromName == toName {
		return nil, nil, errors.New("--from and --to must be different")
	}
	if fromName == domain.Yandex && !a.cfg.Yandex.Connected() {
		return nil, nil, errors.New("Yandex Music is not connected; run `music-syncer auth yandex`")
	}
	if fromName == domain.Spotify && !a.cfg.Spotify.Connected() {
		return nil, nil, errors.New("Spotify is not connected; run `music-syncer auth spotify`")
	}
	if toName == domain.Yandex && !a.cfg.Yandex.Connected() {
		return nil, nil, errors.New("Yandex Music is not connected; run `music-syncer auth yandex`")
	}
	if toName == domain.Spotify && !a.cfg.Spotify.Connected() {
		return nil, nil, errors.New("Spotify is not connected; run `music-syncer auth spotify`")
	}

	src, err := a.openProvider(fromName)
	if err != nil {
		return nil, nil, err
	}
	dst, err := a.openProvider(toName)
	if err != nil {
		return nil, nil, err
	}
	return src, dst, nil
}

func otherProvider(n domain.Name) domain.Name {
	if n == domain.Yandex {
		return domain.Spotify
	}
	return domain.Yandex
}

// identify validates an account and refreshes cached account metadata.
func (a *App) identify(ctx context.Context, p domain.Provider) (domain.Account, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	acc, err := p.Account(ctx)
	if err != nil {
		return domain.Account{}, err
	}
	changed := false
	switch acc.Provider {
	case domain.Yandex:
		if a.cfg.Yandex.UID != acc.ID {
			a.cfg.Yandex.UID = acc.ID
			changed = true
		}
		if acc.DisplayName != "" && a.cfg.Yandex.DisplayName != acc.DisplayName {
			a.cfg.Yandex.DisplayName = acc.DisplayName
			changed = true
		}
	case domain.Spotify:
		if acc.ID != "" && a.cfg.Spotify.UserID != acc.ID {
			a.cfg.Spotify.UserID = acc.ID
			changed = true
		}
		if acc.DisplayName != "" && a.cfg.Spotify.DisplayName != acc.DisplayName {
			a.cfg.Spotify.DisplayName = acc.DisplayName
			changed = true
		}
	}
	if changed {
		if err := a.saveConfig(); err != nil {
			a.log.Warn("could not update cached account info", "error", err)
		}
	}
	return acc, nil
}

func parseSince(value string) (time.Time, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return time.Time{}, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", value, time.Local); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}
	unit := value[len(value)-1]
	if unit == 'd' || unit == 'w' || unit == 'm' {
		n, err := strconv.Atoi(value[:len(value)-1])
		if err == nil && n >= 0 {
			switch unit {
			case 'd':
				return time.Now().AddDate(0, 0, -n), nil
			case 'w':
				return time.Now().AddDate(0, 0, -7*n), nil
			case 'm':
				return time.Now().AddDate(0, -n, 0), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("invalid date %q (use YYYY-MM-DD or a relative value such as 30d, 4w, 6m)", value)
}

func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}

func formatDuration(ms int) string {
	if ms <= 0 {
		return "?"
	}
	seconds := ms / 1000
	return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
}

func printAccount(w io.Writer, acc domain.Account) {
	name := acc.DisplayName
	if name == "" {
		name = acc.Login
	}
	if name == "" {
		name = acc.ID
	}
	fmt.Fprintf(w, "%s: %s (id %s)\n", acc.Provider.DisplayName(), name, acc.ID)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
