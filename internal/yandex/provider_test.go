package yandex

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zxbass/music-syncer/internal/domain"
	"github.com/zxbass/music-syncer/internal/httpx"
)

func newTestProvider(t *testing.T, uid string, handler http.HandlerFunc) *Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	hc := httpx.New(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client := New("token", WithBaseURL(srv.URL), WithHTTPClient(hc))
	return NewProvider(client, uid, nil)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"result": v})
}

func TestAccountAndPlaylists(t *testing.T) {
	provider := newTestProvider(t, "", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "OAuth token" {
			t.Errorf("Authorization = %q", got)
		}
		switch r.URL.Path {
		case "/account/status":
			writeJSON(w, map[string]any{"account": map[string]any{
				"uid": 123, "login": "user@example.com", "display_name": "User",
			}})
		case "/users/123/playlists/list":
			writeJSON(w, []any{
				map[string]any{"uid": 123, "kind": 10, "title": "Mine", "trackCount": 2, "revision": 1,
					"owner": map[string]any{"uid": 123}},
				map[string]any{"uid": 123, "kind": 3, "title": "Мне нравится", "trackCount": 5,
					"owner": map[string]any{"uid": 123}},
				map[string]any{"uid": 123, "kind": 99, "title": "Плейлист дня", "generatedPlaylistType": "playlistOfTheDay",
					"owner": map[string]any{"uid": 123}},
				map[string]any{"uid": 999, "kind": 1, "title": "Other", "owner": map[string]any{"uid": 999}},
			})
		default:
			http.NotFound(w, r)
		}
	})
	account, err := provider.Account(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "123" || account.DisplayName != "User" {
		t.Errorf("account = %+v", account)
	}
	playlists, err := provider.Playlists(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(playlists) != 4 {
		t.Fatalf("got %d playlists", len(playlists))
	}
	if playlists[0].ID != "123:10" || playlists[0].Kind != "10" || playlists[0].ReadOnly {
		t.Errorf("mine = %+v", playlists[0])
	}
	if playlists[1].Special != "liked" {
		t.Errorf("liked playlist = %+v", playlists[1])
	}
	if !playlists[2].Generated {
		t.Errorf("generated playlist = %+v", playlists[2])
	}
	if !playlists[3].ReadOnly {
		t.Errorf("foreign playlist = %+v", playlists[3])
	}
}

func TestPlaylistTracksHydration(t *testing.T) {
	var hydratedForm url.Values
	provider := newTestProvider(t, "123", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/123/playlists/10":
			writeJSON(w, map[string]any{
				"uid": 123, "kind": 10, "revision": 3, "trackCount": 2,
				"tracks": []any{
					map[string]any{"id": 111, "albumId": 222, "track": map[string]any{
						"id": 111, "title": "Full", "durationMs": 180000, "available": true,
						"artists": []any{map[string]any{"id": 1, "name": "Artist"}},
						"albums":  []any{map[string]any{"id": 222, "title": "Album"}},
					}},
					map[string]any{"id": 333, "albumId": 444},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/tracks":
			_ = r.ParseForm()
			hydratedForm = r.PostForm
			writeJSON(w, []any{map[string]any{
				"id": 333, "title": "Hydrated", "durationMs": 200000, "available": true,
				"artists": []any{map[string]any{"id": 2, "name": "Other"}},
				"albums":  []any{map[string]any{"id": 444, "title": "Album Two"}},
			}})
		default:
			http.NotFound(w, r)
		}
	})
	tracks, err := provider.PlaylistTracks(context.Background(), domain.Playlist{
		Provider: domain.Yandex, ID: "123:10", Kind: "10", OwnerID: "123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 {
		t.Fatalf("got %d tracks", len(tracks))
	}
	if tracks[0].ID != "111" || tracks[0].AlbumID != "222" || tracks[0].Title != "Full" {
		t.Errorf("track[0] = %+v", tracks[0])
	}
	if tracks[1].ID != "333" || tracks[1].Title != "Hydrated" || tracks[1].AlbumID != "444" {
		t.Errorf("track[1] = %+v", tracks[1])
	}
	if got := hydratedForm.Get("track-ids"); got != "333:444" {
		t.Errorf("track-ids = %q", got)
	}
}

func TestUnresolvedPlaylistTrackIsKeptForReporting(t *testing.T) {
	provider := newTestProvider(t, "123", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/123/playlists/10":
			writeJSON(w, map[string]any{
				"uid": 123, "kind": 10, "revision": 1, "trackCount": 1,
				"tracks": []any{map[string]any{"id": 111, "albumId": 222}},
			})
		case "/tracks":
			writeJSON(w, []any{})
		default:
			http.NotFound(w, r)
		}
	})
	tracks, err := provider.PlaylistTracks(context.Background(), domain.Playlist{
		Provider: domain.Yandex, ID: "123:10", Kind: "10", OwnerID: "123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 1 || tracks[0].ID != "111" || tracks[0].Title != "" {
		t.Fatalf("tracks = %+v, want an unresolved placeholder", tracks)
	}
}

func TestLikedTracksLimitBeforeHydration(t *testing.T) {
	var requested []string
	provider := newTestProvider(t, "123", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/123/likes/tracks":
			writeJSON(w, map[string]any{"library": map[string]any{
				"uid": 123, "revision": 1,
				"tracks": []any{
					map[string]any{"id": 1, "albumId": 2},
					map[string]any{"id": 3, "albumId": 4},
				},
			}})
		case "/tracks":
			_ = r.ParseForm()
			requested = r.PostForm["track-ids"]
			writeJSON(w, []any{map[string]any{
				"id": 1, "title": "One", "durationMs": 1000, "available": true,
				"artists": []any{map[string]any{"name": "Artist"}},
				"albums":  []any{map[string]any{"id": 2, "title": "Album"}},
			}})
		default:
			http.NotFound(w, r)
		}
	})
	tracks, err := provider.LikedTracks(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 1 || tracks[0].ID != "1" {
		t.Fatalf("tracks = %+v", tracks)
	}
	if len(requested) != 1 || requested[0] != "1:2" {
		t.Errorf("requested = %v", requested)
	}
}

func TestLikedArtists(t *testing.T) {
	provider := newTestProvider(t, "123", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/123/likes/artists" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, []any{
			map[string]any{"type": "artist", "id": 5, "artist": map[string]any{"id": 5, "name": "Nested"}},
			map[string]any{"id": 6, "name": "Direct"},
		})
	})
	artists, err := provider.LikedArtists(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(artists) != 2 || artists[0].Name != "Nested" || artists[1].Name != "Direct" {
		t.Fatalf("artists = %+v", artists)
	}
}

