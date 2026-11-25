package spotify

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

func newTestProvider(t *testing.T, handler http.HandlerFunc) (*Provider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	hc := httpx.New(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client := New("client-id", Token{
		AccessToken:  "token",
		RefreshToken: "refresh",
		Expiry:       time.Now().Add(time.Hour),
	}, WithBaseURLs(srv.URL+"/v1", srv.URL), WithHTTPClient(hc))
	return NewProvider(client, nil), srv
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestPlaylistsAndReadOnly(t *testing.T) {
	provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Errorf("Authorization = %q", got)
		}
		switch r.URL.Path {
		case "/v1/me":
			writeJSON(w, 200, map[string]any{"id": "me", "display_name": "Me"})
		case "/v1/me/playlists":
			writeJSON(w, 200, map[string]any{
				"total": 3,
				"items": []any{
					map[string]any{"id": "own", "name": "Mine", "owner": map[string]any{"id": "me"}, "items": map[string]any{"total": 2}},
					map[string]any{"id": "other", "name": "Followed", "owner": map[string]any{"id": "someone"}, "tracks": map[string]any{"total": 3}},
					map[string]any{"id": "editorial", "name": "Discover Weekly", "owner": map[string]any{"id": "spotify"}, "items": map[string]any{"total": 30}},
				},
			})
		default:
			http.NotFound(w, r)
		}
	})
	playlists, err := provider.Playlists(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(playlists) != 3 {
		t.Fatalf("got %d playlists", len(playlists))
	}
	owned, followed, editorial := playlists[0], playlists[1], playlists[2]
	if owned.ReadOnly || owned.Generated || owned.TrackCount != 2 {
		t.Errorf("owned = %+v", owned)
	}
	if !followed.ReadOnly || followed.TrackCount != 3 {
		t.Errorf("followed = %+v", followed)
	}
	if !editorial.Generated || !editorial.ReadOnly {
		t.Errorf("editorial = %+v", editorial)
	}
}

func TestPlaylistTracksNewShape(t *testing.T) {
	provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/playlists/own/items" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, 200, map[string]any{
			"total": 2,
			"items": []any{
				map[string]any{"item": map[string]any{
					"id": "t1", "name": "Song One", "duration_ms": 180000, "isrc": "US1",
					"artists": []any{map[string]any{"id": "a1", "name": "Artist"}},
					"album":   map[string]any{"id": "al1", "name": "Album"},
				}},
				map[string]any{"track": map[string]any{
					"id": "t2", "name": "Song Two", "duration_ms": 200000,
					"artists": []any{map[string]any{"id": "a1", "name": "Artist"}},
					"album":   map[string]any{"id": "al1", "name": "Album"},
				}},
			},
		})
	})
	tracks, err := provider.PlaylistTracks(context.Background(), domain.Playlist{ID: "own"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 {
		t.Fatalf("got %d tracks", len(tracks))
	}
	if tracks[0].ID != "t1" || tracks[0].AlbumID != "al1" || tracks[0].ISRC != "US1" {
		t.Errorf("track[0] = %+v", tracks[0])
	}
	if tracks[1].ID != "t2" {
		t.Errorf("track[1] = %+v", tracks[1])
	}
}

func TestPlaylistTracksLegacyFallback(t *testing.T) {
	var itemCalls, trackCalls int32
	provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/playlists/legacy/items":
			atomic.AddInt32(&itemCalls, 1)
			http.NotFound(w, r)
		case "/v1/playlists/legacy/tracks":
			atomic.AddInt32(&trackCalls, 1)
			writeJSON(w, 200, map[string]any{
				"total": 1,
				"items": []any{map[string]any{"track": map[string]any{
					"id": "old", "name": "Old Shape",
					"artists": []any{map[string]any{"name": "Artist"}},
					"album":   map[string]any{"id": "al", "name": "Album"},
				}}},
			})
		default:
			http.NotFound(w, r)
		}
	})
	tracks, err := provider.PlaylistTracks(context.Background(), domain.Playlist{ID: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 1 || tracks[0].ID != "old" {
		t.Fatalf("tracks = %+v", tracks)
	}
	if itemCalls != 1 || trackCalls != 1 {
		t.Errorf("itemCalls=%d trackCalls=%d", itemCalls, trackCalls)
	}
}

func TestLikedTracksArtistsHistory(t *testing.T) {
	provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/me/tracks":
			writeJSON(w, 200, map[string]any{
				"total": 1,
				"items": []any{map[string]any{"track": map[string]any{
					"id": "t1", "name": "Liked", "duration_ms": 1000,
					"artists": []any{map[string]any{"name": "Artist"}},
					"album":   map[string]any{"id": "al", "name": "Album"},
				}}},
			})
		case "/v1/me/following":
			after := r.URL.Query().Get("after")
			if after == "" {
				writeJSON(w, 200, map[string]any{"artists": map[string]any{
					"items":   []any{map[string]any{"id": "a1", "name": "One"}},
					"cursors": map[string]any{"after": "next"},
				}})
			} else {
				writeJSON(w, 200, map[string]any{"artists": map[string]any{
					"items":   []any{map[string]any{"id": "a2", "name": "Two"}},
					"cursors": map[string]any{"after": ""},
				}})
			}
		case "/v1/me/player/recently-played":
			writeJSON(w, 200, map[string]any{"items": []any{map[string]any{
				"played_at": time.Now().Add(-time.Hour).Format(time.RFC3339),
				"track": map[string]any{
					"id": "h1", "name": "Played", "duration_ms": 1000,
					"artists": []any{map[string]any{"name": "Artist"}},
					"album":   map[string]any{"id": "al", "name": "Album"},
				},
			}}})
		default:
			http.NotFound(w, r)
		}
	})

	liked, err := provider.LikedTracks(context.Background(), 0)
	if err != nil || len(liked) != 1 || liked[0].ID != "t1" {
		t.Fatalf("liked = %+v, err=%v", liked, err)
	}
	artists, err := provider.LikedArtists(context.Background(), 0)
	if err != nil || len(artists) != 2 {
		t.Fatalf("artists = %+v, err=%v", artists, err)
	}
	history, err := provider.History(context.Background(), time.Time{}, 0)
	if err != nil || len(history) != 1 || history[0].Track.ID != "h1" {
		t.Fatalf("history = %+v, err=%v", history, err)
	}
}

