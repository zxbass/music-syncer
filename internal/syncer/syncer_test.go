package syncer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zxbass/music-syncer/internal/domain"
	"github.com/zxbass/music-syncer/internal/matcher"
	"github.com/zxbass/music-syncer/internal/store"
)

type fakeProvider struct {
	name domain.Name

	playlists      []domain.Playlist
	playlistTracks map[string][]domain.Track
	liked          []domain.Track
	artists        []domain.Artist
	history        []domain.PlayEvent

	catalog       []domain.Track
	artistCatalog []domain.Artist
	searchErr     error
	searchCalls   int

	created   []string
	added     []domain.Track
	removed   []domain.Track
	likedAdd  []domain.Track
	followAdd []domain.Artist
}

func newFake(name domain.Name) *fakeProvider {
	return &fakeProvider{
		name:           name,
		playlistTracks: map[string][]domain.Track{},
	}
}

func (f *fakeProvider) Name() domain.Name { return f.name }

func (f *fakeProvider) Capabilities() domain.Capabilities {
	return domain.Capabilities{
		Playlists:      true,
		LikedTracks:    true,
		FollowedArtist: true,
		HistoryRead:    true,
	}
}

func (f *fakeProvider) Account(context.Context) (domain.Account, error) {
	return domain.Account{Provider: f.name, ID: "uid", DisplayName: string(f.name)}, nil
}

func (f *fakeProvider) Playlists(context.Context) ([]domain.Playlist, error) {
	return append([]domain.Playlist(nil), f.playlists...), nil
}

func (f *fakeProvider) PlaylistTracks(_ context.Context, pl domain.Playlist) ([]domain.Track, error) {
	return append([]domain.Track(nil), f.playlistTracks[pl.ID]...), nil
}

