package syncer

import (
	"context"
	"fmt"

	"github.com/zxbass/music-syncer/internal/domain"
)

func (e *Engine) syncLikes(ctx context.Context) error {
	if !e.Source.Capabilities().LikedTracks || !e.Target.Capabilities().LikedTracks {
		return fmt.Errorf("%w: liked tracks", domain.ErrNotSupported)
	}
	src, err := e.Source.LikedTracks(ctx, e.Options.Limit)
	if err != nil {
		return fmt.Errorf("load source liked tracks: %w", err)
	}
	if e.Options.Limit > 0 && len(src) > e.Options.Limit {
		src = src[:e.Options.Limit]
	}
	target, err := e.Target.LikedTracks(ctx, 0)
	if err != nil {
		return fmt.Errorf("load target liked tracks: %w", err)
	}

	stats := &TrackStats{Source: len(src)}
	e.report.Likes = stats

	knownIDs := make(map[string]bool, len(target))
	for _, t := range target {
		if t.ID != "" {
			knownIDs[t.ID] = true
		}
	}
	index := newTrackIndex(target)
	seen := map[string]bool{}
	var add []domain.Track

	for _, srcTrack := range src {
		res, err := e.resolve(ctx, srcTrack, index, knownIDs)
		if err != nil {
			if isFatal(err) {
				return err
			}
			stats.Failed++
			e.noteError("match liked track "+srcTrack.String(), err)
			continue
		}
		if res.Unmatched != nil {
			stats.Unmatched++
			e.report.UnmatchedLikes = append(e.report.UnmatchedLikes, *res.Unmatched)
			e.Log.Info("unmatched liked track", "track", srcTrack.String(), "reason", res.Unmatched.Reason)
			continue
		}
		if res.Track.ID == "" {
			stats.Failed++
			continue
		}
		if res.Existing || seen[res.Track.ID] {
			stats.Existing++
			continue
		}
		seen[res.Track.ID] = true
		add = append(add, res.Track)
	}

	if len(add) > 0 {
		if e.Options.DryRun {
			stats.Added = len(add)
		} else if err := e.Target.LikeTracks(ctx, add); err != nil {
			stats.Failed += len(add)
			if isFatal(err) {
				return err
			}
			e.recordError("add liked tracks", err)
		} else {
			stats.Added = len(add)
		}
	}

	e.Log.Info("synced liked tracks", "source_tracks", stats.Source, "added", stats.Added,
		"existing", stats.Existing, "unmatched", stats.Unmatched, "failed", stats.Failed)
	return nil
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

	for _, srcArtist := range src {
		artist, existing, miss, err := e.resolveArtist(ctx, srcArtist, index, knownIDs)
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
		"existing", stats.Existing, "unmatched", stats.Unmatched, "failed", stats.Failed)
	return nil
}