func TestSearchTracksISRC(t *testing.T) {
	var calls int32
	provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/search" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&calls, 1)
		q := r.URL.Query().Get("q")
		if strings.HasPrefix(q, "isrc:") {
			writeJSON(w, 200, map[string]any{"tracks": map[string]any{"items": []any{map[string]any{
				"id": "found", "name": "Song", "isrc": "USRC123", "duration_ms": 180000,
				"artists": []any{map[string]any{"name": "Artist"}},
				"album":   map[string]any{"id": "al", "name": "Album"},
			}}}})
			return
		}
		t.Errorf("unexpected query %q", q)
		writeJSON(w, 200, map[string]any{"tracks": map[string]any{"items": []any{}}})
	})
	src := domain.Track{Title: "Song", Artists: []string{"Artist"}, DurationMS: 180000, ISRC: "USRC123"}
	candidates, err := provider.SearchTracks(context.Background(), src, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != "found" {
		t.Fatalf("candidates = %+v", candidates)
	}
	if calls != 1 {
		t.Errorf("search calls = %d, want 1", calls)
	}
}

func TestSearchTracksFallsBackToText(t *testing.T) {
	var isrcCalls, textCalls int32
	provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if strings.HasPrefix(q, "isrc:") {
			atomic.AddInt32(&isrcCalls, 1)
			writeJSON(w, 400, map[string]any{"error": map[string]any{"status": 400, "message": "unsupported query"}})
			return
		}
		atomic.AddInt32(&textCalls, 1)
		writeJSON(w, 200, map[string]any{"tracks": map[string]any{"items": []any{map[string]any{
			"id": "found", "name": "Song", "duration_ms": 180000,
			"artists": []any{map[string]any{"name": "Artist"}},
			"album":   map[string]any{"id": "al", "name": "Album"},
		}}}})
	})
	src := domain.Track{Title: "Song", Artists: []string{"Artist"}, DurationMS: 180000, ISRC: "MISSING"}
	candidates, err := provider.SearchTracks(context.Background(), src, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %+v", candidates)
	}
	if isrcCalls != 1 || textCalls != 1 {
		t.Errorf("isrcCalls=%d textCalls=%d", isrcCalls, textCalls)
	}
}

func TestSearchArtists(t *testing.T) {
	provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") != "artist" {
			t.Errorf("type = %q", r.URL.Query().Get("type"))
		}
		writeJSON(w, 200, map[string]any{"artists": map[string]any{"items": []any{
			map[string]any{"id": "a1", "name": "Artist"},
		}}})
	})
	artists, err := provider.SearchArtists(context.Background(), domain.Artist{Name: "Artist"}, 10)
	if err != nil || len(artists) != 1 || artists[0].ID != "a1" {
		t.Fatalf("artists = %+v, err=%v", artists, err)
	}
}

