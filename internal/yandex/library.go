package yandex

import (
	"context"
	"strings"

	"github.com/zxbass/music-syncer/internal/domain"
)

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

// UnlikeTracks implements domain.Provider.
func (p *Provider) UnlikeTracks(ctx context.Context, tracks []domain.Track) error {
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
		if err := p.client.removeLikedTracks(ctx, uid, ids[start:end]); err != nil {
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
