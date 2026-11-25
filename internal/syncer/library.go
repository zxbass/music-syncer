package syncer

import (
	"context"
	"errors"
	"fmt"

	"github.com/zxbass/music-syncer/internal/domain"
)

const likesPlaylistSourceID = "__liked_tracks__"

func (e *Engine) syncLikes(ctx context.Context) error {
	if !e.Source.Capabilities().LikedTracks || !e.Target.Capabilities().Playlists {
		return fmt.Errorf("%w: liked tracks", domain.ErrNotSupported)
	}
	src, err := e.Source.LikedTracks(ctx, e.Options.Limit)
	if err != nil {
		return fmt.Errorf("load source liked tracks: %w", err)
	}
	if e.Options.Limit > 0 && len(src) > e.Options.Limit {
		src = src[:e.Options.Limit]
	}

	stats := &TrackStats{Source: len(src)}
	e.report.Likes = stats
	if len(src) == 0 {
		e.Log.Info("synced liked tracks", "source_tracks", 0)
		return nil
	}

	target, playlist, err := e.ensureLikesPlaylist(ctx)
	if err != nil {
		return err
	}
	e.report.LikesPlaylist = playlist

	existing, err := e.loadTargetTracks(ctx, target)
	if err != nil {
		return fmt.Errorf("load favorites playlist: %w", err)
	}

	result := &PlaylistResult{
		TargetID:   target.ID,
		TargetName: playlist.TargetName,
	}
	newTracks, _, err := e.matchPlaylistTracks(ctx, src, existing, result)
	stats.Existing = result.Stats.Existing
	stats.Unmatched = result.Stats.Unmatched
	stats.Deferred = result.Stats.Deferred
	stats.Failed = result.Stats.Failed
	if len(result.Unmatched) > 0 {
		e.report.UnmatchedLikes = append(e.report.UnmatchedLikes, result.Unmatched...)
		for _, m := range result.Unmatched {
			e.Log.Info("unmatched liked track", "track", m.Source.String(), "reason", m.Reason)
		}
	}
	if err != nil {
		return err
	}

	if err := e.addTracks(ctx, target, newTracks, stats); err != nil {
		if isFatal(err) {
			return err
		}
		e.recordError("add liked tracks to "+playlist.TargetName, err)
	}

	e.Log.Info("synced liked tracks", "source_tracks", stats.Source, "added", stats.Added,
		"existing", stats.Existing, "unmatched", stats.Unmatched, "deferred", stats.Deferred,
		"failed", stats.Failed, "playlist", playlist.TargetName)
	return nil
}

// ensureLikesPlaylist finds or creates the playlist that receives liked
// tracks. The target's native favorites are intentionally not touched.
func (e *Engine) ensureLikesPlaylist(ctx context.Context) (domain.Playlist, *LikesPlaylistResult, error) {
	name := targetPlaylistName(e.Options.Prefix, favoritesPlaylistName(e.Source.Name()))
	playlists, err := e.Target.Playlists(ctx)
	if err != nil {
		return domain.Playlist{}, nil, fmt.Errorf("load target playlists: %w", err)
	}

	byID := make(map[string]domain.Playlist, len(playlists))
	for _, pl := range playlists {
		if pl.ID != "" {
			byID[pl.ID] = pl
		}
	}
	if link, ok := e.Store.LookupPlaylist(e.Source.Name(), e.Target.Name(), likesPlaylistSourceID); ok && link.TargetID != "" {
		if pl, found := byID[link.TargetID]; found && !pl.ReadOnly {
			return pl, &LikesPlaylistResult{TargetID: pl.ID, TargetName: pl.Name}, nil
		}
	}

	var existing *domain.Playlist
	for _, pl := range playlists {
		if pl.ID == "" || pl.ReadOnly || playlistNameKey(pl.Name) != playlistNameKey(name) {
			continue
		}
		if e.Store.TargetPlaylistInUse(e.Target.Name(), pl.ID) {
			continue
		}
		pl := pl
		existing = &pl
		break
	}
	if existing != nil {
		e.linkPlaylist(likesPlaylistSourceID, *existing)
		return *existing, &LikesPlaylistResult{TargetID: existing.ID, TargetName: existing.Name}, nil
	}

	if e.Options.DryRun {
		return domain.Playlist{Provider: e.Target.Name(), Name: name},
			&LikesPlaylistResult{TargetName: name, WouldCreate: true}, nil
	}

	description := fmt.Sprintf("Liked tracks imported from %s with music-syncer", e.Source.Name().DisplayName())
	created, err := e.Target.CreatePlaylist(ctx, name, truncateText(description, 300))
	if err != nil {
		return domain.Playlist{}, nil, fmt.Errorf("create favorites playlist %q: %w", name, err)
	}
	if created.Name == "" {
		created.Name = name
	}
	e.linkPlaylist(likesPlaylistSourceID, created)
	return created, &LikesPlaylistResult{
		TargetID:   created.ID,
		TargetName: created.Name,
		Created:    true,
	}, nil
}

func (e *Engine) syncArtists(ctx context.Context) error {
	if !e.Source.Capabilities().FollowedArtist || !e.Target.Capabilities().FollowedArtist {
		return fmt.Errorf("%w: followed artists", domain.ErrNotSupported)
	}
	src, err := e.Source.LikedArtists(ctx, e.Options.Limit)
	if err != nil {
		return fmt.Errorf("load source artists: %w", err)
	}
	if e.Options.Limit > 0 && len(src) > e.Options.Limit {
		src = src[:e.Options.Limit]
	}
	target, err := e.Target.LikedArtists(ctx, 0)
	if err != nil {
		return fmt.Errorf("load target artists: %w", err)
	}

	stats := &ArtistStats{Source: len(src)}
	e.report.Artists = stats

	knownIDs := make(map[string]bool, len(target))
	for _, a := range target {
		if a.ID != "" {
			knownIDs[a.ID] = true
		}
	}
	index := newArtistIndex(target)
	seen := map[string]bool{}
	var add []domain.Artist
	var unmatched []UnmatchedArtist

	for i, srcArtist := range src {
		artist, existing, miss, err := e.resolveArtist(ctx, srcArtist, index, knownIDs)
		if errors.Is(err, errSearchBatch) {
			stats.Deferred += len(src) - i
			break
		}
		if err != nil {
			if isFatal(err) {
				return err
			}
			stats.Failed++
			e.noteError("match artist "+srcArtist.Name, err)
			continue
		}
		if miss != nil {
			stats.Unmatched++
			unmatched = append(unmatched, *miss)
			continue
		}
		if existing || seen[artist.ID] {
			stats.Existing++
			continue
		}
		seen[artist.ID] = true
		add = append(add, artist)
	}

	if len(add) > 0 {
		if e.Options.DryRun {
			stats.Added = len(add)
		} else if err := e.Target.FollowArtists(ctx, add); err != nil {
			stats.Failed += len(add)
			if isFatal(err) {
				return err
			}
			e.recordError("follow artists", err)
		} else {
			stats.Added = len(add)
		}
	}

	if len(unmatched) > 0 {
		e.report.UnmatchedArtists = append(e.report.UnmatchedArtists, unmatched...)
		e.Log.Info("unmatched artists", "count", len(unmatched))
		for _, m := range unmatched {
			e.Log.Info("unmatched artist", "artist", m.Source.Name, "reason", m.Reason)
		}
	}
	e.Log.Info("synced artists", "source_artists", stats.Source, "added", stats.Added,
		"existing", stats.Existing, "unmatched", stats.Unmatched, "deferred", stats.Deferred,
		"failed", stats.Failed)
	return nil
}
