package syncer

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"

	"github.com/zxbass/music-syncer/internal/domain"
)

// targetPlaylistIndex resolves source playlists to target playlists and makes
// sure one target playlist is never shared by two source playlists in the same
// run.
type targetPlaylistIndex struct {
	byID    map[string]domain.Playlist
	byName  map[string][]domain.Playlist
	claimed map[string]bool
}

func newTargetPlaylistIndex(playlists []domain.Playlist) *targetPlaylistIndex {
	index := &targetPlaylistIndex{
		byID:    make(map[string]domain.Playlist, len(playlists)),
		byName:  make(map[string][]domain.Playlist, len(playlists)),
		claimed: make(map[string]bool),
	}
	for _, playlist := range playlists {
		index.add(playlist)
	}
	return index
}

func (i *targetPlaylistIndex) add(playlist domain.Playlist) {
	if playlist.ID == "" {
		return
	}
	i.byID[playlist.ID] = playlist
	if key := playlistNameKey(playlist.Name); key != "" {
		i.byName[key] = append(i.byName[key], playlist)
	}
}

func (i *targetPlaylistIndex) claim(id string) {
	if id != "" {
		i.claimed[id] = true
	}
}

func playlistNameKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func (e *Engine) syncPlaylists(ctx context.Context) error {
	srcPlaylists, err := e.Source.Playlists(ctx)
	if err != nil {
		return fmt.Errorf("load source playlists: %w", err)
	}
	targetPlaylists, err := e.Target.Playlists(ctx)
	if err != nil {
		return fmt.Errorf("load target playlists: %w", err)
	}
	index := newTargetPlaylistIndex(targetPlaylists)

	selected := 0
	for _, src := range srcPlaylists {
		if !e.selectPlaylist(src) {
			continue
		}
		selected++
		result, fatal := e.syncOnePlaylist(ctx, src, index)
		e.report.Playlists = append(e.report.Playlists, result)
		if err := e.Store.Save(); err != nil {
			e.recordError("save state", err)
		}
		if fatal != nil {
			return fatal
		}
		if e.deferred {
			break
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
			if f == pl.ID || f == pl.Kind || strings.EqualFold(f, pl.Name) ||
				strings.Contains(strings.ToLower(pl.Name), strings.ToLower(f)) {
				return true
			}
		}
		return false
	}
	if pl.Special != "" || pl.ReadOnly {
		return false
	}
	return !pl.Generated || e.Options.AllPlaylists
}

func (e *Engine) syncOnePlaylist(ctx context.Context, src domain.Playlist, index *targetPlaylistIndex) (PlaylistResult, error) {
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

	mirrorUnsafeReason := ""
	if e.Options.Limit > 0 && len(srcTracks) > e.Options.Limit {
		srcTracks = srcTracks[:e.Options.Limit]
		mirrorUnsafeReason = "--limit truncated the source playlist"
		result.Notes = append(result.Notes, fmt.Sprintf("limited to the first %d tracks", e.Options.Limit))
	}
	if src.TrackCount > len(srcTracks) {
		reason := fmt.Sprintf("source returned %d of %d tracks", len(srcTracks), src.TrackCount)
		result.Notes = append(result.Notes, reason)
		if mirrorUnsafeReason == "" {
			mirrorUnsafeReason = reason
		}
	}

	target, found := e.findTargetPlaylist(src, index, &result)
	if !found {
		if e.Options.DryRun {
			result.WouldCreate = true
		} else {
			created, err := e.createTargetPlaylist(ctx, src, result.TargetName)
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
			if created.Name != "" {
				result.TargetName = created.Name
			}
			target = created
			index.add(created)
			index.claim(created.ID)
		}
	} else {
		result.TargetID = target.ID
		result.TargetName = target.Name
	}

	// Persist the link before any tracks are written. If the process is
	// interrupted here, the next run must reuse this playlist instead of
	// creating a duplicate.
	e.linkPlaylist(src.ID, target)

	existing, err := e.loadTargetTracks(ctx, target)
	if err != nil {
		result.Error = err.Error()
		if isFatal(err) {
			return result, err
		}
		e.recordError("read target playlist "+target.Name, err)
		return result, nil
	}

	newTracks, desired, err := e.matchPlaylistTracks(ctx, srcTracks, existing, &result)
	if err != nil {
		result.Error = err.Error()
		return result, err
	}
	if err := e.applyPlaylistAdds(ctx, target, newTracks, &result); err != nil {
		if isFatal(err) {
			return result, err
		}
	}
	if e.Options.Mirror {
		if err := e.applyPlaylistMirror(ctx, target, existing, desired, &result, mirrorUnsafeReason); err != nil {
			if isFatal(err) {
				return result, err
			}
		}
	}

	e.logPlaylistResult(src, target, result, found)
	return result, nil
}