func TestCreatePlaylistAndAddRemove(t *testing.T) {
	var createBody map[string]any
	var addURIs []string
	var removeBody map[string]any
	provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/me/playlists":
			_ = json.NewDecoder(r.Body).Decode(&createBody)
			writeJSON(w, 201, map[string]any{"id": "new", "name": createBody["name"], "owner": map[string]any{"id": "me"}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/playlists/new/items":
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/playlists/new/tracks":
			var body struct {
				URIs []string `json:"uris"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			addURIs = body.URIs
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/playlists/new/items":
			_ = json.NewDecoder(r.Body).Decode(&removeBody)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	})
	pl, err := provider.CreatePlaylist(context.Background(), "Imported", "desc")
	if err != nil {
		t.Fatal(err)
	}
	if pl.ID != "new" || createBody["public"] != false {
		t.Errorf("playlist = %+v, body = %+v", pl, createBody)
	}
	err = provider.AddTracks(context.Background(), pl, []domain.Track{{ID: "t1"}, {ID: "t2"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(addURIs) != 2 || addURIs[0] != "spotify:track:t1" {
		t.Errorf("uris = %v", addURIs)
	}
	err = provider.RemoveTracks(context.Background(), pl, []domain.Track{{ID: "t1"}})
	if err != nil {
		t.Fatal(err)
	}
	items, _ := removeBody["items"].([]any)
	if len(items) != 1 {
		t.Errorf("remove body = %+v", removeBody)
	}
}

func TestLikeAndFollowFallbacks(t *testing.T) {
	var libraryURIs []string
	var trackIDs []string
	var followingIDs []string
	provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/me/library":
			if strings.Contains(r.URL.Query().Get("uris"), "track") {
				libraryURIs = append(libraryURIs, r.URL.Query().Get("uris"))
				w.WriteHeader(http.StatusOK)
				return
			}
			if strings.Contains(r.URL.Query().Get("uris"), "artist") {
				http.NotFound(w, r)
				return
			}
			http.NotFound(w, r)
		case "/v1/me/tracks":
			trackIDs = append(trackIDs, r.URL.Query().Get("ids"))
			w.WriteHeader(http.StatusOK)
		case "/v1/me/following":
			followingIDs = append(followingIDs, r.URL.Query().Get("ids"))
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	if err := provider.LikeTracks(context.Background(), []domain.Track{{ID: "t1"}, {ID: "t2"}}); err != nil {
		t.Fatal(err)
	}
	if len(libraryURIs) != 1 || libraryURIs[0] != "spotify:track:t1,spotify:track:t2" {
		t.Errorf("library uris = %v", libraryURIs)
	}
	if err := provider.FollowArtists(context.Background(), []domain.Artist{{ID: "a1"}}); err != nil {
		t.Fatal(err)
	}
	if len(followingIDs) != 1 || followingIDs[0] != "a1" {
		t.Errorf("following ids = %v", followingIDs)
	}
	if len(trackIDs) != 0 {
		t.Errorf("unexpected /me/tracks call")
	}
}

func TestLikeTracksLegacyFallback(t *testing.T) {
	var ids string
	provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/me/library":
			http.NotFound(w, r)
		case "/v1/me/tracks":
			ids = r.URL.Query().Get("ids")
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	})
	if err := provider.LikeTracks(context.Background(), []domain.Track{{ID: "t1"}}); err != nil {
		t.Fatal(err)
	}
	if ids != "t1" {
		t.Errorf("ids = %q", ids)
	}
}

func TestTokenRefreshOn401(t *testing.T) {
	var tokenCalls int32
	var refreshed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/token":
			atomic.AddInt32(&tokenCalls, 1)
			writeJSON(w, 200, map[string]any{"access_token": "new-token", "expires_in": 3600})
		case "/me":
			if r.Header.Get("Authorization") == "Bearer new-token" {
				writeJSON(w, 200, map[string]any{"id": "me"})
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"status":401,"message":"expired"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	hc := httpx.New(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client := New("client-id", Token{AccessToken: "old-token", RefreshToken: "refresh"},
		WithBaseURLs(srv.URL, srv.URL), WithHTTPClient(hc),
		WithTokenUpdate(func(tok Token) {
			if tok.AccessToken == "new-token" {
				refreshed.Store(true)
			}
		}))
	var u user
	if err := client.Get(context.Background(), "/me", url.Values{}, &u); err != nil {
		t.Fatal(err)
	}
	if u.ID != "me" {
		t.Errorf("user = %+v", u)
	}
	if tokenCalls != 1 || !refreshed.Load() {
		t.Errorf("tokenCalls=%d refreshed=%v", tokenCalls, refreshed.Load())
	}
}

func TestReadManualCode(t *testing.T) {
	in := strings.NewReader("http://127.0.0.1:8085/callback?code=abc&state=xyz\n")
	code, err := readManualCode(in, io.Discard, "/callback", "xyz")
	if err != nil {
		t.Fatal(err)
	}
	if code != "abc" {
		t.Errorf("code = %q", code)
	}
	_, err = readManualCode(strings.NewReader("http://127.0.0.1:8085/callback?code=abc&state=wrong\n"),
		io.Discard, "/callback", "xyz")
	if err == nil {
		t.Error("expected a state mismatch error")
	}
}

func TestAPIError(t *testing.T) {
	provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 400, map[string]any{"error": map[string]any{"status": 400, "message": "bad request detail"}})
	})
	_, err := provider.Playlists(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "bad request detail") {
		t.Errorf("error = %v", err)
	}
}
