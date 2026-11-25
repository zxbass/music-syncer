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
	"github.com/zxbass/music-syncer/internal/httpx"
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
	// SearchBatch caps the number of new catalog lookups per run. Zero means
	// unlimited. Cached matches do not consume the budget, so repeated runs
	// make forward progress without exhausting a provider's daily quota.
	SearchBatch int
	// MigrateLikes moves matched source likes from the target's native liked
	// tracks into the favorites playlist. It is only used by the
	// migrate-likes command.
	MigrateLikes bool
}

// TrackStats summarizes a track collection sync.
type TrackStats struct {
	Source    int `json:"source"`
	Existing  int `json:"existing"`
	Added     int `json:"added"`
	Unmatched int `json:"unmatched"`
	Deferred  int `json:"deferred,omitempty"`
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
	Deferred  int `json:"deferred,omitempty"`
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
	StartedAt              time.Time            `json:"started_at"`
	FinishedAt             time.Time            `json:"finished_at"`
	Source                 domain.Name          `json:"source"`
	Target                 domain.Name          `json:"target"`
	DryRun                 bool                 `json:"dry_run"`
	Categories             string               `json:"categories"`
	SearchBatch            int                  `json:"search_batch,omitempty"`
	BatchLimitReached      bool                 `json:"batch_limit_reached,omitempty"`
	Playlists              []PlaylistResult     `json:"playlists,omitempty"`
	Likes                  *TrackStats          `json:"likes,omitempty"`
	LikesPlaylist          *LikesPlaylistResult `json:"likes_playlist,omitempty"`
	NativeLikesRemoved     int                  `json:"native_likes_removed,omitempty"`
	NativeLikesWouldRemove int                  `json:"native_likes_would_remove,omitempty"`
	Artists                *ArtistStats         `json:"artists,omitempty"`
	Errors                 []string             `json:"errors,omitempty"`
	UnmatchedLikes         []Unmatched          `json:"unmatched_likes,omitempty"`
	UnmatchedArtists       []UnmatchedArtist    `json:"unmatched_artists,omitempty"`
}

// Engine transfers a library between two providers.
type Engine struct {
	Source  domain.Provider
	Target  domain.Provider
	Matcher *matcher.Matcher
	Store   *store.Store
	Log     *slog.Logger
	Options Options

	report       *Report
	unsaved      int
	misses       map[string]*Unmatched
	searchBudget int
	deferred     bool
}

// stateCheckpointEvery bounds how many newly resolved match/miss decisions can
// be lost if the process is killed before the final state save. On a quota or
// rate limit the engine aborts and saves immediately, but a hard crash or
// power loss must not force the next run to repeat all catalog searches.
const stateCheckpointEvery = 50

// missTTL controls how long an unmatched catalog lookup is remembered. It
// keeps batched runs moving forward instead of spending the whole search
// budget on the same unmatched tracks, while still re-checking the catalog
// periodically.
const missTTL = 7 * 24 * time.Hour

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
	if e.Options.SearchBatch < 0 {
		return nil, errors.New("syncer: search batch cannot be negative")
	}
	if e.Options.MigrateLikes && !e.Options.Categories.Likes {
		return nil, errors.New("syncer: migrate likes requires the likes category")
	}
	if e.Log == nil {
		e.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	e.report = &Report{
		StartedAt:   time.Now(),
		Source:      e.Source.Name(),
		Target:      e.Target.Name(),
		DryRun:      e.Options.DryRun,
		Categories:  e.Options.Categories.String(),
		SearchBatch: e.Options.SearchBatch,
	}
	e.misses = make(map[string]*Unmatched)
	e.searchBudget = e.Options.SearchBatch
	e.deferred = false

	var errs []error
	if !e.deferred && e.Options.Categories.Playlists {
		if err := e.syncPlaylists(ctx); err != nil {
			if isFatal(err) {
				return e.abort(err)
			}
			e.recordError("playlists", err)
			errs = append(errs, err)
		}
	}
	if !e.deferred && e.Options.Categories.Likes {
		var likeErr error
		if e.Options.MigrateLikes {
			likeErr = e.migrateLikes(ctx)
		} else {
			likeErr = e.syncLikes(ctx)
		}
		if likeErr != nil {
			if isFatal(likeErr) {
				return e.abort(likeErr)
			}
			e.recordError("liked tracks", likeErr)
			errs = append(errs, likeErr)
		}
	}
	if !e.deferred && e.Options.Categories.Artists {
		if err := e.syncArtists(ctx); err != nil {
			if isFatal(err) {
				return e.abort(err)
			}
			e.recordError("artists", err)
			errs = append(errs, err)
		}
	}

	e.report.BatchLimitReached = e.deferred
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
	e.recordError("transfer aborted", err)
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

// takeSearch reserves one new catalog lookup. When the configured batch is
// spent the engine stops early and leaves the remaining work for the next run.
func (e *Engine) takeSearch() bool {
	if e.Options.SearchBatch <= 0 {
		return true
	}
	if e.searchBudget <= 0 {
		e.deferred = true
		return false
	}
	e.searchBudget--
	return true
}

// checkpoint saves the state periodically so a killed process does not lose
// all match and miss decisions made since the start of the run.
func (e *Engine) checkpoint() {
	e.unsaved++
	if e.unsaved < stateCheckpointEvery {
		return
	}
	e.unsaved = 0
	if err := e.Store.Save(); err != nil {
		e.recordError("save state", err)
	}
}

// linkPlaylist stores a source -> target playlist link immediately and saves
// the state. The link must exist before tracks are added: if the process is
// interrupted, the next run must reuse the same target playlist instead of
// creating a second one.
func (e *Engine) linkPlaylist(srcID string, target domain.Playlist) {
	if e.Options.DryRun || target.ID == "" {
		return
	}
	e.Store.PutPlaylist(e.Source.Name(), e.Target.Name(), srcID, target.ID, target.Name)
	if err := e.Store.Save(); err != nil {
		e.recordError("save state", err)
	}
}

// errSearchBatch signals that the per-run catalog search budget is spent. It
// is not an error: the caller stops the current collection, saves progress and
// lets the next run continue from the cached matches.
var errSearchBatch = errors.New("search batch limit reached")

func isFatal(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, httpx.ErrRateLimited) ||
		errors.Is(err, httpx.ErrQuotaExceeded) ||
		domain.IsUnauthorized(err)
}