func (e *Engine) createTargetPlaylist(ctx context.Context, src domain.Playlist, name string) (domain.Playlist, error) {
	description := "Imported from " + src.Provider.DisplayName() + " with music-syncer"
	if src.URL != "" {
		description += "; Source: " + src.URL
	}
	return e.Target.CreatePlaylist(ctx, name, truncateText(description, 300))
}

func (e *Engine) loadTargetTracks(ctx context.Context, target domain.Playlist) ([]domain.Track, error) {
	if target.ID == "" {
		return nil, nil
	}
	return e.Target.PlaylistTracks(ctx, target)
}

func (e *Engine) matchPlaylistTracks(ctx context.Context, srcTracks, existing []domain.Track, result *PlaylistResult) ([]domain.Track, map[string]bool, error) {
	existingIDs := make(map[string]bool, len(existing))
	for _, track := range existing {
		if track.ID != "" {
			existingIDs[track.ID] = true
		}
	}

	known := newTrackIndex(existing)
	desired := make(map[string]bool)
	seen := make(map[string]bool)
	var newTracks []domain.Track

	result.Stats.Source = len(srcTracks)
	for i, srcTrack := range srcTracks {
		resolved, err := e.resolve(ctx, srcTrack, known, existingIDs)
		if errors.Is(err, errSearchBatch) {
			remaining := len(srcTracks) - i
			result.Stats.Deferred += remaining
			result.Notes = append(result.Notes,
				fmt.Sprintf("search batch limit reached; %d track(s) deferred to the next run", remaining))
			break
		}
		if err != nil {
			if isFatal(err) {
				return nil, nil, err
			}
			result.Stats.Failed++
			e.noteError("match "+srcTrack.String(), err)
			continue
		}
		if resolved.Unmatched != nil {
			result.Stats.Unmatched++
			result.Unmatched = append(result.Unmatched, *resolved.Unmatched)
			continue
		}
		if resolved.Track.ID == "" {
			result.Stats.Failed++
			e.noteError("match "+srcTrack.String(), errors.New("target returned an empty track id"))
			continue
		}

		desired[resolved.Track.ID] = true
		if resolved.Existing || seen[resolved.Track.ID] {
			result.Stats.Existing++
			continue
		}
		seen[resolved.Track.ID] = true
		newTracks = append(newTracks, resolved.Track)
	}
	return newTracks, desired, nil
}

func (e *Engine) applyPlaylistAdds(ctx context.Context, target domain.Playlist, tracks []domain.Track, result *PlaylistResult) error {
	if len(tracks) == 0 {
		return nil
	}
	if err := e.addTracks(ctx, target, tracks, &result.Stats); err != nil {
		result.Error = err.Error()
		e.recordError("add tracks to "+result.TargetName, err)
		return err
	}
	return nil
}

