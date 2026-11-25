package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zxbass/music-syncer/internal/domain"
)

func TestMatchesPlaylistsArtists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	track := domain.Track{
		Provider: domain.Spotify, ID: "s1", AlbumID: "al1", Title: "Song",
		Artists: []string{"Artist"}, DurationMS: 180000,
	}
	st.PutMatch(domain.Yandex, domain.Spotify, "y1", track, 0.93)
	st.PutPlaylist(domain.Yandex, domain.Spotify, "ypl", "spl", "My Playlist")
	st.PutArtist(domain.Yandex, domain.Spotify, "ya", "sa", "Artist")

	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	match, ok := reopened.LookupMatch(domain.Yandex, domain.Spotify, "y1")
	if !ok || match.TargetID != "s1" || match.TargetAlbumID != "al1" || match.Confidence != 0.93 {
		t.Errorf("match = %+v, ok=%v", match, ok)
	}
	link, ok := reopened.LookupPlaylist(domain.Yandex, domain.Spotify, "ypl")
	if !ok || link.TargetID != "spl" || link.TargetName != "My Playlist" {
		t.Errorf("playlist link = %+v, ok=%v", link, ok)
	}
	artist, ok := reopened.LookupArtist(domain.Yandex, domain.Spotify, "ya")
	if !ok || artist.TargetID != "sa" {
		t.Errorf("artist link = %+v, ok=%v", artist, ok)
	}
	if !reopened.TargetPlaylistInUse(domain.Spotify, "spl") {
		t.Error("target playlist should be marked as in use")
	}
	if reopened.TargetPlaylistInUse(domain.Yandex, "spl") {
		t.Error("target playlist should only be in use for the linked target provider")
	}
	stats := reopened.Stats()
	if stats.Matches != 1 || stats.Playlists != 1 || stats.Artists != 1 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestMissingAndEmpty(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Stats() != (Stats{}) {
		t.Errorf("new store stats = %+v", st.Stats())
	}

	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(empty); err != nil {
		t.Errorf("empty state: %v", err)
	}
}

func TestFutureVersionRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Error("expected a version error")
	}
}

func TestClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.PutMatch(domain.Yandex, domain.Spotify, "y1", domain.Track{ID: "s1"}, 1)
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if err := st.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("state file still exists: %v", err)
	}
	if st.Stats() != (Stats{}) {
		t.Errorf("stats after clear = %+v", st.Stats())
	}
}
