package syncer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zxbass/music-syncer/internal/domain"
)

func (e *Engine) syncPlaylists(ctx context.Context) error {
	srcPlaylists, err := e.Source.Playlists(ctx)
	if err != nil {
		return fmt.Errorf("load source playlists: %w", err)
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

	selected := 0
	for _, src := range srcPlaylists {
		if !e.selectPlaylist(src) {
			continue
		}
		selected++
		result, fatal := e.syncOnePlaylist(ctx, src, byID, byName)
		e.report.Playlists = append(e.report.Playlists, result)
		if err := e.Store.Save(); err != nil {
			e.recordError("save state", err)
		}
		if fatal != nil {
			return fatal
		}
	}

	if selected == 0 {
		if len(e.Options.Playlists) > 0 {
			return fmt.Errorf("no playlists matched %q", strings.Join(e.Options.Playlists, ", "))
		}
		return fmt.Errorf("source account has no playlists to transfer")
	}
	return nil
}

func (e *Engine) selectPlaylist(pl domain.Playlist) bool {
	if len(e.Options.Playlists) > 0 {
		for _, filter := range e.Options.Playlists {
			f := strings.TrimSpace(filter)
			if f == "" {
				continue
			}
			if f == pl.ID || f == pl.Kind {
				return true
			}
			if strings.EqualFold(f, pl.Name) {
				return true
			}
			if strings.Contains(strings.ToLower(pl.Name), strings.ToLower(f)) {
				return true
			}
		}
		return false
	}
	if pl.Special != "" {
		return false
	}
	if pl.ReadOnly {
		return false
	}
	if pl.Generated && !e.Options.AllPlaylists {
		return false
	}
	return true
}

func (e *Engine) syncOnePlaylist(ctx context.Context, src domain.Playlist, targetByID map[string]domain.Playlist, targetByName map[string][]domain.Playlist) (PlaylistResult, error) {
	result := PlaylistResult{
		SourceID:   src.ID,
		SourceName: src.Name,
		TargetName: targetPlaylistName(e.Options.Prefix, src.Name),
	}
	if src.ReadOnly {
		result.Error = "playlist is read-only for this account; its tracks cannot be read or copied"
		e.recordError("playlist "+src.Name, errors.New(result.Error))
		return result, nil
	}

	srcTracks, err := e.Source.PlaylistTracks(ctx, src)
	if err != nil {
		result.Error = err.Error()
		if isFatal(err) {
			return result, err
		}
		e.recordError("playlist "+src.Name, err)
		return result, nil
	}
	truncated := false
	if e.Options.Limit > 0 && len(srcTracks) > e.Options.Limit {
		srcTracks = srcTracks[:e.Options.Limit]
		truncated = true
		result.Notes = append(result.Notes, fmt.Sprintf("limited to the first %d tracks", e.Options.Limit))
	}

	target, found := e.findTargetPlaylist(src, targetByID, targetByName, &result)
	if !found {
		if e.Options.DryRun {
			result.WouldCreate = true
		} else {
			description := "Imported from " + src.Provider.DisplayName() + " with music-syncer"
			if src.URL != "" {
				description += "\nSource: " + src.URL
			}
			description = truncateText(description, 300)
			created, err := e.Target.CreatePlaylist(ctx, result.TargetName, description)
			if err != nil {
				result.Error = err.Error()
				if isFatal(err) {
					return result, err
				}
				e.recordError("create playlist "+result.TargetName, err)
				return result, nil
			}
			result.Created = true
			result.TargetID = created.ID
			target = created
			if created.ID != "" {
				targetByID[created.ID] = created
			}
			key := strings.ToLower(strings.TrimSpace(created.Name))
			targetByName[key] = append(targetByName[key], created)
		}
	} else {
		result.TargetID = target.ID
		result.TargetName = target.Name
	}

	var existing []domain.Track
	if target.ID != "" {
		var err error
		existing, err = e.Target.PlaylistTracks(ctx, target)
		if err != nil {
			result.Error = err.Error()
			if isFatal(err) {
				return result, err
			}
			e.recordError("read target playlist "+target.Name, err)
			return result, nil
		}
	}

	existingIDs := make(map[string]bool, len(existing))
	for _, t := range existing {
		if t.ID != "" {
			existingIDs[t.ID] = true
		}
	}
	index := newTrackIndex(existing)
	desired := map[string]bool{}
	seenNew := map[string]bool{}
	var newTracks []domain.Track

	for _, srcTrack := range srcTracks {
		result.Stats.Source++
		res, err := e.resolve(ctx, srcTrack, index, existingIDs)
		if err != nil {
			if isFatal(err) {
				result.Error = err.Error()
				return result, err
			}
			result.Stats.Failed++
			e.noteError("match "+srcTrack.String(), err)
			continue
		}
		if res.Unmatched != nil {
			result.Stats.Unmatched++
			result.Unmatched = append(result.Unmatched, *res.Unmatched)
			continue
		}
		if res.Track.ID == "" {
			result.Stats.Failed++
			e.noteError("match "+srcTrack.String(), fmt.Errorf("target returned an empty track id"))
			continue
		}
		if res.Existing || desired[res.Track.ID] || seenNew[res.Track.ID] {
			result.Stats.Existing++
			desired[res.Track.ID] = true
			continue
		}
		desired[res.Track.ID] = true
		seenNew[res.Track.ID] = true
		newTracks = append(newTracks, res.Track)
	}

	if len(newTracks) > 0 {
		if e.Options.DryRun {
			result.Stats.Added = len(newTracks)
		} else {
			if err := e.Target.AddTracks(ctx, target, newTracks); err != nil {
				result.Stats.Failed += len(newTracks)
				result.Error = err.Error()
				if isFatal(err) {
					return result, err
				}
				e.recordError("add tracks to "+result.TargetName, err)
			} else {
				result.Stats.Added = len(newTracks)
			}
		}
	}

	if e.Options.Mirror {
		var extra []domain.Track
		for _, t := range existing {
			if t.ID != "" && !desired[t.ID] {
				extra = append(extra, t)
			}
		}
		if len(extra) > 0 {
			switch {
			case truncated:
				result.Notes = append(result.Notes,
					"mirror skipped: --limit truncated the source playlist, so removing tracks would be unsafe")
			case result.Stats.Unmatched > 0 || result.Stats.Failed > 0:
				result.Notes = append(result.Notes,
					fmt.Sprintf("mirror skipped: %d unmatched/failed source tracks would be at risk", result.Stats.Unmatched+result.Stats.Failed))
			case e.Options.DryRun:
				result.Removed = len(extra)
				result.Notes = append(result.Notes, "dry-run: tracks would be removed from the target playlist")
			default:
				if err := e.Target.RemoveTracks(ctx, target, extra); err != nil {
					result.Error = err.Error()
					if isFatal(err) {
						return result, err
					}
					e.recordError("remove tracks from "+result.TargetName, err)
				} else {
					result.Removed = len(extra)
				}
			}
		}
	}

	if !e.Options.DryRun && target.ID != "" {
		e.Store.PutPlaylist(src.Provider, e.Target.Name(), src.ID, target.ID, target.Name)
	}

	if result.Created {
		e.Log.Info("created playlist", "name", result.TargetName, "tracks", len(srcTracks))
	} else if !found && e.Options.DryRun {
		e.Log.Info("would create playlist", "name", result.TargetName, "tracks", len(srcTracks))
	} else {
		e.Log.Info("synced playlist", "name", src.Name, "source_tracks", result.Stats.Source,
			"added", result.Stats.Added, "existing", result.Stats.Existing,
			"unmatched", result.Stats.Unmatched, "failed", result.Stats.Failed)
	}
	return result, nil
}

func (e *Engine) findTargetPlaylist(src domain.Playlist, byID map[string]domain.Playlist, byName map[string][]domain.Playlist, result *PlaylistResult) (domain.Playlist, bool) {
	if link, ok := e.Store.LookupPlaylist(src.Provider, e.Target.Name(), src.ID); ok && link.TargetID != "" {
		if pl, found := byID[link.TargetID]; found && !pl.ReadOnly {
			return pl, true
		}
		result.Notes = append(result.Notes, "previous target playlist link is stale or read-only")
	}
	name := targetPlaylistName(e.Options.Prefix, src.Name)
	matches := byName[strings.ToLower(strings.TrimSpace(name))]
	var writable []domain.Playlist
	for _, pl := range matches {
		if !pl.ReadOnly {
			writable = append(writable, pl)
		}
	}
	matches = writable
	switch len(matches) {
	case 1:
		result.Notes = append(result.Notes, "linked to an existing target playlist with the same name")
		return matches[0], true
	case 0:
		return domain.Playlist{}, false
	default:
		result.Notes = append(result.Notes,
			fmt.Sprintf("found %d target playlists named %q; creating a separate one", len(matches), name))
		return domain.Playlist{}, false
	}
}
