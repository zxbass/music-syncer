package spotify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/zxbass/music-syncer/internal/domain"
)

const (
	pageSize         = 50
	libraryBatchSize = 40 // Spotify's documented limit for PUT /me/library
)

// Provider adapts the Spotify client to domain.Provider.
type Provider struct {
	client *Client
	log    *slog.Logger

	mu     sync.Mutex
	selfID string
}

// NewProvider creates a Spotify provider.
func NewProvider(client *Client, logger *slog.Logger) *Provider {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Provider{client: client, log: logger}
}

// Name implements domain.Provider.
func (p *Provider) Name() domain.Name { return domain.Spotify }

// Capabilities implements domain.Provider.
func (p *Provider) Capabilities() domain.Capabilities {
	return domain.Capabilities{
		Playlists:      true,
		LikedTracks:    true,
		FollowedArtist: true,
	}
}

// Account implements domain.Provider.
func (p *Provider) Account(ctx context.Context) (domain.Account, error) {
	var u user
	if err := p.client.Get(ctx, "/me", nil, &u); err != nil {
		return domain.Account{}, err
	}
	return domain.Account{
		Provider:    domain.Spotify,
		ID:          u.ID,
		DisplayName: u.DisplayName,
		Email:       u.Email,
	}, nil
}

// Playlists implements domain.Provider.
func (p *Provider) Playlists(ctx context.Context) ([]domain.Playlist, error) {
	selfID, err := p.currentUserID(ctx)
	if err != nil {
		return nil, err
	}
	var result []domain.Playlist
	offset := 0
	for {
		q := url.Values{
			"limit":  {strconv.Itoa(pageSize)},
			"offset": {strconv.Itoa(offset)},
		}
		var page playlistPage
		if err := p.client.Get(ctx, "/me/playlists", q, &page); err != nil {
			return nil, err
		}
		for _, pl := range page.Items {
			result = append(result, p.toDomainPlaylist(pl, selfID))
		}
		offset += len(page.Items)
		if len(page.Items) == 0 || (page.Total > 0 && offset >= page.Total) {
			break
		}
	}
	return result, nil
}

func (p *Provider) currentUserID(ctx context.Context) (string, error) {
	p.mu.Lock()
	id := p.selfID
	p.mu.Unlock()
	if id != "" {
		return id, nil
	}
	var u user
	if err := p.client.Get(ctx, "/me", nil, &u); err != nil {
		return "", err
	}
	p.mu.Lock()
	p.selfID = u.ID
	p.mu.Unlock()
	return u.ID, nil
}

func (p *Provider) toDomainPlaylist(pl playlist, selfID string) domain.Playlist {
	out := domain.Playlist{
		Provider:    domain.Spotify,
		ID:          pl.ID,
		Name:        pl.Name,
		Description: pl.Description,
		TrackCount:  pl.trackCount(),
		URL:         pl.ExternalURLs["spotify"],
	}
	if pl.Owner != nil {
		out.OwnerID = pl.Owner.ID
		if pl.Owner.ID == "spotify" {
			out.Generated = true
		}
		if selfID != "" && pl.Owner.ID != selfID && !pl.Collaborative {
			out.ReadOnly = true
		}
	}
	return out
}

// PlaylistTracks implements domain.Provider.
func (p *Provider) PlaylistTracks(ctx context.Context, playlist domain.Playlist) ([]domain.Track, error) {
	if playlist.ID == "" {
		return nil, errors.New("spotify: playlist has no id")
	}
	newPath := "/playlists/" + url.PathEscape(playlist.ID) + "/items"
	oldPath := "/playlists/" + url.PathEscape(playlist.ID) + "/tracks"

	path := newPath
	useLegacy := false
	var result []domain.Track
	offset := 0
	for {
		q := url.Values{
			"limit":  {strconv.Itoa(pageSize)},
			"offset": {strconv.Itoa(offset)},
		}
		var page playlistItemsPage
		err := p.client.Get(ctx, path, q, &page)
		if err != nil && !useLegacy && isUnsupported(err) {
			useLegacy = true
			path = oldPath
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			t := item.value()
			if t == nil || t.IsLocal || t.ID == "" {
				continue
			}
			result = append(result, t.toDomain())
		}
		offset += len(page.Items)
		if len(page.Items) == 0 || (page.Total > 0 && offset >= page.Total) {
			break
		}
	}
	return result, nil
}

