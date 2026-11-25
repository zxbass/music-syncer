package yandex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/zxbass/music-syncer/internal/domain"
)

// Provider adapts the Yandex Music client to domain.Provider.
type Provider struct {
	client *Client
	log    *slog.Logger

	mu  sync.Mutex
	uid string
}

// NewProvider creates a Yandex Music provider. uid may be empty; it is loaded
// from /account/status on first use.
func NewProvider(client *Client, uid string, logger *slog.Logger) *Provider {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Provider{client: client, uid: strings.TrimSpace(uid), log: logger}
}

// Name implements domain.Provider.
func (p *Provider) Name() domain.Name { return domain.Yandex }

// Capabilities implements domain.Provider.
func (p *Provider) Capabilities() domain.Capabilities {
	return domain.Capabilities{
		Playlists:      true,
		LikedTracks:    true,
		FollowedArtist: true,
		HistoryRead:    true,
		HistoryWrite:   false,
	}
}

// Account implements domain.Provider.
func (p *Provider) Account(ctx context.Context) (domain.Account, error) {
	status, err := p.client.accountStatus(ctx)
	if err != nil {
		return domain.Account{}, err
	}
	uid := status.Account.UID.String()
	if uid == "" {
		return domain.Account{}, errors.New("yandex: account response has no uid")
	}
	p.mu.Lock()
	p.uid = uid
	p.mu.Unlock()

	display := status.Account.DisplayName
	if display == "" {
		display = status.Account.FullName
	}
	if display == "" {
		display = status.Account.Login
	}
	return domain.Account{
		Provider:    domain.Yandex,
		ID:          uid,
		Login:       status.Account.Login,
		DisplayName: display,
	}, nil
}

func (p *Provider) ensureUID(ctx context.Context) (string, error) {
	p.mu.Lock()
	uid := p.uid
	p.mu.Unlock()
	if uid != "" {
		return uid, nil
	}
	acc, err := p.Account(ctx)
	if err != nil {
		return "", err
	}
	return acc.ID, nil
}