func (e *Engine) applyPlaylistMirror(ctx context.Context, target domain.Playlist, existing []domain.Track, desired map[string]bool, result *PlaylistResult, unsafeReason string) error {
	var extra []domain.Track
	for _, track := range existing {
		if track.ID != "" && !desired[track.ID] {
			extra = append(extra, track)
		}
	}
	if len(extra) == 0 {
		return nil
	}

	switch {
	case unsafeReason != "":
		result.Notes = append(result.Notes, "mirror skipped: "+unsafeReason+", so removing tracks would be unsafe")
	case result.Stats.Unmatched > 0 || result.Stats.Failed > 0 || result.Stats.Deferred > 0:
		result.Notes = append(result.Notes,
			fmt.Sprintf("mirror skipped: %d unmatched/failed/deferred source tracks would be at risk",
				result.Stats.Unmatched+result.Stats.Failed+result.Stats.Deferred))
	case e.Options.DryRun:
		result.Removed = len(extra)
		result.Notes = append(result.Notes, "dry-run: tracks would be removed from the target playlist")
	default:
		if err := e.Target.RemoveTracks(ctx, target, extra); err != nil {
			result.Error = err.Error()
			e.recordError("remove tracks from "+result.TargetName, err)
			return err
		}
		result.Removed = len(extra)
	}
	return nil
}

func (e *Engine) findTargetPlaylist(src domain.Playlist, index *targetPlaylistIndex, result *PlaylistResult) (domain.Playlist, bool) {
	if link, ok := e.Store.LookupPlaylist(src.Provider, e.Target.Name(), src.ID); ok && link.TargetID != "" {
		if playlist, found := index.byID[link.TargetID]; found && !playlist.ReadOnly && !index.claimed[playlist.ID] {
			index.claim(playlist.ID)
			return playlist, true
		}
		result.Notes = append(result.Notes,
			"previous target playlist link is stale, read-only, or already used by another source playlist")
	}

	name := targetPlaylistName(e.Options.Prefix, src.Name)
	candidates := index.byName[playlistNameKey(name)]

	// A previous run may have created the target playlist but lost the state
	// link (for example, a hard crash immediately after creation). Prefer the
	// playlist whose description points back to this exact source playlist;
	// this also resolves duplicate target names unambiguously.
	if src.URL != "" {
		for _, playlist := range candidates {
			if playlist.ReadOnly || index.claimed[playlist.ID] {
				continue
			}
			if e.Store.TargetPlaylistInUse(e.Target.Name(), playlist.ID) {
				continue
			}
			if strings.Contains(html.UnescapeString(playlist.Description), src.URL) {
				result.Notes = append(result.Notes, "linked to an existing target playlist by its source URL")
				index.claim(playlist.ID)
				return playlist, true
			}
		}
	}

	var matches []domain.Playlist
	for _, playlist := range candidates {
		if playlist.ReadOnly || index.claimed[playlist.ID] {
			continue
		}
		if e.Store.TargetPlaylistInUse(e.Target.Name(), playlist.ID) {
			continue
		}
		matches = append(matches, playlist)
	}

	switch len(matches) {
	case 1:
		result.Notes = append(result.Notes, "linked to an existing target playlist with the same name")
		index.claim(matches[0].ID)
		return matches[0], true
	case 0:
		return domain.Playlist{}, false
	default:
		result.Notes = append(result.Notes,
			fmt.Sprintf("found %d unclaimed target playlists named %q; creating a separate one", len(matches), name))
		return domain.Playlist{}, false
	}
}

func (e *Engine) logPlaylistResult(src domain.Playlist, target domain.Playlist, result PlaylistResult, found bool) {
	switch {
	case result.Created:
		e.Log.Info("created playlist", "name", result.TargetName, "tracks", result.Stats.Source)
	case !found && e.Options.DryRun:
		e.Log.Info("would create playlist", "name", result.TargetName, "tracks", result.Stats.Source)
	default:
		e.Log.Info("synced playlist", "name", src.Name, "target", target.Name,
			"source_tracks", result.Stats.Source, "added", result.Stats.Added,
			"existing", result.Stats.Existing, "unmatched", result.Stats.Unmatched,
			"deferred", result.Stats.Deferred, "failed", result.Stats.Failed)
	}
}