// LikedTracks implements domain.Provider.
func (p *Provider) LikedTracks(ctx context.Context, limit int) ([]domain.Track, error) {
	var result []domain.Track
	offset := 0
	for {
		q := url.Values{
			"limit":  {strconv.Itoa(pageSize)},
			"offset": {strconv.Itoa(offset)},
		}
		var page savedTracksPage
		if err := p.client.Get(ctx, "/me/tracks", q, &page); err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			t := item.value()
			if t == nil || t.IsLocal || t.ID == "" {
				continue
			}
			result = append(result, t.toDomain())
			if limit > 0 && len(result) >= limit {
				return result, nil
			}
		}
		offset += len(page.Items)
		if len(page.Items) == 0 || (page.Total > 0 && offset >= page.Total) {
			break
		}
	}
	return result, nil
}

// LikedArtists implements domain.Provider. Spotify calls this "followed
// artists", which is the closest equivalent of Yandex's liked artists.
func (p *Provider) LikedArtists(ctx context.Context, limit int) ([]domain.Artist, error) {
	var result []domain.Artist
	after := ""
	for {
		q := url.Values{
			"type":  {"artist"},
			"limit": {strconv.Itoa(pageSize)},
		}
		if after != "" {
			q.Set("after", after)
		}
		var page followedArtistsPage
		if err := p.client.Get(ctx, "/me/following", q, &page); err != nil {
			return nil, err
		}
		for _, a := range page.Artists.Items {
			if a.ID == "" {
				continue
			}
			result = append(result, domain.Artist{
				Provider: domain.Spotify,
				ID:       a.ID,
				Name:     a.Name,
				URL:      a.ExternalURLs["spotify"],
			})
			if limit > 0 && len(result) >= limit {
				return result, nil
			}
		}
		after = page.Artists.Cursors.After
		if after == "" || len(page.Artists.Items) == 0 {
			break
		}
	}
	return result, nil
}