// Playlists implements domain.Provider.
func (p *Provider) Playlists(ctx context.Context) ([]domain.Playlist, error) {
	uid, err := p.ensureUID(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := p.client.playlists(ctx, uid)
	if err != nil {
		return nil, err
	}
	result := make([]domain.Playlist, 0, len(raw))
	for _, pl := range raw {
		result = append(result, p.mapPlaylist(pl, uid))
	}
	return result, nil
}

func (p *Provider) mapPlaylist(pl playlist, selfUID string) domain.Playlist {
	owner := pl.Owner.UID.String()
	if owner == "" {
		owner = pl.UID.String()
	}
	kind := pl.Kind.String()
	out := domain.Playlist{
		Provider:    domain.Yandex,
		Kind:        kind,
		OwnerID:     owner,
		Name:        pl.Title,
		Description: pl.Description,
		TrackCount:  pl.TrackCount,
		URL:         "https://music.yandex.ru/users/" + owner + "/playlists/" + kind,
	}
	if owner != "" && kind != "" {
		out.ID = owner + ":" + kind
	}
	if owner != "" && selfUID != "" && owner != selfUID {
		out.ReadOnly = true
	}
	if pl.GeneratedPlaylistType != "" || pl.Type == "missedLikes" || pl.Type == "recentTracks" {
		out.Generated = true
	}
	if kind == "3" && (owner == "" || owner == selfUID) {
		out.Special = "liked"
	}
	return out
}

// PlaylistTracks implements domain.Provider.
func (p *Provider) PlaylistTracks(ctx context.Context, playlist domain.Playlist) ([]domain.Track, error) {
	owner := playlist.OwnerID
	if owner == "" {
		var err error
		owner, err = p.ensureUID(ctx)
		if err != nil {
			return nil, err
		}
	}
	kind := playlist.Kind
	if kind == "" {
		_, kind, _ = strings.Cut(playlist.ID, ":")
	}
	if kind == "" {
		return nil, errors.New("yandex: playlist has no kind")
	}
	raw, err := p.client.playlist(ctx, owner, kind)
	if err != nil {
		return nil, err
	}
	return p.mapTrackRefs(ctx, raw.Tracks)
}

// mapTrackRefs resolves short playlist entries to full tracks, fetching the
// missing metadata in batches.
func (p *Provider) mapTrackRefs(ctx context.Context, refs []trackRef) ([]domain.Track, error) {
	var missing []string
	for _, ref := range refs {
		if ref.Track == nil || ref.Track.Title == "" {
			if id := ref.ID.String(); id != "" {
				missing = append(missing, trackID(id, ref.AlbumID.String()))
			}
		}
	}
	hydrated, err := p.hydrate(ctx, missing)
	if err != nil {
		return nil, err
	}

	result := make([]domain.Track, 0, len(refs))
	for _, ref := range refs {
		id := ref.ID.String()
		if id == "" {
			continue
		}
		t := ref.Track
		if t == nil || t.Title == "" {
			if got, ok := hydrated[id]; ok {
				t = &got
			}
		}
		if t == nil || t.Title == "" {
			// Keep the identifier so the sync engine can report the track as
			// unmatched instead of silently dropping it. This also prevents
			// --mirror from deleting a track that simply failed to hydrate.
			p.log.Debug("unresolved yandex track", "id", id)
			result = append(result, domain.Track{
				Provider: domain.Yandex,
				ID:       id,
				AlbumID:  ref.AlbumID.String(),
			})
			continue
		}
		result = append(result, p.mapTrack(*t, ref.AlbumID))
	}
	return result, nil
}

func (p *Provider) hydrate(ctx context.Context, ids []string) (map[string]track, error) {
	result := map[string]track{}
	unique := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	for start := 0; start < len(unique); start += 100 {
		end := min(start+100, len(unique))
		tracks, err := p.client.tracks(ctx, unique[start:end])
		if err != nil {
			return nil, err
		}
		for _, t := range tracks {
			result[t.ID.String()] = t
		}
	}
	return result, nil
}

func (p *Provider) mapTrack(t track, albumFallback FlexID) domain.Track {
	title := t.Title
	if t.Version != "" && !strings.Contains(strings.ToLower(title), strings.ToLower(t.Version)) {
		title = strings.TrimSpace(title + " (" + t.Version + ")")
	}
	artists := make([]string, 0, len(t.Artists))
	for _, a := range t.Artists {
		if strings.TrimSpace(a.Name) != "" {
			artists = append(artists, a.Name)
		}
	}
	if len(artists) == 0 && t.UGCArtistName != "" {
		artists = append(artists, t.UGCArtistName)
	}
	albumID := albumFallback.String()
	albumName := ""
	if len(t.Albums) > 0 {
		if id := t.Albums[0].ID.String(); id != "" {
			albumID = id
		}
		albumName = t.Albums[0].Title
	}
	id := t.ID.String()
	return domain.Track{
		Provider:   domain.Yandex,
		ID:         id,
		AlbumID:    albumID,
		Title:      title,
		Artists:    artists,
		Album:      albumName,
		DurationMS: t.DurationMS,
		ISRC:       strings.TrimSpace(t.ISRC),
		Explicit:   t.Explicit,
		URL:        "https://music.yandex.ru/track/" + id,
	}
}

// LikedTracks implements domain.Provider.
func (p *Provider) LikedTracks(ctx context.Context, limit int) ([]domain.Track, error) {
	uid, err := p.ensureUID(ctx)
	if err != nil {
		return nil, err
	}
	refs, err := p.client.likedTrackRefs(ctx, uid)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(refs) > limit {
		refs = refs[:limit]
	}
	return p.mapTrackRefs(ctx, refs)
}

// LikedArtists implements domain.Provider.
func (p *Provider) LikedArtists(ctx context.Context, limit int) ([]domain.Artist, error) {
	uid, err := p.ensureUID(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := p.client.likedArtists(ctx, uid)
	if err != nil {
		return nil, err
	}
	result := make([]domain.Artist, 0, len(raw))
	for _, item := range raw {
		a := item.Artist
		if a == nil {
			if item.Name == "" || item.ID == "" {
				continue
			}
			a = &artist{ID: item.ID, Name: item.Name}
		}
		if a.ID == "" || a.Name == "" {
			continue
		}
		result = append(result, domain.Artist{
			Provider: domain.Yandex,
			ID:       a.ID.String(),
			Name:     a.Name,
			URL:      "https://music.yandex.ru/artist/" + a.ID.String(),
		})
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	return result, nil
}

// History implements domain.Provider. Yandex returns history grouped by day.
func (p *Provider) History(ctx context.Context, since time.Time, limit int) ([]domain.PlayEvent, error) {
	raw, err := p.client.history(ctx, 1000)
	if err != nil {
		return nil, err
	}

	type pending struct {
		item  historyItem
		date  time.Time
		refID string
	}
	var entries []pending
	collect := func(items []historyItem, date time.Time) {
		for _, item := range items {
			if item.Type != "" && item.Type != "track" {
				continue
			}
			id := ""
			if item.Data.FullModel != nil {
				id = item.Data.FullModel.ID.String()
			}
			if id == "" && item.Track != nil {
				id = item.Track.ID.String()
			}
			if id == "" {
				id = item.Data.ItemID.TrackID.String()
			}
			if id == "" {
				continue
			}
			entries = append(entries, pending{item: item, date: date, refID: id})
		}
	}

	for _, tab := range raw.HistoryTabs {
		date := parseHistoryDate(tab.Date)
		for _, group := range tab.Items {
			collect(group.Tracks, date)
		}
	}
	if len(entries) == 0 {
		// Some API versions return flat groups/items without tabs.
		for _, group := range raw.Groups {
			collect(group.Tracks, time.Time{})
		}
		collect(raw.Items, time.Time{})
	}

	var missing []string
	for _, e := range entries {
		full := e.item.Data.FullModel
		if (full == nil || full.Title == "") && (e.item.Track == nil || e.item.Track.Title == "") {
			missing = append(missing, trackID(e.refID, e.item.Data.ItemID.AlbumID.String()))
		}
	}
	hydrated, err := p.hydrate(ctx, missing)
	if err != nil {
		return nil, err
	}

	result := make([]domain.PlayEvent, 0, len(entries))
	for _, e := range entries {
		t := e.item.Data.FullModel
		if t == nil || t.Title == "" {
			t = e.item.Track
		}
		if t == nil || t.Title == "" {
			if got, ok := hydrated[e.refID]; ok {
				t = &got
			}
		}
		if t == nil || t.Title == "" {
			if !since.IsZero() && !e.date.IsZero() && e.date.Before(since) {
				continue
			}
			result = append(result, domain.PlayEvent{
				Track: domain.Track{
					Provider: domain.Yandex,
					ID:       e.refID,
					AlbumID:  e.item.Data.ItemID.AlbumID.String(),
				},
				PlayedAt: e.date,
				Context:  "yandex:history",
			})
			if limit > 0 && len(result) >= limit {
				break
			}
			continue
		}
		if !since.IsZero() && !e.date.IsZero() && e.date.Before(since) {
			continue
		}
		ev := domain.PlayEvent{
			Track:    p.mapTrack(*t, e.item.Data.ItemID.AlbumID),
			PlayedAt: e.date,
			Context:  "yandex:history",
		}
		result = append(result, ev)
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	return result, nil
}

func parseHistoryDate(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	if t, err := time.ParseInLocation("2006-01-02", value, time.Local); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t
	}
	return time.Time{}
}

// SearchTracks implements domain.Provider.
func (p *Provider) SearchTracks(ctx context.Context, track domain.Track, limit int) ([]domain.Track, error) {
	if strings.TrimSpace(track.Title) == "" {
		return nil, nil
	}
	limit = clampLimit(limit, 10)
	query := strings.TrimSpace(track.Title + " " + track.PrimaryArtist())
	raw, err := p.client.searchTracks(ctx, sanitizeQuery(query), limit)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 && track.PrimaryArtist() != "" {
		raw, err = p.client.searchTracks(ctx, sanitizeQuery(track.Title), limit)
		if err != nil {
			return nil, err
		}
	}
	return p.mapSearchTracks(raw), nil
}

func (p *Provider) mapSearchTracks(raw []track) []domain.Track {
	result := make([]domain.Track, 0, len(raw))
	for _, t := range raw {
		if t.ID.String() == "" || t.Title == "" {
			continue
		}
		result = append(result, p.mapTrack(t, ""))
	}
	return result
}

// SearchArtists implements domain.Provider.
func (p *Provider) SearchArtists(ctx context.Context, artist domain.Artist, limit int) ([]domain.Artist, error) {
	if strings.TrimSpace(artist.Name) == "" {
		return nil, nil
	}
	raw, err := p.client.searchArtists(ctx, sanitizeQuery(artist.Name), clampLimit(limit, 10))
	if err != nil {
		return nil, err
	}
	result := make([]domain.Artist, 0, len(raw))
	for _, a := range raw {
		if a.ID.String() == "" || a.Name == "" {
			continue
		}
		result = append(result, domain.Artist{
			Provider: domain.Yandex,
			ID:       a.ID.String(),
			Name:     a.Name,
			URL:      "https://music.yandex.ru/artist/" + a.ID.String(),
		})
	}
	return result, nil
}

// CreatePlaylist implements domain.Provider. Playlists are created private.
func (p *Provider) CreatePlaylist(ctx context.Context, name, description string) (domain.Playlist, error) {
	uid, err := p.ensureUID(ctx)
	if err != nil {
		return domain.Playlist{}, err
	}
	raw, err := p.client.createPlaylist(ctx, uid, name)
	if err != nil {
		return domain.Playlist{}, err
	}
	raw.Owner.UID = FlexID(uid)
	raw.UID = FlexID(uid)
	pl := p.mapPlaylist(raw, uid)
	if pl.Name == "" {
		pl.Name = name
	}
	if description != "" {
		p.log.Debug("yandex create playlist does not support a description", "playlist", name)
	}
	return pl, nil
}

// AddTracks implements domain.Provider.
func (p *Provider) AddTracks(ctx context.Context, playlist domain.Playlist, tracks []domain.Track) error {
	if len(tracks) == 0 {
		return nil
	}
	uid, kind, err := p.ownTarget(ctx, playlist)
	if err != nil {
		return err
	}

	// Album IDs are mandatory in Yandex's playlist diff. Tracks returned by
	// search and playlist reads normally have one; repair the rare exceptions.
	var missing []string
	for _, t := range tracks {
		if t.AlbumID == "" && t.ID != "" {
			missing = append(missing, t.ID)
		}
	}
	if len(missing) > 0 {
		hydrated, err := p.hydrate(ctx, missing)
		if err != nil {
			return err
		}
		for i := range tracks {
			if tracks[i].AlbumID == "" {
				if got, ok := hydrated[tracks[i].ID]; ok && len(got.Albums) > 0 {
					tracks[i].AlbumID = got.Albums[0].ID.String()
				}
			}
		}
	}

	refs := make([]changeRef, 0, len(tracks))
	for _, t := range tracks {
		if t.ID == "" {
			continue
		}
		if t.AlbumID == "" {
			return fmt.Errorf("yandex: track %q has no album id, cannot add to playlist", t)
		}
		refs = append(refs, changeRef{ID: t.ID, AlbumID: t.AlbumID})
	}
	if len(refs) == 0 {
		return nil
	}

	for start := 0; start < len(refs); start += 100 {
		end := min(start+100, len(refs))
		if err := p.insertChunk(ctx, uid, kind, refs[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) insertChunk(ctx context.Context, uid, kind string, refs []changeRef) error {
	for attempt := 0; attempt < 2; attempt++ {
		current, err := p.client.playlist(ctx, uid, kind)
		if err != nil {
			return err
		}
		at := current.TrackCount
		if at <= 0 {
			at = len(current.Tracks)
		}
		ops := []changeOp{insertOp(at, refs)}
		_, err = p.client.changePlaylist(ctx, uid, kind, current.Revision, ops)
		if err == nil {
			return nil
		}
		if !isRevisionMismatch(err) {
			return err
		}
		p.log.Debug("yandex playlist revision changed, retrying insert", "kind", kind)
	}
	return fmt.Errorf("yandex: playlist revision kept changing while adding tracks")
}

// RemoveTracks implements domain.Provider.
func (p *Provider) RemoveTracks(ctx context.Context, playlist domain.Playlist, tracks []domain.Track) error {
	if len(tracks) == 0 {
		return nil
	}
	uid, kind, err := p.ownTarget(ctx, playlist)
	if err != nil {
		return err
	}
	remove := map[string]bool{}
	for _, t := range tracks {
		if t.ID != "" {
			remove[t.ID] = true
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		current, err := p.client.playlist(ctx, uid, kind)
		if err != nil {
			return err
		}
		ranges := deletionRanges(current.Tracks, remove)
		if len(ranges) == 0 {
			return nil
		}
		revision := current.Revision
		retry := false
		for _, r := range ranges {
			ops := []changeOp{deleteOp(r[0], r[1])}
			updated, err := p.client.changePlaylist(ctx, uid, kind, revision, ops)
			if err != nil {
				if isRevisionMismatch(err) {
					retry = true
					break
				}
				return err
			}
			revision = updated.Revision
		}
		if !retry {
			return nil
		}
	}
	return fmt.Errorf("yandex: playlist revision kept changing while removing tracks")
}

// deletionRanges returns [from,to) index ranges for the given track IDs,
// sorted from the end of the playlist so earlier indices stay valid.
func deletionRanges(refs []trackRef, remove map[string]bool) [][2]int {
	var ranges [][2]int
	start := -1
	for i, ref := range refs {
		if remove[ref.ID.String()] {
			if start < 0 {
				start = i
			}
		} else if start >= 0 {
			ranges = append(ranges, [2]int{start, i})
			start = -1
		}
	}
	if start >= 0 {
		ranges = append(ranges, [2]int{start, len(refs)})
	}
	for i, j := 0, len(ranges)-1; i < j; i, j = i+1, j-1 {
		ranges[i], ranges[j] = ranges[j], ranges[i]
	}
	return ranges
}

func (p *Provider) ownTarget(ctx context.Context, playlist domain.Playlist) (uid, kind string, err error) {
	self, err := p.ensureUID(ctx)
	if err != nil {
		return "", "", err
	}
	if playlist.OwnerID != "" && playlist.OwnerID != self {
		return "", "", fmt.Errorf("yandex: cannot modify a playlist owned by another user (%s)", playlist.OwnerID)
	}
	uid = self
	kind = playlist.Kind
	if kind == "" {
		_, kind, _ = strings.Cut(playlist.ID, ":")
	}
	if kind == "" {
		return "", "", errors.New("yandex: playlist has no kind")
	}
	return uid, kind, nil
}

// LikeTracks implements domain.Provider.
func (p *Provider) LikeTracks(ctx context.Context, tracks []domain.Track) error {
	uid, err := p.ensureUID(ctx)
	if err != nil {
		return err
	}
	var ids []string
	for _, t := range tracks {
		if t.ID != "" {
			ids = append(ids, t.ID)
		}
	}
	for start := 0; start < len(ids); start += 100 {
		end := min(start+100, len(ids))
		if err := p.client.addLikedTracks(ctx, uid, ids[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// FollowArtists implements domain.Provider.
func (p *Provider) FollowArtists(ctx context.Context, artists []domain.Artist) error {
	uid, err := p.ensureUID(ctx)
	if err != nil {
		return err
	}
	var ids []string
	for _, a := range artists {
		if a.ID != "" {
			ids = append(ids, a.ID)
		}
	}
	for start := 0; start < len(ids); start += 100 {
		end := min(start+100, len(ids))
		if err := p.client.addLikedArtists(ctx, uid, ids[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func isRevisionMismatch(err error) bool {
	var apiErr *domain.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == 412
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

var _ domain.Provider = (*Provider)(nil)