// targetTrackBatch is the maximum number of tracks written by one AddTracks
// call. Writing in batches keeps progress visible and lets a resumed run skip
// the tracks that were already written.
const targetTrackBatch = 100

// addTracks writes tracks in provider-sized batches and updates stats as each
// batch succeeds. A failure stops the remaining batches; the next run reloads
// the target playlist and only adds what is still missing.
func (e *Engine) addTracks(ctx context.Context, target domain.Playlist, tracks []domain.Track, stats *TrackStats) error {
	if len(tracks) == 0 {
		return nil
	}
	if e.Options.DryRun {
		stats.Added += len(tracks)
		return nil
	}
	for start := 0; start < len(tracks); start += targetTrackBatch {
		end := min(start+targetTrackBatch, len(tracks))
		if err := e.Target.AddTracks(ctx, target, tracks[start:end]); err != nil {
			stats.Failed += len(tracks) - start
			return err
		}
		stats.Added += end - start
	}
	return nil
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

	// A track already present in the target collection is a free, exact
	// match. Check it before cached misses: a miss from another collection or
	// an earlier run must not hide a track that is now available here.
	if known != nil {
		if best, ok := e.Matcher.Best(src, known.lookup(src.Title)); ok {
			e.Store.PutMatch(src.Provider, e.Target.Name(), src.ID, best.Track, best.Score)
			e.checkpoint()
			return resolution{Track: best.Track, Confidence: best.Score, Existing: true}, nil
		}
	}

	if src.ID != "" && e.misses != nil {
		if miss, ok := e.misses[src.Key()]; ok {
			return resolution{Unmatched: miss}, nil
		}
	}
	if src.ID != "" {
		if miss, ok := e.Store.LookupMiss(src.Provider, e.Target.Name(), src.ID); ok &&
			time.Since(miss.UpdatedAt) < missTTL {
			unmatched := &Unmatched{Source: src, Reason: miss.Reason}
			if e.misses != nil {
				e.misses[src.Key()] = unmatched
			}
			return resolution{Unmatched: unmatched}, nil
		}
	}

	if !e.takeSearch() {
		return resolution{}, errSearchBatch
	}
	candidates, err := e.Target.SearchTracks(ctx, src, 10)
	if err != nil {
		return resolution{}, err
	}
	ranked := e.Matcher.Rank(src, candidates)
	best, ok := e.Matcher.Best(src, candidates)
	if ok {
		e.Store.PutMatch(src.Provider, e.Target.Name(), src.ID, best.Track, best.Score)
		e.checkpoint()
		return resolution{
			Track:      best.Track,
			Confidence: best.Score,
			Existing:   knownIDs[best.Track.ID],
		}, nil
	}

	unmatched := e.unmatchedTrack(src, ranked)
	if src.ID != "" {
		if e.misses == nil {
			e.misses = make(map[string]*Unmatched)
		}
		e.misses[src.Key()] = unmatched
		e.Store.PutMiss(src.Provider, e.Target.Name(), src.ID, unmatched.Reason)
		e.checkpoint()
	}
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
			e.checkpoint()
			return best.Artist, true, nil, nil
		}
	}
	if src.ID != "" {
		if miss, ok := e.Store.LookupArtistMiss(src.Provider, e.Target.Name(), src.ID); ok &&
			time.Since(miss.UpdatedAt) < missTTL {
			return domain.Artist{}, false, &UnmatchedArtist{Source: src, Reason: miss.Reason}, nil
		}
	}
	if !e.takeSearch() {
		return domain.Artist{}, false, nil, errSearchBatch
	}
	candidates, err := e.Target.SearchArtists(ctx, src, 10)
	if err != nil {
		return domain.Artist{}, false, nil, err
	}
	best, ok := e.Matcher.BestArtist(src, candidates)
	if ok {
		e.Store.PutArtist(src.Provider, e.Target.Name(), src.ID, best.Artist.ID, best.Artist.Name)
		e.checkpoint()
		return best.Artist, false, nil, nil
	}
	unmatched := &UnmatchedArtist{Source: src, Reason: "no confident artist match"}
	if best.Score > 0 {
		unmatched.Best = &best
		unmatched.Reason = fmt.Sprintf("best score %.2f is below threshold %.2f", best.Score, matcher.DefaultArtistThreshold)
	}
	if src.ID != "" {
		e.Store.PutArtistMiss(src.Provider, e.Target.Name(), src.ID, unmatched.Reason)
		e.checkpoint()
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

// FavoritesPlaylistName names the playlist that receives liked tracks. The
// source service is used so the playlist is not confused with the target's
// native favorites.
func FavoritesPlaylistName(source domain.Name) string {
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
