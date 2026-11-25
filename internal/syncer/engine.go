// Package syncer contains the provider-independent transfer engine.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/zxbass/music-syncer/internal/domain"
	"github.com/zxbass/music-syncer/internal/matcher"
	"github.com/zxbass/music-syncer/internal/store"
)

// Categories selects what to synchronize.
type Categories struct {
	Playlists bool
	Likes     bool
	Artists   bool
}

// ParseCategories parses a comma-separated list such as
// "playlists,likes,artists".
func ParseCategories(value string) (Categories, error) {
	var c Categories
	value = strings.TrimSpace(value)
	if value == "" {
		return c, errors.New("empty category list")
	}
	for _, part := range strings.Split(value, ",") {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "playlists", "playlist":
			c.Playlists = true
		case "likes", "liked", "tracks":
			c.Likes = true
		case "artists", "artist":
			c.Artists = true
		case "all":
			c = Categories{Playlists: true, Likes: true, Artists: true}
		default:
			return Categories{}, fmt.Errorf("unknown category %q (want playlists, likes, artists or all)", part)
		}
	}
	return c, nil
}

// String returns a comma-separated category list.
func (c Categories) String() string {
	var parts []string
	if c.Playlists {
		parts = append(parts, "playlists")
	}
	if c.Likes {
		parts = append(parts, "likes")
	}
	if c.Artists {
		parts = append(parts, "artists")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

// Options configures a transfer.
type Options struct {
	Categories Categories
	// Playlists filters by playlist ID, kind, exact name or name substring.
	Playlists    []string
	AllPlaylists bool
	Prefix       string
	Mirror       bool
	DryRun       bool
	// Limit caps the number of source tracks per collection. Zero means all.
	Limit int
}

// TrackStats summarizes a track collection sync.
type TrackStats struct {
	Source    int `json:"source"`
	Existing  int `json:"existing"`
	Added     int `json:"added"`
	Unmatched int `json:"unmatched"`
	Failed    int `json:"failed"`
}

// Matched returns the number of source tracks that resolved to a target track.
func (s TrackStats) Matched() int { return s.Existing + s.Added }

// ArtistStats summarizes a followed-artist sync.
type ArtistStats struct {
	Source    int `json:"source"`
	Existing  int `json:"existing"`
	Added     int `json:"added"`
	Unmatched int `json:"unmatched"`
	Failed    int `json:"failed"`
}

// LikesPlaylistResult describes the playlist that receives transferred liked
// tracks. Liked tracks are never written to the target's native favorites.
type LikesPlaylistResult struct {
	TargetID    string `json:"target_id,omitempty"`
	TargetName  string `json:"target_name"`
	Created     bool   `json:"created,omitempty"`
	WouldCreate bool   `json:"would_create,omitempty"`
}

// Unmatched describes a source item that could not be resolved confidently.
type Unmatched struct {
	Source domain.Track `json:"source"`
	Reason string       `json:"reason"`
	// Best is the closest candidate, even when it was rejected.
	Best *matcher.Candidate `json:"best,omitempty"`
}

// UnmatchedArtist describes an artist that could not be resolved.
type UnmatchedArtist struct {
	Source domain.Artist            `json:"source"`
	Reason string                   `json:"reason"`
	Best   *matcher.ArtistCandidate `json:"best,omitempty"`
}

// PlaylistResult is the outcome for one source playlist.
type PlaylistResult struct {
	SourceID   string `json:"source_id"`
	SourceName string `json:"source_name"`
	TargetID   string `json:"target_id,omitempty"`
	TargetName string `json:"target_name"`
	Created    bool   `json:"created"`
	// WouldCreate is set in dry-run mode when a playlist would be created.
	WouldCreate bool        `json:"would_create,omitempty"`
	Stats       TrackStats  `json:"stats"`
	Removed     int         `json:"removed,omitempty"`
	Unmatched   []Unmatched `json:"unmatched,omitempty"`
	Notes       []string    `json:"notes,omitempty"`
	Error       string      `json:"error,omitempty"`
}

// Report is a complete synchronization report.
type Report struct {
	StartedAt        time.Time            `json:"started_at"`
	FinishedAt       time.Time            `json:"finished_at"`
	Source           domain.Name          `json:"source"`
	Target           domain.Name          `json:"target"`
	DryRun           bool                 `json:"dry_run"`
	Categories       string               `json:"categories"`
	Playlists        []PlaylistResult     `json:"playlists,omitempty"`
	Likes            *TrackStats          `json:"likes,omitempty"`
	LikesPlaylist    *LikesPlaylistResult `json:"likes_playlist,omitempty"`
	Artists          *ArtistStats         `json:"artists,omitempty"`
	Errors           []string             `json:"errors,omitempty"`
	UnmatchedLikes   []Unmatched          `json:"unmatched_likes,omitempty"`
	UnmatchedArtists []UnmatchedArtist    `json:"unmatched_artists,omitempty"`
}

// Engine transfers a library between two providers.
type Engine struct {
	Source  domain.Provider
	Target  domain.Provider
	Matcher *matcher.Matcher
	Store   *store.Store
	Log     *slog.Logger
	Options Options

	report *Report
}

// Run executes the configured transfer. A fatal authentication error stops the
// run; ordinary per-item failures are recorded in the report.
func (e *Engine) Run(ctx context.Context) (*Report, error) {
	if e.Source == nil || e.Target == nil {
		return nil, errors.New("syncer: source and target providers are required")
	}
	if e.Source.Name() == e.Target.Name() {
		return nil, errors.New("syncer: source and target must be different providers")
	}
	if e.Matcher == nil {
		e.Matcher = matcher.New(0)
	}
	if e.Store == nil {
		return nil, errors.New("syncer: state store is required")
	}
	if e.Log == nil {
		e.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	e.report = &Report{
		StartedAt:  time.Now(),
		Source:     e.Source.Name(),
		Target:     e.Target.Name(),
		DryRun:     e.Options.DryRun,
		Categories: e.Options.Categories.String(),
	}

	var errs []error
	if e.Options.Categories.Playlists {
		if err := e.syncPlaylists(ctx); err != nil {
			if isFatal(err) {
				return e.abort(err)
			}
			e.recordError("playlists", err)
			errs = append(errs, err)
		}
	}
	if e.Options.Categories.Likes {
		if err := e.syncLikes(ctx); err != nil {
			if isFatal(err) {
				return e.abort(err)
			}
			e.recordError("liked tracks", err)
			errs = append(errs, err)
		}
	}
	if e.Options.Categories.Artists {
		if err := e.syncArtists(ctx); err != nil {
			if isFatal(err) {
				return e.abort(err)
			}
			e.recordError("artists", err)
			errs = append(errs, err)
		}
	}

	e.finish()
	if err := e.Store.Save(); err != nil {
		errs = append(errs, err)
	}
	return e.report, errors.Join(errs...)
}

func (e *Engine) finish() {
	e.report.FinishedAt = time.Now()
}

func (e *Engine) abort(err error) (*Report, error) {
	e.finish()
	_ = e.Store.Save()
	return e.report, err
}

func (e *Engine) recordError(scope string, err error) {
	msg := fmt.Sprintf("%s: %v", scope, err)
	e.report.Errors = append(e.report.Errors, msg)
	e.Log.Warn("sync error", "scope", scope, "error", err)
}

func (e *Engine) noteError(scope string, err error) {
	e.report.Errors = append(e.report.Errors, fmt.Sprintf("%s: %v", scope, err))
}

func isFatal(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		domain.IsUnauthorized(err)
}

// resolution is the outcome of mapping one source track to the target.
type resolution struct {
	Track      domain.Track
	Confidence float64
	Existing   bool
	Unmatched  *Unmatched
}

// resolve maps a source track to the target, using the state cache, then
// tracks already known to the target, then a catalog search.
func (e *Engine) resolve(ctx context.Context, src domain.Track, known *trackIndex, knownIDs map[string]bool) (resolution, error) {
	if cached, ok := e.Store.LookupMatch(src.Provider, e.Target.Name(), src.ID); ok && cached.TargetID != "" {
		track := domain.Track{
			Provider:   e.Target.Name(),
			ID:         cached.TargetID,
			AlbumID:    cached.TargetAlbumID,
			Title:      cached.Title,
			Artists:    append([]string(nil), cached.Artists...),
			DurationMS: cached.DurationMS,
		}
		return resolution{Track: track, Confidence: cached.Confidence, Existing: knownIDs[track.ID]}, nil
	}

	if known != nil {
		if best, ok := e.Matcher.Best(src, known.lookup(src.Title)); ok {
			e.Store.PutMatch(src.Provider, e.Target.Name(), src.ID, best.Track, best.Score)
			return resolution{Track: best.Track, Confidence: best.Score, Existing: true}, nil
		}
	}

	candidates, err := e.Target.SearchTracks(ctx, src, 10)
	if err != nil {
		return resolution{}, err
	}
	ranked := e.Matcher.Rank(src, candidates)
	best, ok := e.Matcher.Best(src, candidates)
	if ok {
		e.Store.PutMatch(src.Provider, e.Target.Name(), src.ID, best.Track, best.Score)
		return resolution{
			Track:      best.Track,
			Confidence: best.Score,
			Existing:   knownIDs[best.Track.ID],
		}, nil
	}

	unmatched := e.unmatchedTrack(src, ranked)
	return resolution{Unmatched: unmatched}, nil
}

func (e *Engine) unmatchedTrack(src domain.Track, ranked []matcher.Candidate) *Unmatched {
	unmatched := &Unmatched{Source: src}
	if len(ranked) == 0 {
		unmatched.Reason = "no search results"
		return unmatched
	}

	best := ranked[0]
	unmatched.Best = &best
	switch {
	case best.StrongVersionConflict:
		unmatched.Reason = fmt.Sprintf("best candidate %q is a different version (score %.2f)", best.Track.String(), best.Score)
	case best.TitleScore < e.Matcher.MinTitle:
		unmatched.Reason = fmt.Sprintf("title is too different (best title score %.2f)", best.TitleScore)
	case best.ArtistScore < e.Matcher.MinArtist:
		unmatched.Reason = fmt.Sprintf("artist does not match (best artist score %.2f)", best.ArtistScore)
	case best.Score < e.Matcher.Threshold:
		unmatched.Reason = fmt.Sprintf("best score %.2f is below threshold %.2f", best.Score, e.Matcher.Threshold)
	default:
		unmatched.Reason = "duration differs too much"
	}
	return unmatched
}

func (e *Engine) resolveArtist(ctx context.Context, src domain.Artist, known *artistIndex, knownIDs map[string]bool) (domain.Artist, bool, *UnmatchedArtist, error) {
	if cached, ok := e.Store.LookupArtist(src.Provider, e.Target.Name(), src.ID); ok && cached.TargetID != "" {
		artist := domain.Artist{Provider: e.Target.Name(), ID: cached.TargetID, Name: cached.Name}
		return artist, knownIDs[artist.ID], nil, nil
	}
	if known != nil {
		if best, ok := e.Matcher.BestArtist(src, known.lookup(src.Name)); ok {
			e.Store.PutArtist(src.Provider, e.Target.Name(), src.ID, best.Artist.ID, best.Artist.Name)
			return best.Artist, true, nil, nil
		}
	}
	candidates, err := e.Target.SearchArtists(ctx, src, 10)
	if err != nil {
		return domain.Artist{}, false, nil, err
	}
	best, ok := e.Matcher.BestArtist(src, candidates)
	if ok {
		e.Store.PutArtist(src.Provider, e.Target.Name(), src.ID, best.Artist.ID, best.Artist.Name)
		return best.Artist, false, nil, nil
	}
	unmatched := &UnmatchedArtist{Source: src, Reason: "no confident artist match"}
	if best.Score > 0 {
		unmatched.Best = &best
		unmatched.Reason = fmt.Sprintf("best score %.2f is below threshold %.2f", best.Score, matcher.DefaultArtistThreshold)
	}
	return domain.Artist{}, false, unmatched, nil
}

// trackIndex maps normalized titles to tracks so tracks already present in the
// target can be recognized without a search request.
type trackIndex struct {
	byKey map[string][]domain.Track
}

func newTrackIndex(tracks []domain.Track) *trackIndex {
	idx := &trackIndex{byKey: map[string][]domain.Track{}}
	for _, t := range tracks {
		if t.ID == "" {
			continue
		}
		seen := map[string]bool{}
		for _, key := range matcher.TitleKeys(t.Title) {
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			idx.byKey[key] = append(idx.byKey[key], t)
		}
	}
	return idx
}

func (i *trackIndex) lookup(title string) []domain.Track {
	if i == nil {
		return nil
	}
	var result []domain.Track
	seen := map[string]bool{}
	for _, key := range matcher.TitleKeys(title) {
		for _, t := range i.byKey[key] {
			if seen[t.ID] {
				continue
			}
			seen[t.ID] = true
			result = append(result, t)
		}
	}
	return result
}

type artistIndex struct {
	byKey map[string][]domain.Artist
}

func newArtistIndex(artists []domain.Artist) *artistIndex {
	idx := &artistIndex{byKey: map[string][]domain.Artist{}}
	for _, a := range artists {
		if a.ID == "" {
			continue
		}
		for _, key := range matcher.ArtistKeys(a.Name) {
			if key == "" {
				continue
			}
			idx.byKey[key] = append(idx.byKey[key], a)
		}
	}
	return idx
}

func (i *artistIndex) lookup(name string) []domain.Artist {
	if i == nil {
		return nil
	}
	var result []domain.Artist
	seen := map[string]bool{}
	for _, key := range matcher.ArtistKeys(name) {
		for _, a := range i.byKey[key] {
			if seen[a.ID] {
				continue
			}
			seen[a.ID] = true
			result = append(result, a)
		}
	}
	return result
}

func targetPlaylistName(prefix, name string) string {
	value := strings.TrimSpace(prefix + name)
	if value == "" {
		value = "Imported playlist"
	}
	return truncateText(value, 100)
}

// favoritesPlaylistName names the playlist that receives liked tracks. The
// source service is used so the playlist is not confused with the target's
// native favorites.
func favoritesPlaylistName(source domain.Name) string {
	switch source {
	case domain.Yandex:
		return "Yandex favorites"
	case domain.Spotify:
		return "Spotify favorites"
	default:
		return source.DisplayName() + " favorites"
	}
}

func truncateText(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max])
}