func (f *fakeProvider) LikedTracks(_ context.Context, limit int) ([]domain.Track, error) {
	result := append([]domain.Track(nil), f.liked...)
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (f *fakeProvider) LikedArtists(_ context.Context, limit int) ([]domain.Artist, error) {
	result := append([]domain.Artist(nil), f.artists...)
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (f *fakeProvider) History(_ context.Context, since time.Time, limit int) ([]domain.PlayEvent, error) {
	var result []domain.PlayEvent
	for _, ev := range f.history {
		if !since.IsZero() && ev.PlayedAt.Before(since) {
			continue
		}
		result = append(result, ev)
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	return result, nil
}

func (f *fakeProvider) SearchTracks(context.Context, domain.Track, int) ([]domain.Track, error) {
	f.searchCalls++
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return append([]domain.Track(nil), f.catalog...), nil
}

func (f *fakeProvider) SearchArtists(context.Context, domain.Artist, int) ([]domain.Artist, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return append([]domain.Artist(nil), f.artistCatalog...), nil
}

func (f *fakeProvider) CreatePlaylist(_ context.Context, name, _ string) (domain.Playlist, error) {
	id := fmt.Sprintf("%s-created-%d", f.name, len(f.playlists)+1)
	pl := domain.Playlist{Provider: f.name, ID: id, Name: name}
	f.playlists = append(f.playlists, pl)
	f.playlistTracks[id] = nil
	f.created = append(f.created, name)
	return pl, nil
}

func (f *fakeProvider) AddTracks(_ context.Context, pl domain.Playlist, tracks []domain.Track) error {
	f.added = append(f.added, tracks...)
	f.playlistTracks[pl.ID] = append(f.playlistTracks[pl.ID], tracks...)
	for i := range f.playlists {
		if f.playlists[i].ID == pl.ID {
			f.playlists[i].TrackCount = len(f.playlistTracks[pl.ID])
		}
	}
	return nil
}

func (f *fakeProvider) RemoveTracks(_ context.Context, pl domain.Playlist, tracks []domain.Track) error {
	remove := map[string]bool{}
	for _, t := range tracks {
		remove[t.ID] = true
	}
	var kept []domain.Track
	for _, t := range f.playlistTracks[pl.ID] {
		if remove[t.ID] {
			f.removed = append(f.removed, t)
			continue
		}
		kept = append(kept, t)
	}
	f.playlistTracks[pl.ID] = kept
	for i := range f.playlists {
		if f.playlists[i].ID == pl.ID {
			f.playlists[i].TrackCount = len(kept)
		}
	}
	return nil
}

func (f *fakeProvider) LikeTracks(_ context.Context, tracks []domain.Track) error {
	f.likedAdd = append(f.likedAdd, tracks...)
	f.liked = append(f.liked, tracks...)
	return nil
}

func (f *fakeProvider) FollowArtists(_ context.Context, artists []domain.Artist) error {
	f.followAdd = append(f.followAdd, artists...)
	f.artists = append(f.artists, artists...)
	return nil
}

func (f *fakeProvider) addPlaylist(name string, tracks ...domain.Track) domain.Playlist {
	id := fmt.Sprintf("%s-pl-%d", f.name, len(f.playlists)+1)
	pl := domain.Playlist{Provider: f.name, ID: id, Name: name, TrackCount: len(tracks)}
	f.playlists = append(f.playlists, pl)
	f.playlistTracks[id] = append([]domain.Track(nil), tracks...)
	return pl
}

func track(provider domain.Name, id, title, artist string, duration int) domain.Track {
	return domain.Track{
		Provider: provider, ID: id, AlbumID: "album-" + id,
		Title: title, Artists: []string{artist}, Album: "Album", DurationMS: duration,
	}
}

func newTestEngine(t *testing.T, src, dst domain.Provider, opts Options) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{
		Source:  src,
		Target:  dst,
		Matcher: matcher.New(0),
		Store:   st,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Options: opts,
	}, st
}

func TestPlaylistTransferAndIdempotency(t *testing.T) {
	src := newFake(domain.Yandex)
	src.addPlaylist("Road Trip",
		track(domain.Yandex, "y1", "Blinding Lights", "The Weeknd", 200040),
		track(domain.Yandex, "y2", "Песня без слов", "Кино", 265000),
		track(domain.Yandex, "y3", "Unknown Obscure Song", "Nobody", 123000),
	)
	src.addPlaylist("Плейлист дня",
		track(domain.Yandex, "g1", "Generated", "Someone", 100000),
	)
	src.playlists[1].Generated = true

	dst := newFake(domain.Spotify)
	dst.catalog = []domain.Track{
		track(domain.Spotify, "s1", "Blinding Lights", "The Weeknd", 200040),
		track(domain.Spotify, "s2", "Pesnya bez slov", "Kino", 265000),
	}

	engine, _ := newTestEngine(t, src, dst, Options{
		Categories: Categories{Playlists: true},
	})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Playlists) != 1 {
		t.Fatalf("got %d playlist results, want 1 (generated playlist must be skipped)", len(report.Playlists))
	}
	res := report.Playlists[0]
	if !res.Created {
		t.Error("expected a new target playlist")
	}
	if res.Stats.Source != 3 || res.Stats.Added != 2 || res.Stats.Unmatched != 1 {
		t.Errorf("stats = %+v, want source 3, added 2, unmatched 1", res.Stats)
	}
	if len(dst.created) != 1 || dst.created[0] != "Road Trip" {
		t.Errorf("created = %v", dst.created)
	}
	got := dst.playlistTracks[res.TargetID]
	if len(got) != 2 {
		t.Fatalf("target playlist has %d tracks, want 2: %+v", len(got), got)
	}

	// Second run: the same link must be reused and nothing must be added.
	engine2, _ := newTestEngine(t, src, dst, Options{Categories: Categories{Playlists: true}})
	report2, err := engine2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	res2 := report2.Playlists[0]
	if res2.Created {
		t.Error("second run created another playlist")
	}
	if res2.Stats.Added != 0 || res2.Stats.Existing != 2 || res2.Stats.Unmatched != 1 {
		t.Errorf("second run stats = %+v, want added 0, existing 2, unmatched 1", res2.Stats)
	}
	if len(dst.created) != 1 {
		t.Errorf("created %d playlists, want 1", len(dst.created))
	}
}

func TestPlaylistDryRun(t *testing.T) {
	src := newFake(domain.Yandex)
	src.addPlaylist("Road Trip", track(domain.Yandex, "y1", "Song", "Artist", 180000))
	dst := newFake(domain.Spotify)
	dst.catalog = []domain.Track{track(domain.Spotify, "s1", "Song", "Artist", 180000)}

	engine, st := newTestEngine(t, src, dst, Options{
		Categories: Categories{Playlists: true},
		DryRun:     true,
	})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	res := report.Playlists[0]
	if !res.WouldCreate || res.Stats.Added != 1 {
		t.Errorf("dry-run result = %+v, want would-create and one planned add", res)
	}
	if len(dst.playlists) != 0 || len(dst.added) != 0 {
		t.Errorf("dry run wrote to target: playlists=%d added=%d", len(dst.playlists), len(dst.added))
	}
	if _, ok := st.LookupPlaylist(src.Name(), dst.Name(), "yandex-pl-1"); ok {
		t.Error("dry run stored a playlist link")
	}
}

func TestPlaylistMirror(t *testing.T) {
	src := newFake(domain.Yandex)
	src.addPlaylist("Road Trip",
		track(domain.Yandex, "y1", "Song One", "Artist", 180000),
		track(domain.Yandex, "y2", "Song Two", "Artist", 180000),
	)
	dst := newFake(domain.Spotify)
	dst.catalog = []domain.Track{
		track(domain.Spotify, "s1", "Song One", "Artist", 180000),
		track(domain.Spotify, "s2", "Song Two", "Artist", 180000),
	}
	existing := dst.addPlaylist("Road Trip",
		track(domain.Spotify, "s1", "Song One", "Artist", 180000),
		track(domain.Spotify, "s2", "Song Two", "Artist", 180000),
		track(domain.Spotify, "extra", "Extra Song", "Artist", 180000),
	)

	engine, _ := newTestEngine(t, src, dst, Options{
		Categories: Categories{Playlists: true},
		Mirror:     true,
	})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	res := report.Playlists[0]
	if res.Removed != 1 {
		t.Errorf("removed = %d, want 1", res.Removed)
	}
	if len(dst.playlistTracks[existing.ID]) != 2 {
		t.Errorf("target playlist has %d tracks, want 2", len(dst.playlistTracks[existing.ID]))
	}
}

func TestPlaylistMirrorSkippedOnUnmatched(t *testing.T) {
	src := newFake(domain.Yandex)
	src.addPlaylist("Road Trip",
		track(domain.Yandex, "y1", "Song One", "Artist", 180000),
		track(domain.Yandex, "y2", "Unmatchable Song", "Nobody", 10000),
	)
	dst := newFake(domain.Spotify)
	dst.catalog = []domain.Track{track(domain.Spotify, "s1", "Song One", "Artist", 180000)}
	existing := dst.addPlaylist("Road Trip",
		track(domain.Spotify, "s1", "Song One", "Artist", 180000),
		track(domain.Spotify, "extra", "Extra Song", "Artist", 180000),
	)

	engine, _ := newTestEngine(t, src, dst, Options{
		Categories: Categories{Playlists: true},
		Mirror:     true,
	})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	res := report.Playlists[0]
	if res.Removed != 0 {
		t.Errorf("removed = %d, want 0 when a source track is unmatched", res.Removed)
	}
	if len(dst.playlistTracks[existing.ID]) != 2 {
		t.Errorf("extra track was removed despite unmatched source track")
	}
	foundNote := false
	for _, note := range res.Notes {
		if strings.Contains(note, "mirror skipped") {
			foundNote = true
		}
	}
	if !foundNote {
		t.Errorf("missing mirror-skipped note: %v", res.Notes)
	}
}

func TestPlaylistNameFallback(t *testing.T) {
	src := newFake(domain.Yandex)
	src.addPlaylist("Road Trip", track(domain.Yandex, "y1", "Song One", "Artist", 180000))
	dst := newFake(domain.Spotify)
	dst.catalog = []domain.Track{track(domain.Spotify, "s1", "Song One", "Artist", 180000)}
	existing := dst.addPlaylist("Road Trip")

	engine, st := newTestEngine(t, src, dst, Options{Categories: Categories{Playlists: true}})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Playlists[0].Created {
		t.Error("expected the existing same-name playlist to be reused")
	}
	if len(dst.playlists) != 1 {
		t.Errorf("created a duplicate playlist")
	}
	if len(dst.playlistTracks[existing.ID]) != 1 {
		t.Errorf("track was not added to the existing playlist")
	}
	if _, ok := st.LookupPlaylist(src.Name(), dst.Name(), src.playlists[0].ID); !ok {
		t.Error("link was not stored")
	}
}

func TestLikesAndTransliterationIndex(t *testing.T) {
	src := newFake(domain.Yandex)
	src.liked = []domain.Track{
		track(domain.Yandex, "y1", "Песня без слов", "Кино", 265000),
	}
	dst := newFake(domain.Spotify)
	dst.liked = []domain.Track{
		track(domain.Spotify, "s1", "Pesnya bez slov", "Kino", 265000),
	}
	engine, _ := newTestEngine(t, src, dst, Options{Categories: Categories{Likes: true}})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Likes.Existing != 1 || report.Likes.Added != 0 {
		t.Errorf("likes stats = %+v, want existing 1, added 0", report.Likes)
	}
	if dst.searchCalls != 0 {
		t.Errorf("search called %d times despite a matching liked track", dst.searchCalls)
	}
}

func TestLikesAddAndIdempotency(t *testing.T) {
	src := newFake(domain.Yandex)
	src.liked = []domain.Track{
		track(domain.Yandex, "y1", "Song One", "Artist", 180000),
		track(domain.Yandex, "y2", "Song Two", "Artist", 180000),
	}
	dst := newFake(domain.Spotify)
	dst.liked = []domain.Track{track(domain.Spotify, "s1", "Song One", "Artist", 180000)}
	dst.catalog = []domain.Track{track(domain.Spotify, "s2", "Song Two", "Artist", 180000)}

	engine, _ := newTestEngine(t, src, dst, Options{Categories: Categories{Likes: true}})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Likes.Added != 1 || report.Likes.Existing != 1 {
		t.Errorf("stats = %+v, want added 1, existing 1", report.Likes)
	}
	if len(dst.likedAdd) != 1 || dst.likedAdd[0].ID != "s2" {
		t.Errorf("likedAdd = %+v", dst.likedAdd)
	}

	engine2, _ := newTestEngine(t, src, dst, Options{Categories: Categories{Likes: true}})
	report2, err := engine2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report2.Likes.Added != 0 || report2.Likes.Existing != 2 {
		t.Errorf("second run stats = %+v, want added 0, existing 2", report2.Likes)
	}
	if len(dst.likedAdd) != 1 {
		t.Errorf("second run added %d tracks", len(dst.likedAdd))
	}
}

func TestArtists(t *testing.T) {
	src := newFake(domain.Yandex)
	src.artists = []domain.Artist{
		{Provider: domain.Yandex, ID: "ya1", Name: "Кино"},
		{Provider: domain.Yandex, ID: "ya2", Name: "Мумий Тролль"},
	}
	dst := newFake(domain.Spotify)
	dst.artists = []domain.Artist{{Provider: domain.Spotify, ID: "sa1", Name: "Kino"}}
	dst.artistCatalog = []domain.Artist{{Provider: domain.Spotify, ID: "sa2", Name: "Mumiy Troll"}}

	engine, _ := newTestEngine(t, src, dst, Options{Categories: Categories{Artists: true}})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Artists.Added != 1 || report.Artists.Existing != 1 {
		t.Errorf("stats = %+v, want added 1, existing 1", report.Artists)
	}
	if len(dst.followAdd) != 1 || dst.followAdd[0].ID != "sa2" {
		t.Errorf("followAdd = %+v", dst.followAdd)
	}

	engine2, _ := newTestEngine(t, src, dst, Options{Categories: Categories{Artists: true}})
	report2, err := engine2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report2.Artists.Added != 0 || report2.Artists.Existing != 2 {
		t.Errorf("second run stats = %+v", report2.Artists)
	}
}

func TestHistoryArchive(t *testing.T) {
	now := time.Now()
	src := newFake(domain.Yandex)
	src.history = []domain.PlayEvent{
		{Track: track(domain.Yandex, "y1", "Song One", "Artist", 180000), PlayedAt: now.Add(-2 * time.Hour)},
		{Track: track(domain.Yandex, "y2", "Song Two", "Artist", 180000), PlayedAt: now.Add(-time.Hour)},
		{Track: track(domain.Yandex, "y1", "Song One", "Artist", 180000), PlayedAt: now.Add(-30 * time.Minute)},
	}
	dst := newFake(domain.Spotify)
	dst.catalog = []domain.Track{
		track(domain.Spotify, "s1", "Song One", "Artist", 180000),
		track(domain.Spotify, "s2", "Song Two", "Artist", 180000),
	}
	engine, _ := newTestEngine(t, src, dst, Options{Categories: Categories{History: true}})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.History.Events != 3 || report.History.UniqueTracks != 2 {
		t.Errorf("history = %+v, want 3 events, 2 unique tracks", report.History)
	}
	if report.History.Stats.Added != 2 {
		t.Errorf("added = %d, want 2", report.History.Stats.Added)
	}
	if len(dst.created) != 1 || !strings.Contains(dst.created[0], "History from") {
		t.Errorf("created = %v", dst.created)
	}
	if len(report.History.Notes) == 0 {
		t.Error("expected a note about read-only history")
	}
}

func TestLimit(t *testing.T) {
	src := newFake(domain.Yandex)
	src.addPlaylist("Road Trip",
		track(domain.Yandex, "y1", "Song One", "Artist", 180000),
		track(domain.Yandex, "y2", "Song Two", "Artist", 180000),
	)
	dst := newFake(domain.Spotify)
	dst.catalog = []domain.Track{
		track(domain.Spotify, "s1", "Song One", "Artist", 180000),
		track(domain.Spotify, "s2", "Song Two", "Artist", 180000),
	}
	engine, _ := newTestEngine(t, src, dst, Options{Categories: Categories{Playlists: true}, Limit: 1})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Playlists[0].Stats.Source != 1 || report.Playlists[0].Stats.Added != 1 {
		t.Errorf("stats = %+v, want one source track and one add", report.Playlists[0].Stats)
	}
}

func TestMirrorSkippedWhenLimited(t *testing.T) {
	src := newFake(domain.Yandex)
	src.addPlaylist("Road Trip",
		track(domain.Yandex, "y1", "Song One", "Artist", 180000),
		track(domain.Yandex, "y2", "Song Two", "Artist", 180000),
	)
	dst := newFake(domain.Spotify)
	dst.catalog = []domain.Track{
		track(domain.Spotify, "s1", "Song One", "Artist", 180000),
		track(domain.Spotify, "s2", "Song Two", "Artist", 180000),
	}
	existing := dst.addPlaylist("Road Trip",
		track(domain.Spotify, "s1", "Song One", "Artist", 180000),
		track(domain.Spotify, "s2", "Song Two", "Artist", 180000),
	)
	engine, _ := newTestEngine(t, src, dst, Options{
		Categories: Categories{Playlists: true},
		Mirror:     true,
		Limit:      1,
	})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Playlists[0].Removed != 0 {
		t.Errorf("removed = %d, want 0 with --limit", report.Playlists[0].Removed)
	}
	if len(dst.playlistTracks[existing.ID]) != 2 {
		t.Error("mirror removed tracks despite a truncated source")
	}
}

func TestFatalAuthErrorStopsRun(t *testing.T) {
	src := newFake(domain.Yandex)
	src.liked = []domain.Track{track(domain.Yandex, "y1", "Song", "Artist", 180000)}
	dst := newFake(domain.Spotify)
	dst.searchErr = &domain.APIError{Provider: domain.Spotify, StatusCode: 401, Message: "expired"}
	engine, _ := newTestEngine(t, src, dst, Options{Categories: Categories{Likes: true}})
	_, err := engine.Run(context.Background())
	if !domain.IsUnauthorized(err) {
		t.Fatalf("err = %v, want unauthorized", err)
	}
}

func TestParseCategories(t *testing.T) {
	c, err := ParseCategories("playlists,likes,history")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Playlists || !c.Likes || c.Artists || !c.History {
		t.Errorf("parsed categories = %+v", c)
	}
	if _, err := ParseCategories("nope"); err == nil {
		t.Error("expected an error for an unknown category")
	}
}