// SearchTracks implements domain.Provider.
func (p *Provider) SearchTracks(ctx context.Context, track domain.Track, limit int) ([]domain.Track, error) {
	if track.Title == "" {
		return nil, nil
	}
	limit = clampLimit(limit, 10)

	// ISRC is a much stronger identifier than a free-text query, so try it
	// first when the source track has one. Spotify's search index accepts
	// isrc: queries for most catalog entries.
	if strings.TrimSpace(track.ISRC) != "" {
		candidates, err := p.searchTracks(ctx, "isrc:"+strings.TrimSpace(track.ISRC), limit)
		if err != nil {
			if domain.IsUnauthorized(err) {
				return nil, err
			}
			p.log.Debug("spotify ISRC lookup failed, falling back to text search", "error", err)
		} else {
			var exact []domain.Track
			for _, c := range candidates {
				if strings.EqualFold(c.ISRC, track.ISRC) {
					exact = append(exact, c)
				}
			}
			if len(exact) > 0 {
				return exact, nil
			}
		}
	}

	query := strings.TrimSpace(track.Title + " " + track.PrimaryArtist())
	candidates, err := p.searchTracks(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 && track.PrimaryArtist() != "" {
		candidates, err = p.searchTracks(ctx, track.Title, limit)
		if err != nil {
			return nil, err
		}
	}
	return candidates, nil
}

func (p *Provider) searchTracks(ctx context.Context, query string, limit int) ([]domain.Track, error) {
	q := url.Values{
		"q":     {sanitizeQuery(query)},
		"type":  {"track"},
		"limit": {strconv.Itoa(limit)},
	}
	var page searchTracksPage
	if err := p.client.Get(ctx, "/search", q, &page); err != nil {
		return nil, err
	}
	result := make([]domain.Track, 0, len(page.Tracks.Items))
	for _, t := range page.Tracks.Items {
		if t.ID == "" || t.IsLocal {
			continue
		}
		result = append(result, t.toDomain())
	}
	return result, nil
}

// SearchArtists implements domain.Provider.
func (p *Provider) SearchArtists(ctx context.Context, artist domain.Artist, limit int) ([]domain.Artist, error) {
	if strings.TrimSpace(artist.Name) == "" {
		return nil, nil
	}
	q := url.Values{
		"q":     {sanitizeQuery(artist.Name)},
		"type":  {"artist"},
		"limit": {strconv.Itoa(clampLimit(limit, 10))},
	}
	var page searchArtistsPage
	if err := p.client.Get(ctx, "/search", q, &page); err != nil {
		return nil, err
	}
	result := make([]domain.Artist, 0, len(page.Artists.Items))
	for _, a := range page.Artists.Items {
		if a.ID == "" {
			continue
		}
		result = append(result, domain.Artist{
			Provider: domain.Spotify,
			ID:       a.ID,
			Name:     a.Name,
			URL:      a.ExternalURLs["spotify"],
		})
	}
	return result, nil
}

// CreatePlaylist implements domain.Provider. Imported playlists are private by
// default so a transfer cannot accidentally publish a user's library.
func (p *Provider) CreatePlaylist(ctx context.Context, name, description string) (domain.Playlist, error) {
	body := map[string]any{
		"name":        name,
		"description": sanitizePlaylistDescription(description),
		"public":      false,
	}
	var pl playlist
	if err := p.client.Post(ctx, "/me/playlists", nil, body, &pl); err != nil {
		return domain.Playlist{}, err
	}
	return p.toDomainPlaylist(pl, p.selfID), nil
}

// AddTracks implements domain.Provider.
func (p *Provider) AddTracks(ctx context.Context, playlist domain.Playlist, tracks []domain.Track) error {
	if playlist.ID == "" {
		return errors.New("spotify: playlist has no id")
	}
	uris := make([]string, 0, len(tracks))
	for _, t := range tracks {
		if t.ID != "" {
			uris = append(uris, "spotify:track:"+t.ID)
		}
	}
	if len(uris) == 0 {
		return nil
	}
	for start := 0; start < len(uris); start += 100 {
		end := min(start+100, len(uris))
		body := map[string]any{"uris": uris[start:end]}
		err := p.client.Post(ctx, "/playlists/"+url.PathEscape(playlist.ID)+"/items", nil, body, nil)
		if isUnsupported(err) {
			err = p.client.Post(ctx, "/playlists/"+url.PathEscape(playlist.ID)+"/tracks", nil, body, nil)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// RemoveTracks implements domain.Provider.
func (p *Provider) RemoveTracks(ctx context.Context, playlist domain.Playlist, tracks []domain.Track) error {
	if playlist.ID == "" {
		return errors.New("spotify: playlist has no id")
	}
	seen := map[string]bool{}
	var uris []string
	for _, t := range tracks {
		if t.ID == "" || seen[t.ID] {
			continue
		}
		seen[t.ID] = true
		uris = append(uris, "spotify:track:"+t.ID)
	}
	if len(uris) == 0 {
		return nil
	}
	for start := 0; start < len(uris); start += 100 {
		end := min(start+100, len(uris))
		items := make([]map[string]string, 0, end-start)
		for _, uri := range uris[start:end] {
			items = append(items, map[string]string{"uri": uri})
		}
		err := p.client.Delete(ctx, "/playlists/"+url.PathEscape(playlist.ID)+"/items", nil,
			map[string]any{"items": items})
		if isUnsupported(err) {
			err = p.client.Delete(ctx, "/playlists/"+url.PathEscape(playlist.ID)+"/tracks", nil,
				map[string]any{"tracks": items})
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// LikeTracks implements domain.Provider. It uses the new generic library
// endpoint and falls back to the entity-specific one for older API access.
func (p *Provider) LikeTracks(ctx context.Context, tracks []domain.Track) error {
	var uris []string
	for _, t := range tracks {
		if t.ID != "" {
			uris = append(uris, "spotify:track:"+t.ID)
		}
	}
	return p.saveLibraryURIs(ctx, uris, func(batch []string) error {
		ids := make([]string, 0, len(batch))
		for _, uri := range batch {
			ids = append(ids, strings.TrimPrefix(uri, "spotify:track:"))
		}
		return p.client.Put(ctx, "/me/tracks", url.Values{"ids": {strings.Join(ids, ",")}}, nil)
	})
}

// UnlikeTracks implements domain.Provider. It removes tracks from the native
// Spotify library using the generic endpoint, with the entity-specific
// endpoint as a fallback for extended quota accounts.
func (p *Provider) UnlikeTracks(ctx context.Context, tracks []domain.Track) error {
	var uris []string
	for _, t := range tracks {
		if t.ID != "" {
			uris = append(uris, "spotify:track:"+t.ID)
		}
	}
	return p.removeLibraryURIs(ctx, uris, func(batch []string) error {
		ids := make([]string, 0, len(batch))
		for _, uri := range batch {
			ids = append(ids, strings.TrimPrefix(uri, "spotify:track:"))
		}
		return p.client.Delete(ctx, "/me/tracks", url.Values{"ids": {strings.Join(ids, ",")}}, nil)
	})
}

// FollowArtists implements domain.Provider.
func (p *Provider) FollowArtists(ctx context.Context, artists []domain.Artist) error {
	var uris []string
	for _, a := range artists {
		if a.ID != "" {
			uris = append(uris, "spotify:artist:"+a.ID)
		}
	}
	return p.saveLibraryURIs(ctx, uris, func(batch []string) error {
		ids := make([]string, 0, len(batch))
		for _, uri := range batch {
			ids = append(ids, strings.TrimPrefix(uri, "spotify:artist:"))
		}
		return p.client.Put(ctx, "/me/following", url.Values{
			"type": {"artist"},
			"ids":  {strings.Join(ids, ",")},
		}, nil)
	})
}

// saveLibraryURIs writes URIs in batches accepted by the generic library
// endpoint. legacy is called for each batch when the endpoint is unavailable.
func (p *Provider) saveLibraryURIs(ctx context.Context, uris []string, legacy func([]string) error) error {
	for start := 0; start < len(uris); start += libraryBatchSize {
		end := min(start+libraryBatchSize, len(uris))
		batch := uris[start:end]
		q := url.Values{"uris": {strings.Join(batch, ",")}}
		err := p.client.Put(ctx, "/me/library", q, nil)
		if isUnsupported(err) {
			err = legacy(batch)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// removeLibraryURIs removes URIs in batches accepted by the generic library
// endpoint. legacy is called for each batch when the endpoint is unavailable.
func (p *Provider) removeLibraryURIs(ctx context.Context, uris []string, legacy func([]string) error) error {
	for start := 0; start < len(uris); start += libraryBatchSize {
		end := min(start+libraryBatchSize, len(uris))
		batch := uris[start:end]
		q := url.Values{"uris": {strings.Join(batch, ",")}}
		err := p.client.Delete(ctx, "/me/library", q, nil)
		if isUnsupported(err) {
			err = legacy(batch)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func isUnsupported(err error) bool {
	var apiErr *domain.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == 404 || apiErr.StatusCode == 405
}

func clampLimit(limit, max int) int {
	if limit <= 0 || limit > max {
		return max
	}
	return limit
}

func sanitizeQuery(q string) string {
	q = strings.NewReplacer("\n", " ", "\r", " ", `"`, " ").Replace(q)
	return strings.Join(strings.Fields(q), " ")
}

// sanitizePlaylistDescription removes line breaks and other control
// whitespace. Spotify rejects descriptions containing newlines with HTTP 400,
// even though the API reference does not mention that restriction.
func sanitizePlaylistDescription(description string) string {
	description = strings.Join(strings.Fields(description), " ")
	const maxDescription = 300
	runes := []rune(description)
	if len(runes) > maxDescription {
		return string(runes[:maxDescription])
	}
	return description
}

var _ domain.Provider = (*Provider)(nil)
