package syncer

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/zxbass/music-syncer/internal/domain"
)

// syncHistory archives listening history as a playlist. Neither Yandex Music
// nor Spotify exposes an API for writing play history; a playlist is the only
// portable representation.
func (e *Engine) syncHistory(ctx context.Context) error {
	if !e.Source.Capabilities().HistoryRead {
		return fmt.Errorf("%w: listening history", domain.ErrNotSupported)
	}
	events, err := e.Source.History(ctx, e.Options.HistorySince, e.Options.Limit)
	if err != nil {
		return fmt.Errorf("load listening history: %w", err)
	}
	if e.Options.Limit > 0 && len(events) > e.Options.Limit {
		events = events[:e.Options.Limit]
	}

	result := &HistoryResult{Events: len(events)}
	e.report.History = result

	if e.Source.Name() == domain.Spotify {
		result.Notes = append(result.Notes,
			"Spotify only exposes the last 50 plays through its API; older history cannot be retrieved.")
	}
	if e.Target.Name() == domain.Spotify {
		result.Notes = append(result.Notes,
			"Spotify does not allow writing listening history. The tracks are archived as a playlist instead.")
	} else {
		result.Notes = append(result.Notes,
			"Playback history is read-only in both APIs. The tracks are archived as a playlist instead.")
	}

	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i].PlayedAt, events[j].PlayedAt
		if a.IsZero() {
			return false
		}
		if b.IsZero() {
			return true
		}
		return a.Before(b)
	})

	seen := map[string]bool{}
	var unique []domain.Track
	for _, ev := range events {
		id := ev.Track.ID
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, ev.Track)
	}
	result.UniqueTracks = len(unique)
	if len(unique) == 0 {
		result.Notes = append(result.Notes, "No listening history events were returned.")
		e.Log.Info("no listening history returned")
		return nil
	}

	targetPlaylists, err := e.Target.Playlists(ctx)
	if err != nil {
		return fmt.Errorf("load target playlists: %w", err)
	}
	byID := make(map[string]domain.Playlist, len(targetPlaylists))
	byName := make(map[string][]domain.Playlist, len(targetPlaylists))
	for _, pl := range targetPlaylists {
		byID[pl.ID] = pl
		key := strings.ToLower(strings.TrimSpace(pl.Name))
		byName[key] = append(byName[key], pl)
	}

	historyName := "History from " + e.Source.Name().DisplayName()
	synthetic := domain.Playlist{
		Provider: e.Source.Name(),
		ID:       "__history__",
		Name:     historyName,
	}
	placeholder := PlaylistResult{TargetName: targetPlaylistName(e.Options.Prefix, historyName)}
	target, found := e.findTargetPlaylist(synthetic, byID, byName, &placeholder)
	result.TargetName = targetPlaylistName(e.Options.Prefix, historyName)
	if !found {
		if e.Options.DryRun {
			result.WouldCreate = true
		} else {
			description := "Listening history archived from " + e.Source.Name().DisplayName() +
				" by music-syncer. Playback history cannot be written through public APIs."
			description = truncateText(description, 300)
			created, err := e.Target.CreatePlaylist(ctx, result.TargetName, description)
			if err != nil {
				return fmt.Errorf("create history playlist: %w", err)
			}
			target = created
			result.TargetPlaylistID = created.ID
		}
	} else {
		result.TargetPlaylistID = target.ID
		result.TargetName = target.Name
	}

	var existing []domain.Track
	if target.ID != "" {
		existing, err = e.Target.PlaylistTracks(ctx, target)
		if err != nil {
			return fmt.Errorf("read history playlist: %w", err)
		}
	}
	knownIDs := make(map[string]bool, len(existing))
	for _, t := range existing {
		if t.ID != "" {
			knownIDs[t.ID] = true
		}
	}
	index := newTrackIndex(existing)

	stats := TrackStats{Source: len(unique)}
	var add []domain.Track
	addedIDs := map[string]bool{}
	for _, srcTrack := range unique {
		res, err := e.resolve(ctx, srcTrack, index, knownIDs)
		if err != nil {
			if isFatal(err) {
				return err
			}
			stats.Failed++
			e.noteError("match history track "+srcTrack.String(), err)
			continue
		}
		if res.Unmatched != nil {
			stats.Unmatched++
			result.Unmatched = append(result.Unmatched, *res.Unmatched)
			continue
		}
		if res.Track.ID == "" {
			stats.Failed++
			continue
		}
		if res.Existing || addedIDs[res.Track.ID] {
			stats.Existing++
			continue
		}
		addedIDs[res.Track.ID] = true
		add = append(add, res.Track)
	}

	if len(add) > 0 {
		if e.Options.DryRun {
			stats.Added = len(add)
		} else if err := e.Target.AddTracks(ctx, target, add); err != nil {
			stats.Failed += len(add)
			if isFatal(err) {
				return err
			}
			return fmt.Errorf("add history tracks: %w", err)
		} else {
			stats.Added = len(add)
		}
	}
	result.Stats = stats

	if !e.Options.DryRun && target.ID != "" {
		e.Store.PutPlaylist(e.Source.Name(), e.Target.Name(), "__history__", target.ID, target.Name)
	}
	e.Log.Info("archived listening history", "events", result.Events, "unique_tracks", result.UniqueTracks,
		"added", stats.Added, "existing", stats.Existing, "unmatched", stats.Unmatched)
	return nil
}
