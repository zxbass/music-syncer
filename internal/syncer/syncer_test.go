package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zxbass/music-syncer/internal/domain"
	"github.com/zxbass/music-syncer/internal/httpx"
	"github.com/zxbass/music-syncer/internal/matcher"
	"github.com/zxbass/music-syncer/internal/store"
)

type fakeProvider struct {
	name domain.Name

	playlists      []domain.Playlist
	playlistTracks map[string][]domain.Track
	liked          []domain.Track
	artists        []domain.Artist

	catalog           []domain.Track
	artistCatalog     []domain.Artist
	searchErr         error
	searchCalls       int
	artistSearchCalls int
	addErr            error
	unlikeErr         error

	created     []string
	added       []domain.Track
	removed     []domain.Track
	likedAdd    []domain.Track
	likedRemove []domain.Track
	followAdd   []domain.Artist
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

func (f *fakeProvider) SearchTracks(context.Context, domain.Track, int) ([]domain.Track, error) {
	f.searchCalls++
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return append([]domain.Track(nil), f.catalog...), nil
}

func (f *fakeProvider) SearchArtists(context.Context, domain.Artist, int) ([]domain.Artist, error) {
	f.artistSearchCalls++
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
	if f.addErr != nil {
		return f.addErr
	}
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

func (f *fakeProvider) UnlikeTracks(_ context.Context, tracks []domain.Track) error {
	if f.unlikeErr != nil {
		return f.unlikeErr
	}
	remove := map[string]bool{}
	for _, t := range tracks {
		remove[t.ID] = true
	}
	kept := f.liked[:0]
	for _, t := range f.liked {
		if remove[t.ID] {
			continue
		}
		kept = append(kept, t)
	}
	f.liked = kept
	f.likedRemove = append(f.likedRemove, tracks...)
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

func TestPlaylistLinkIsSavedBeforeAddFailure(t *testing.T) {
	src := newFake(domain.Yandex)
	pl := src.addPlaylist("Road Trip", track(domain.Yandex, "y1", "Song", "Artist", 180000))
	dst := newFake(domain.Spotify)
	dst.catalog = []domain.Track{track(domain.Spotify, "s1", "Song", "Artist", 180000)}
	dst.addErr = context.Canceled

	engine, st := newTestEngine(t, src, dst, Options{Categories: Categories{Playlists: true}})
	_, err := engine.Run(context.Background())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	link, ok := st.LookupPlaylist(src.Name(), dst.Name(), pl.ID)
	if !ok || link.TargetID == "" {
		t.Fatalf("playlist link was not saved before adding tracks: %+v", link)
	}
	if len(dst.playlists) != 1 {
		t.Errorf("target has %d playlists, want exactly one", len(dst.playlists))
	}
}

func TestResolvedMatchesAreCheckpointed(t *testing.T) {
	src := newFake(domain.Yandex)
	dst := newFake(domain.Spotify)
	dst.catalog = []domain.Track{track(domain.Spotify, "s1", "Song", "Artist", 180000)}
	engine, st := newTestEngine(t, src, dst, Options{})
	engine.report = &Report{}

	for i := 0; i < stateCheckpointEvery+5; i++ {
		srcTrack := track(domain.Yandex, fmt.Sprintf("y%d", i), "Song", "Artist", 180000)
		if _, err := engine.resolve(context.Background(), srcTrack, nil, nil); err != nil {
			t.Fatal(err)
		}
	}

	reopened, err := store.Open(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Stats().Matches; got != stateCheckpointEvery {
		t.Errorf("persisted matches = %d, want %d after a checkpoint", got, stateCheckpointEvery)
	}
}

func TestLinkedTargetIsNotReusedByAnotherSourcePlaylist(t *testing.T) {
	src := newFake(domain.Yandex)
	src.addPlaylist("Shared name", track(domain.Yandex, "y1", "Song One", "Artist", 180000))
	src.addPlaylist("Shared name", track(domain.Yandex, "y2", "Song Two", "Artist", 180000))
	dst := newFake(domain.Spotify)
	dst.catalog = []domain.Track{
		track(domain.Spotify, "s1", "Song One", "Artist", 180000),
		track(domain.Spotify, "s2", "Song Two", "Artist", 180000),
	}
	linkedTarget := dst.addPlaylist("Shared name")

	engine, st := newTestEngine(t, src, dst, Options{Categories: Categories{Playlists: true}})
	st.PutPlaylist(src.Name(), dst.Name(), src.playlists[1].ID, linkedTarget.ID, linkedTarget.Name)

	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Playlists) != 2 {
		t.Fatalf("got %d results", len(report.Playlists))
	}
	first, second := report.Playlists[0], report.Playlists[1]
	if first.TargetID == linkedTarget.ID {
		t.Fatalf("unlinked source reused a target owned by another source")
	}
	if second.TargetID != linkedTarget.ID {
		t.Errorf("linked source got target %q, want %q", second.TargetID, linkedTarget.ID)
	}
	if len(dst.created) != 1 {
		t.Errorf("created %d playlists, want 1", len(dst.created))
	}
	if got := len(dst.playlistTracks[linkedTarget.ID]); got != 1 {
		t.Errorf("linked target has %d tracks, want 1", got)
	}
	if got := len(dst.playlistTracks[first.TargetID]); got != 1 {
		t.Errorf("new target has %d tracks, want 1", got)
	}
}

func TestDuplicateTargetNamesLinkedBySourceURL(t *testing.T) {
	src := newFake(domain.Yandex)
	pl := src.addPlaylist("Data Types", track(domain.Yandex, "y1", "Song", "Artist", 180000))
	pl.URL = "https://music.yandex.ru/users/1/playlists/1020"
	src.playlists[0] = pl

	dst := newFake(domain.Spotify)
	dst.catalog = []domain.Track{track(domain.Spotify, "s1", "Song", "Artist", 180000)}
	dst.addPlaylist("Data Types")
	expected := dst.addPlaylist("Data Types")
	escapedURL := strings.ReplaceAll(pl.URL, "/", "&#x2F;")
	dst.playlists[1].Description = "Imported from Yandex Music with music-syncer; Source: " + escapedURL

	engine, _ := newTestEngine(t, src, dst, Options{Categories: Categories{Playlists: true}})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	res := report.Playlists[0]
	if res.TargetID != expected.ID {
		t.Errorf("target = %q, want the playlist with the source URL (%q)", res.TargetID, expected.ID)
	}
	if res.Created || len(dst.created) != 0 {
		t.Errorf("created %v, want reuse of the existing playlist", dst.created)
	}
	if got := len(dst.playlistTracks[expected.ID]); got != 1 {
		t.Errorf("linked target has %d tracks, want 1", got)
	}
}

func TestDuplicateSourcePlaylistNamesAreNotMerged(t *testing.T) {
	src := newFake(domain.Yandex)
	src.addPlaylist("New playlist", track(domain.Yandex, "y1", "Song One", "Artist", 180000))
	src.addPlaylist("New playlist", track(domain.Yandex, "y2", "Song Two", "Artist", 180000))
	dst := newFake(domain.Spotify)
	dst.catalog = []domain.Track{
		track(domain.Spotify, "s1", "Song One", "Artist", 180000),
		track(domain.Spotify, "s2", "Song Two", "Artist", 180000),
	}

	engine, _ := newTestEngine(t, src, dst, Options{Categories: Categories{Playlists: true}})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Playlists) != 2 {
		t.Fatalf("got %d results", len(report.Playlists))
	}
	if report.Playlists[0].TargetID == report.Playlists[1].TargetID {
		t.Fatalf("both source playlists were merged into %q", report.Playlists[0].TargetID)
	}
	if len(dst.playlists) != 2 || len(dst.created) != 2 {
		t.Errorf("created %d playlists, want 2", len(dst.created))
	}
	for _, res := range report.Playlists {
		if got := len(dst.playlistTracks[res.TargetID]); got != 1 {
			t.Errorf("target %q has %d tracks, want 1", res.TargetID, got)
		}
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

func TestPlaylistMirrorSkippedOnDeferredTracks(t *testing.T) {
	src := newFake(domain.Yandex)
	src.addPlaylist("Road Trip",
		track(domain.Yandex, "y1", "Song One", "Artist", 180000),
		track(domain.Yandex, "y2", "Song Two", "Artist", 180000),
	)
	dst := newFake(domain.Spotify)
	dst.catalog = []domain.Track{track(domain.Spotify, "s1", "Song One", "Artist", 180000)}
	existing := dst.addPlaylist("Road Trip",
		track(domain.Spotify, "extra", "Extra Song", "Artist", 180000),
	)

	engine, _ := newTestEngine(t, src, dst, Options{
		Categories:  Categories{Playlists: true},
		Mirror:      true,
		SearchBatch: 1,
	})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	res := report.Playlists[0]
	if res.Stats.Deferred == 0 {
		t.Fatal("expected deferred source tracks")
	}
	if res.Removed != 0 {
		t.Errorf("removed = %d, want 0 while source tracks are deferred", res.Removed)
	}
	if got := len(dst.playlistTracks[existing.ID]); got != 2 {
		t.Errorf("target playlist has %d tracks, want the extra track to be kept", got)
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
	// A native Spotify like must not be treated as an existing favorites
	// playlist entry.
	dst.liked = []domain.Track{
		track(domain.Spotify, "s1", "Pesnya bez slov", "Kino", 265000),
	}
	dst.addPlaylist("Yandex favorites",
		track(domain.Spotify, "s1", "Pesnya bez slov", "Kino", 265000),
	)
	engine, _ := newTestEngine(t, src, dst, Options{Categories: Categories{Likes: true}})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Likes.Existing != 1 || report.Likes.Added != 0 {
		t.Errorf("likes stats = %+v, want existing 1, added 0", report.Likes)
	}
	if report.LikesPlaylist == nil || report.LikesPlaylist.TargetName != "Yandex favorites" {
		t.Errorf("likes playlist = %+v", report.LikesPlaylist)
	}
	if dst.searchCalls != 0 {
		t.Errorf("search called %d times despite a matching liked track", dst.searchCalls)
	}
	if len(dst.likedAdd) != 0 {
		t.Errorf("native likes were written: %+v", dst.likedAdd)
	}
}

func TestLikesCreateFavoritesPlaylist(t *testing.T) {
	src := newFake(domain.Yandex)
	src.liked = []domain.Track{
		track(domain.Yandex, "y1", "Song One", "Artist", 180000),
		track(domain.Yandex, "y2", "Song Two", "Artist", 180000),
	}
	dst := newFake(domain.Spotify)
	// A native Spotify like must not prevent the track from being added to
	// the imported favorites playlist.
	dst.liked = []domain.Track{track(domain.Spotify, "s1", "Song One", "Artist", 180000)}
	dst.catalog = []domain.Track{
		track(domain.Spotify, "s1", "Song One", "Artist", 180000),
		track(domain.Spotify, "s2", "Song Two", "Artist", 180000),
	}

	engine, _ := newTestEngine(t, src, dst, Options{Categories: Categories{Likes: true}})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Likes.Added != 2 || report.Likes.Existing != 0 {
		t.Errorf("stats = %+v, want added 2, existing 0", report.Likes)
	}
	if report.LikesPlaylist == nil || !report.LikesPlaylist.Created ||
		report.LikesPlaylist.TargetName != "Yandex favorites" {
		t.Fatalf("likes playlist = %+v", report.LikesPlaylist)
	}
	if len(dst.created) != 1 || dst.created[0] != "Yandex favorites" {
		t.Errorf("created = %v", dst.created)
	}
	if len(dst.likedAdd) != 0 {
		t.Errorf("native likes were written: %+v", dst.likedAdd)
	}
	if got := len(dst.playlistTracks[report.LikesPlaylist.TargetID]); got != 2 {
		t.Errorf("favorites playlist has %d tracks, want 2", got)
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
	favorites := dst.addPlaylist("Yandex favorites",
		track(domain.Spotify, "s1", "Song One", "Artist", 180000),
	)

	engine, _ := newTestEngine(t, src, dst, Options{Categories: Categories{Likes: true}})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Likes.Added != 1 || report.Likes.Existing != 1 {
		t.Errorf("stats = %+v, want added 1, existing 1", report.Likes)
	}
	if report.LikesPlaylist == nil || report.LikesPlaylist.Created ||
		report.LikesPlaylist.TargetID != favorites.ID {
		t.Errorf("likes playlist = %+v, want the existing favorites playlist", report.LikesPlaylist)
	}
	if len(dst.likedAdd) != 0 {
		t.Errorf("native likes were written: %+v", dst.likedAdd)
	}
	if got := len(dst.playlistTracks[favorites.ID]); got != 2 {
		t.Errorf("favorites playlist has %d tracks, want 2", got)
	}

	engine2, _ := newTestEngine(t, src, dst, Options{Categories: Categories{Likes: true}})
	report2, err := engine2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report2.Likes.Added != 0 || report2.Likes.Existing != 2 {
		t.Errorf("second run stats = %+v, want added 0, existing 2", report2.Likes)
	}
	if len(dst.created) != 0 {
		t.Errorf("second run created %d playlists", len(dst.created))
	}
	if got := len(dst.playlistTracks[favorites.ID]); got != 2 {
		t.Errorf("second run left %d tracks in favorites playlist, want 2", got)
	}
}

func TestLikesPlaylistUsesSourceServiceName(t *testing.T) {
	src := newFake(domain.Spotify)
	src.liked = []domain.Track{track(domain.Spotify, "s1", "Song", "Artist", 180000)}
	dst := newFake(domain.Yandex)
	dst.catalog = []domain.Track{track(domain.Yandex, "y1", "Song", "Artist", 180000)}

	engine, _ := newTestEngine(t, src, dst, Options{Categories: Categories{Likes: true}})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.LikesPlaylist == nil || report.LikesPlaylist.TargetName != "Spotify favorites" {
		t.Errorf("likes playlist = %+v, want %q", report.LikesPlaylist, "Spotify favorites")
	}
	if len(dst.created) != 1 || dst.created[0] != "Spotify favorites" {
		t.Errorf("created = %v", dst.created)
	}
}

func TestMigrateLikesMovesNativeLikesToFavorites(t *testing.T) {
	src := newFake(domain.Yandex)
	src.liked = []domain.Track{
		track(domain.Yandex, "y1", "Song One", "Artist", 180000),
		track(domain.Yandex, "y2", "Song Two", "Artist", 180000),
		track(domain.Yandex, "y3", "Song Three", "Artist", 180000),
	}
	dst := newFake(domain.Spotify)
	dst.liked = []domain.Track{
		track(domain.Spotify, "s1", "Song One", "Artist", 180000),
		track(domain.Spotify, "s2", "Song Two", "Artist", 180000),
		track(domain.Spotify, "sx", "Unrelated Native Like", "Other", 120000),
	}
	dst.catalog = []domain.Track{
		track(domain.Spotify, "s1", "Song One", "Artist", 180000),
		track(domain.Spotify, "s2", "Song Two", "Artist", 180000),
		track(domain.Spotify, "s3", "Song Three", "Artist", 180000),
	}

	engine, st := newTestEngine(t, src, dst, Options{
		Categories:           Categories{Likes: true},
		MigrateLikes:         true,
		RemoveAllNativeLikes: true,
	})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Likes.Source != 3 || report.Likes.Added != 3 || report.Likes.Existing != 0 {
		t.Errorf("likes stats = %+v, want source 3, added 3", report.Likes)
	}
	if report.NativeLikesRemoved != 2 {
		t.Errorf("native removes = %d, want 2", report.NativeLikesRemoved)
	}
	if len(dst.liked) != 1 || dst.liked[0].ID != "sx" {
		t.Errorf("native liked tracks = %+v, want only the unrelated like", dst.liked)
	}
	if len(dst.likedRemove) != 2 {
		t.Errorf("unliked = %+v, want 2 tracks", dst.likedRemove)
	}
	favorites := report.LikesPlaylist.TargetID
	if got := len(dst.playlistTracks[favorites]); got != 3 {
		t.Errorf("favorites playlist has %d tracks, want 3", got)
	}

	// A second run must not re-add favorites or remove anything else.
	next := &Engine{
		Source:  src,
		Target:  dst,
		Matcher: engine.Matcher,
		Store:   st,
		Log:     engine.Log,
		Options: Options{Categories: Categories{Likes: true}, MigrateLikes: true, RemoveAllNativeLikes: true},
	}
	second, err := next.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Likes.Existing != 3 || second.Likes.Added != 0 || second.NativeLikesRemoved != 0 {
		t.Errorf("second run stats = likes %+v, removed %d", second.Likes, second.NativeLikesRemoved)
	}
	if got := len(dst.playlistTracks[favorites]); got != 3 {
		t.Errorf("second run left %d favorites tracks, want 3", got)
	}
}

func TestMigrateLikesKeepsNativeLikesOutsideWindow(t *testing.T) {
	src := newFake(domain.Yandex)
	src.liked = []domain.Track{
		track(domain.Yandex, "y1", "Old Like", "Artist", 180000),
		track(domain.Yandex, "y2", "Imported Like", "Artist", 180000),
	}
	dst := newFake(domain.Spotify)
	old := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	imported := time.Date(2026, 11, 25, 19, 5, 0, 0, time.UTC)
	s1 := track(domain.Spotify, "s1", "Old Like", "Artist", 180000)
	s1.AddedAt = &old
	s2 := track(domain.Spotify, "s2", "Imported Like", "Artist", 180000)
	s2.AddedAt = &imported
	dst.liked = []domain.Track{s1, s2}
	dst.catalog = []domain.Track{s1, s2}

	engine, _ := newTestEngine(t, src, dst, Options{
		Categories:       Categories{Likes: true},
		MigrateLikes:     true,
		NativeLikesSince: &imported,
		NativeLikesUntil: &imported,
	})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Likes.Added != 2 {
		t.Errorf("favorites added = %d, want 2", report.Likes.Added)
	}
	if report.NativeLikesRemoved != 1 {
		t.Errorf("native removed = %d, want 1", report.NativeLikesRemoved)
	}
	if len(dst.likedRemove) != 1 || dst.likedRemove[0].ID != "s2" {
		t.Errorf("unliked = %+v, want only the imported like", dst.likedRemove)
	}
	if len(dst.liked) != 1 || dst.liked[0].ID != "s1" {
		t.Errorf("native liked tracks = %+v, want the old like kept", dst.liked)
	}
}

func TestMigrateLikesRequiresWindow(t *testing.T) {
	src := newFake(domain.Yandex)
	src.liked = []domain.Track{track(domain.Yandex, "y1", "Song", "Artist", 180000)}
	dst := newFake(domain.Spotify)
	dst.liked = []domain.Track{track(domain.Spotify, "s1", "Song", "Artist", 180000)}
	dst.catalog = []domain.Track{track(domain.Spotify, "s1", "Song", "Artist", 180000)}
	engine, _ := newTestEngine(t, src, dst, Options{
		Categories:   Categories{Likes: true},
		MigrateLikes: true,
	})
	if _, err := engine.Run(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "time window") {
		t.Fatalf("err = %v, want a time-window validation error", err)
	}
}

func TestMigrateLikesDryRunDoesNotTouchNativeLikes(t *testing.T) {
	src := newFake(domain.Yandex)
	src.liked = []domain.Track{
		track(domain.Yandex, "y1", "Song One", "Artist", 180000),
		track(domain.Yandex, "y2", "Song Two", "Artist", 180000),
	}
	dst := newFake(domain.Spotify)
	dst.liked = []domain.Track{track(domain.Spotify, "s1", "Song One", "Artist", 180000)}
	dst.catalog = []domain.Track{
		track(domain.Spotify, "s1", "Song One", "Artist", 180000),
		track(domain.Spotify, "s2", "Song Two", "Artist", 180000),
	}

	engine, _ := newTestEngine(t, src, dst, Options{
		Categories:           Categories{Likes: true},
		MigrateLikes:         true,
		RemoveAllNativeLikes: true,
		DryRun:               true,
	})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Likes.Added != 2 || report.NativeLikesWouldRemove != 1 {
		t.Errorf("stats = %+v, would remove %d", report.Likes, report.NativeLikesWouldRemove)
	}
	if len(dst.likedRemove) != 0 || len(dst.liked) != 1 {
		t.Errorf("dry run changed native likes: liked=%+v removed=%+v", dst.liked, dst.likedRemove)
	}
	if len(dst.playlistTracks) != 0 {
		t.Errorf("dry run created playlist tracks: %+v", dst.playlistTracks)
	}
}

func TestMigrateLikesKeepsNativeLikesWhenFavoritesFail(t *testing.T) {
	src := newFake(domain.Yandex)
	src.liked = []domain.Track{track(domain.Yandex, "y1", "Song One", "Artist", 180000)}
	dst := newFake(domain.Spotify)
	dst.liked = []domain.Track{track(domain.Spotify, "s1", "Song One", "Artist", 180000)}
	dst.catalog = []domain.Track{track(domain.Spotify, "s1", "Song One", "Artist", 180000)}
	dst.addErr = errors.New("cannot add")

	engine, _ := newTestEngine(t, src, dst, Options{
		Categories:           Categories{Likes: true},
		MigrateLikes:         true,
		RemoveAllNativeLikes: true,
	})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.NativeLikesRemoved != 0 || len(dst.likedRemove) != 0 {
		t.Errorf("native likes were removed after a failed add: %+v", dst.likedRemove)
	}
	if len(dst.liked) != 1 {
		t.Errorf("native likes changed after a failed add: %+v", dst.liked)
	}
	if len(report.Errors) == 0 {
		t.Error("expected an add error in the report")
	}
}

func TestLikesPlaylistDryRun(t *testing.T) {
	src := newFake(domain.Yandex)
	src.liked = []domain.Track{track(domain.Yandex, "y1", "Song", "Artist", 180000)}
	dst := newFake(domain.Spotify)
	dst.catalog = []domain.Track{track(domain.Spotify, "s1", "Song", "Artist", 180000)}

	engine, st := newTestEngine(t, src, dst, Options{
		Categories: Categories{Likes: true},
		DryRun:     true,
	})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Likes.Added != 1 {
		t.Errorf("stats = %+v, want one planned add", report.Likes)
	}
	if report.LikesPlaylist == nil || !report.LikesPlaylist.WouldCreate {
		t.Errorf("likes playlist = %+v, want would-create", report.LikesPlaylist)
	}
	if len(dst.playlists) != 0 || len(dst.added) != 0 || len(dst.likedAdd) != 0 {
		t.Errorf("dry run wrote to target: playlists=%d added=%d liked=%d",
			len(dst.playlists), len(dst.added), len(dst.likedAdd))
	}
	if _, ok := st.LookupPlaylist(src.Name(), dst.Name(), likesPlaylistSourceID); ok {
		t.Error("dry run stored a favorites playlist link")
	}
}

func TestSearchBatchStopsAndResumes(t *testing.T) {
	src := newFake(domain.Yandex)
	src.liked = []domain.Track{
		track(domain.Yandex, "y1", "Song One", "Artist", 180000),
		track(domain.Yandex, "y2", "Song Two", "Artist", 180000),
		track(domain.Yandex, "y3", "Song Three", "Artist", 180000),
	}
	dst := newFake(domain.Spotify)
	dst.catalog = []domain.Track{
		track(domain.Spotify, "s1", "Song One", "Artist", 180000),
		track(domain.Spotify, "s2", "Song Two", "Artist", 180000),
		track(domain.Spotify, "s3", "Song Three", "Artist", 180000),
	}

	engine, st := newTestEngine(t, src, dst, Options{
		Categories:  Categories{Likes: true},
		SearchBatch: 1,
	})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.BatchLimitReached {
		t.Error("expected the batch limit to be reported")
	}
	if report.Likes.Added != 1 || report.Likes.Deferred != 2 {
		t.Errorf("first run stats = %+v, want added 1, deferred 2", report.Likes)
	}
	if dst.searchCalls != 1 {
		t.Errorf("search calls = %d, want 1", dst.searchCalls)
	}

	run := func() *Report {
		next := &Engine{
			Source:  src,
			Target:  dst,
			Matcher: engine.Matcher,
			Store:   st,
			Log:     engine.Log,
			Options: Options{Categories: Categories{Likes: true}, SearchBatch: 1},
		}
		r, err := next.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	second := run()
	if second.Likes.Added != 1 || second.Likes.Existing != 1 || second.Likes.Deferred != 1 {
		t.Errorf("second run stats = %+v, want added 1, existing 1, deferred 1", second.Likes)
	}
	if !second.BatchLimitReached {
		t.Error("second run should also stop at the batch limit")
	}

	third := run()
	if third.Likes.Added != 1 || third.Likes.Existing != 2 || third.Likes.Deferred != 0 {
		t.Errorf("third run stats = %+v, want added 1, existing 2, deferred 0", third.Likes)
	}
	if third.BatchLimitReached {
		t.Error("third run should have finished the collection")
	}
	if got := len(dst.playlistTracks[third.LikesPlaylist.TargetID]); got != 3 {
		t.Errorf("favorites playlist has %d tracks, want 3", got)
	}
}

func TestSearchBatchMakesProgressPastUnmatchedTracks(t *testing.T) {
	src := newFake(domain.Yandex)
	src.liked = []domain.Track{
		track(domain.Yandex, "y1", "Unknown One", "Nobody", 1000),
		track(domain.Yandex, "y2", "Unknown Two", "Nobody", 1000),
	}
	dst := newFake(domain.Spotify)

	engine, st := newTestEngine(t, src, dst, Options{
		Categories:  Categories{Likes: true},
		SearchBatch: 1,
	})
	first, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Likes.Unmatched != 1 || first.Likes.Deferred != 1 {
		t.Errorf("first run stats = %+v, want unmatched 1, deferred 1", first.Likes)
	}
	if dst.searchCalls != 1 {
		t.Errorf("search calls after first run = %d, want 1", dst.searchCalls)
	}

	next := &Engine{
		Source:  src,
		Target:  dst,
		Matcher: engine.Matcher,
		Store:   st,
		Log:     engine.Log,
		Options: Options{Categories: Categories{Likes: true}, SearchBatch: 1},
	}
	second, err := next.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Likes.Unmatched != 2 || second.Likes.Deferred != 0 {
		t.Errorf("second run stats = %+v, want unmatched 2, deferred 0", second.Likes)
	}
	if second.BatchLimitReached {
		t.Error("second run should have finished the collection")
	}
	if dst.searchCalls != 2 {
		t.Errorf("search calls after second run = %d, want 2 (first miss must be cached)", dst.searchCalls)
	}
}

func TestSearchBatchMakesProgressPastUnmatchedArtists(t *testing.T) {
	src := newFake(domain.Yandex)
	src.artists = []domain.Artist{
		{Provider: domain.Yandex, ID: "ya1", Name: "Unknown One"},
		{Provider: domain.Yandex, ID: "ya2", Name: "Unknown Two"},
	}
	dst := newFake(domain.Spotify)

	engine, st := newTestEngine(t, src, dst, Options{
		Categories:  Categories{Artists: true},
		SearchBatch: 1,
	})
	first, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Artists.Unmatched != 1 || first.Artists.Deferred != 1 {
		t.Errorf("first run stats = %+v, want unmatched 1, deferred 1", first.Artists)
	}
	if dst.artistSearchCalls != 1 {
		t.Errorf("artist search calls after first run = %d, want 1", dst.artistSearchCalls)
	}

	next := &Engine{
		Source:  src,
		Target:  dst,
		Matcher: engine.Matcher,
		Store:   st,
		Log:     engine.Log,
		Options: Options{Categories: Categories{Artists: true}, SearchBatch: 1},
	}
	second, err := next.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Artists.Unmatched != 2 || second.Artists.Deferred != 0 {
		t.Errorf("second run stats = %+v, want unmatched 2, deferred 0", second.Artists)
	}
	if second.BatchLimitReached {
		t.Error("second run should have finished the collection")
	}
	if dst.artistSearchCalls != 2 {
		t.Errorf("artist search calls after second run = %d, want 2 (first miss must be cached)", dst.artistSearchCalls)
	}
}

func TestEmptyLikesDoNotCreatePlaylist(t *testing.T) {
	src := newFake(domain.Yandex)
	dst := newFake(domain.Spotify)

	engine, _ := newTestEngine(t, src, dst, Options{Categories: Categories{Likes: true}})
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Likes == nil || report.Likes.Source != 0 {
		t.Errorf("likes stats = %+v", report.Likes)
	}
	if len(dst.created) != 0 {
		t.Errorf("created %v for an empty likes library", dst.created)
	}
}

func TestUnmatchedTrackSearchedOncePerRun(t *testing.T) {
	src := newFake(domain.Yandex)
	dst := newFake(domain.Spotify)
	engine, _ := newTestEngine(t, src, dst, Options{})
	tr := track(domain.Yandex, "y1", "Unknown Song", "Nobody", 1000)

	for i := 0; i < 3; i++ {
		res, err := engine.resolve(context.Background(), tr, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Unmatched == nil {
			t.Fatal("expected an unmatched result")
		}
	}
	if dst.searchCalls != 1 {
		t.Errorf("search calls = %d, want 1 for repeated unmatched tracks", dst.searchCalls)
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

func TestMirrorSkippedWhenSourceIsIncomplete(t *testing.T) {
	src := newFake(domain.Yandex)
	src.addPlaylist("Road Trip", track(domain.Yandex, "y1", "Song One", "Artist", 180000))
	// Simulate an API that reports more tracks than it returned.
	src.playlists[0].TrackCount = 2
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
		t.Errorf("removed = %d, want 0 for an incomplete source", res.Removed)
	}
	if len(dst.playlistTracks[existing.ID]) != 2 {
		t.Error("mirror removed tracks from an incomplete source")
	}
	found := false
	for _, note := range res.Notes {
		if strings.Contains(note, "source returned") {
			found = true
		}
	}
	if !found {
		t.Errorf("missing incomplete-source note: %v", res.Notes)
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

func TestQuotaErrorStopsRun(t *testing.T) {
	src := newFake(domain.Yandex)
	src.liked = []domain.Track{track(domain.Yandex, "y1", "Song", "Artist", 180000)}
	dst := newFake(domain.Spotify)
	dst.searchErr = &httpx.RateLimitError{
		Host:          "api.spotify.com",
		RetryAfter:    time.Hour,
		QuotaExceeded: true,
	}
	engine, _ := newTestEngine(t, src, dst, Options{Categories: Categories{Likes: true}})
	_, err := engine.Run(context.Background())
	if !errors.Is(err, httpx.ErrQuotaExceeded) {
		t.Fatalf("err = %v, want quota exceeded", err)
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
	c, err := ParseCategories("playlists,likes,artists")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Playlists || !c.Likes || !c.Artists {
		t.Errorf("parsed categories = %+v", c)
	}
	if _, err := ParseCategories("history"); err == nil {
		t.Error("expected history to be rejected")
	}
	if _, err := ParseCategories("nope"); err == nil {
		t.Error("expected an error for an unknown category")
	}
}