func TestSearchTracksAndArtists(t *testing.T) {
	var textCalls int32
	provider := newTestProvider(t, "123", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&textCalls, 1)
		switch r.URL.Query().Get("type") {
		case "track":
			writeJSON(w, map[string]any{"tracks": map[string]any{"results": []any{map[string]any{
				"id": 1, "title": "Found", "durationMs": 1000, "available": true,
				"artists": []any{map[string]any{"name": "Artist"}},
				"albums":  []any{map[string]any{"id": 2, "title": "Album"}},
			}}}})
		case "artist":
			writeJSON(w, map[string]any{"artists": map[string]any{"results": []any{map[string]any{
				"id": 9, "name": "Artist",
			}}}})
		default:
			http.NotFound(w, r)
		}
	})
	tracks, err := provider.SearchTracks(context.Background(), domain.Track{Title: "Found", Artists: []string{"Artist"}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 1 || tracks[0].ID != "1" {
		t.Fatalf("tracks = %+v", tracks)
	}
	artists, err := provider.SearchArtists(context.Background(), domain.Artist{Name: "Artist"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(artists) != 1 || artists[0].ID != "9" {
		t.Fatalf("artists = %+v", artists)
	}
	if textCalls != 2 {
		t.Errorf("search calls = %d", textCalls)
	}
}

func TestAddTracksDiff(t *testing.T) {
	var diff string
	var revision string
	provider := newTestProvider(t, "123", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/123/playlists/42":
			writeJSON(w, map[string]any{
				"uid": 123, "kind": 42, "revision": 7, "trackCount": 1,
				"tracks": []any{map[string]any{"id": 1, "albumId": 2}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/users/123/playlists/42/change":
			_ = r.ParseForm()
			diff = r.PostForm.Get("diff")
			revision = r.PostForm.Get("revision")
			writeJSON(w, map[string]any{"uid": 123, "kind": 42, "revision": 8, "trackCount": 2})
		default:
			http.NotFound(w, r)
		}
	})
	err := provider.AddTracks(context.Background(), domain.Playlist{
		Provider: domain.Yandex, ID: "123:42", Kind: "42", OwnerID: "123",
	}, []domain.Track{
		{Provider: domain.Yandex, ID: "555", AlbumID: "666", Title: "New"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if revision != "7" {
		t.Errorf("revision = %q, want 7", revision)
	}
	var ops []changeOp
	if err := json.Unmarshal([]byte(diff), &ops); err != nil {
		t.Fatalf("decode diff %q: %v", diff, err)
	}
	if len(ops) != 1 || ops[0].Op != "insert" || ops[0].At == nil || *ops[0].At != 1 ||
		len(ops[0].Tracks) != 1 || ops[0].Tracks[0].ID != "555" || ops[0].Tracks[0].AlbumID != "666" {
		t.Errorf("ops = %+v", ops)
	}
}

func TestAddTracksToEmptyPlaylistKeepsAtZero(t *testing.T) {
	var diff string
	provider := newTestProvider(t, "123", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/123/playlists/42":
			writeJSON(w, map[string]any{"uid": 123, "kind": 42, "revision": 1, "trackCount": 0})
		case r.Method == http.MethodPost && r.URL.Path == "/users/123/playlists/42/change":
			_ = r.ParseForm()
			diff = r.PostForm.Get("diff")
			writeJSON(w, map[string]any{"uid": 123, "kind": 42, "revision": 2})
		default:
			http.NotFound(w, r)
		}
	})
	err := provider.AddTracks(context.Background(), domain.Playlist{
		Provider: domain.Yandex, ID: "123:42", Kind: "42", OwnerID: "123",
	}, []domain.Track{{Provider: domain.Yandex, ID: "555", AlbumID: "666", Title: "New"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, `"at":0`) {
		t.Errorf("diff does not contain at:0: %s", diff)
	}
}

func TestRemoveTracksRanges(t *testing.T) {
	var diffs []string
	revision := 1
	provider := newTestProvider(t, "123", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/123/playlists/42":
			writeJSON(w, map[string]any{
				"uid": 123, "kind": 42, "revision": revision, "trackCount": 5,
				"tracks": []any{
					map[string]any{"id": 1, "albumId": 1},
					map[string]any{"id": 2, "albumId": 2},
					map[string]any{"id": 3, "albumId": 3},
					map[string]any{"id": 4, "albumId": 4},
					map[string]any{"id": 5, "albumId": 5},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/users/123/playlists/42/change":
			_ = r.ParseForm()
			diffs = append(diffs, r.PostForm.Get("diff"))
			revision++
			writeJSON(w, map[string]any{"uid": 123, "kind": 42, "revision": revision})
		default:
			http.NotFound(w, r)
		}
	})
	err := provider.RemoveTracks(context.Background(), domain.Playlist{
		Provider: domain.Yandex, ID: "123:42", Kind: "42", OwnerID: "123",
	}, []domain.Track{{ID: "2"}, {ID: "3"}, {ID: "5"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 2 {
		t.Fatalf("got %d change calls: %v", len(diffs), diffs)
	}
	var first, second []changeOp
	_ = json.Unmarshal([]byte(diffs[0]), &first)
	_ = json.Unmarshal([]byte(diffs[1]), &second)
	if len(first) != 1 || first[0].Op != "delete" || first[0].From == nil || *first[0].From != 4 ||
		first[0].To == nil || *first[0].To != 5 {
		t.Errorf("first diff = %+v", first)
	}
	if len(second) != 1 || second[0].Op != "delete" || second[0].From == nil || *second[0].From != 1 ||
		second[0].To == nil || *second[0].To != 3 {
		t.Errorf("second diff = %+v", second)
	}
}

func TestRemoveTracksRejectsPartialPlaylist(t *testing.T) {
	provider := newTestProvider(t, "123", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/123/playlists/42":
			writeJSON(w, map[string]any{
				"uid": 123, "kind": 42, "revision": 1, "trackCount": 2,
				"tracks": []any{map[string]any{"id": 1, "albumId": 1}},
			})
		default:
			http.NotFound(w, r)
		}
	})
	err := provider.RemoveTracks(context.Background(), domain.Playlist{
		Provider: domain.Yandex, ID: "123:42", Kind: "42", OwnerID: "123",
	}, []domain.Track{{ID: "1"}})
	if err == nil {
		t.Fatal("expected an error for a partial playlist")
	}
	if !strings.Contains(err.Error(), "partial playlist") {
		t.Errorf("error = %v", err)
	}
}

func TestLikesAndFollows(t *testing.T) {
	var likedTracks, likedArtists []string
	provider := newTestProvider(t, "123", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.URL.Path {
		case "/users/123/likes/tracks/add-multiple":
			likedTracks = append(likedTracks, r.PostForm["track-ids"]...)
			writeJSON(w, map[string]any{"revision": 1})
		case "/users/123/likes/artists/add-multiple":
			likedArtists = append(likedArtists, r.PostForm["artist-ids"]...)
			http.Error(w, "not json", 200)
		default:
			http.NotFound(w, r)
		}
	})
	if err := provider.LikeTracks(context.Background(), []domain.Track{{ID: "1"}, {ID: "2"}}); err != nil {
		t.Fatal(err)
	}
	if len(likedTracks) != 2 || likedTracks[0] != "1" {
		t.Errorf("liked tracks = %v", likedTracks)
	}
	// The artist endpoint returns a plain "ok" string in the real API; our
	// client decodes with out=nil, so any 2xx body is fine.
	if err := provider.FollowArtists(context.Background(), []domain.Artist{{ID: "5"}}); err != nil {
		t.Fatal(err)
	}
	if len(likedArtists) != 1 || likedArtists[0] != "5" {
		t.Errorf("liked artists = %v", likedArtists)
	}
}

func TestUnauthorizedMessage(t *testing.T) {
	provider := newTestProvider(t, "123", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"name":"Unauthorized","message":"bad token"}}`)
	})
	_, err := provider.Playlists(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !domain.IsUnauthorized(err) {
		t.Errorf("error is not unauthorized: %v", err)
	}
	if !strings.Contains(err.Error(), "auth yandex") {
		t.Errorf("error = %v", err)
	}
}

func TestDeviceLoginRequiresSecret(t *testing.T) {
	_, err := LoginDevice(context.Background(), DeviceAuthConfig{Out: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "YANDEX_MUSIC_CLIENT_SECRET") {
		t.Fatalf("err = %v, want a client-secret error", err)
	}
}

func TestDeviceLogin(t *testing.T) {
	var tokenCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device/code":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "device", "user_code": "ABCD",
				"verification_url": "https://ya.ru/device", "expires_in": 60, "interval": 1,
			})
		case "/token":
			atomic.AddInt32(&tokenCalls, 1)
			if tokenCalls == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"authorization_pending"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "secret-token"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	var out strings.Builder
	token, err := LoginDevice(context.Background(), DeviceAuthConfig{
		BaseURL: srv.URL, Out: &out, PollInterval: time.Millisecond,
		ClientSecret: "test-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if token != "secret-token" {
		t.Errorf("token = %q", token)
	}
	if !strings.Contains(out.String(), "ABCD") || !strings.Contains(out.String(), "ya.ru/device") {
		t.Errorf("output = %q", out.String())
	}
	if tokenCalls != 2 {
		t.Errorf("token calls = %d", tokenCalls)
	}
}
