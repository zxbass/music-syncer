package yandex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

func (c *Client) accountStatus(ctx context.Context) (accountStatus, error) {
	var out accountStatus
	err := c.request(ctx, "GET", "/account/status", nil, nil, &out)
	return out, err
}

func (c *Client) playlists(ctx context.Context, uid string) ([]playlist, error) {
	var out []playlist
	err := c.request(ctx, "GET", "/users/"+url.PathEscape(uid)+"/playlists/list", nil, nil, &out)
	return out, err
}

func (c *Client) playlist(ctx context.Context, uid, kind string) (playlist, error) {
	var out playlist
	err := c.request(ctx, "GET", "/users/"+url.PathEscape(uid)+"/playlists/"+url.PathEscape(kind), nil, nil, &out)
	return out, err
}

// tracks hydrates "track:album" or bare track IDs. Yandex returns full track
// objects. The response order is not guaranteed to match the request order.
func (c *Client) tracks(ctx context.Context, ids []string) ([]track, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	form := url.Values{}
	for _, id := range ids {
		form.Add("track-ids", id)
	}
	form.Set("with-positions", "false")
	var out []track
	if err := c.request(ctx, "POST", "/tracks", nil, form, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) likedTrackRefs(ctx context.Context, uid string) ([]trackRef, error) {
	var out tracksList
	if err := c.request(ctx, "GET", "/users/"+url.PathEscape(uid)+"/likes/tracks", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Library.Tracks, nil
}

func (c *Client) likedArtists(ctx context.Context, uid string) ([]likedArtist, error) {
	var out []likedArtist
	if err := c.request(ctx, "GET", "/users/"+url.PathEscape(uid)+"/likes/artists", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) history(ctx context.Context, fullModels int) (historyResponse, error) {
	if fullModels <= 0 {
		fullModels = 1000
	}
	q := url.Values{"fullModelsCount": {strconv.Itoa(fullModels)}}
	var out historyResponse
	err := c.request(ctx, "GET", "/music-history", q, nil, &out)
	return out, err
}

func (c *Client) searchTracks(ctx context.Context, text string, limit int) ([]track, error) {
	if limit <= 0 {
		limit = 10
	}
	q := url.Values{
		"text":             {text},
		"type":             {"track"},
		"page":             {"0"},
		"nocorrect":        {"false"},
		"playlist-in-best": {"false"},
	}
	var out searchResponse
	if err := c.request(ctx, "GET", "/search", q, nil, &out); err != nil {
		return nil, err
	}
	results := out.Tracks.Results
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func (c *Client) searchArtists(ctx context.Context, text string, limit int) ([]artist, error) {
	if limit <= 0 {
		limit = 10
	}
	q := url.Values{
		"text":      {text},
		"type":      {"artist"},
		"page":      {"0"},
		"nocorrect": {"false"},
	}
	var out searchResponse
	if err := c.request(ctx, "GET", "/search", q, nil, &out); err != nil {
		return nil, err
	}
	results := out.Artists.Results
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func (c *Client) createPlaylist(ctx context.Context, uid, title string) (playlist, error) {
	form := url.Values{
		"title":      {title},
		"visibility": {"private"},
	}
	var out playlist
	err := c.request(ctx, "POST", "/users/"+url.PathEscape(uid)+"/playlists/create", nil, form, &out)
	return out, err
}

func (c *Client) changePlaylist(ctx context.Context, uid, kind string, revision int, ops []changeOp) (playlist, error) {
	diff, err := json.Marshal(ops)
	if err != nil {
		return playlist{}, fmt.Errorf("yandex: encode playlist diff: %w", err)
	}
	form := url.Values{
		"kind":     {kind},
		"revision": {strconv.Itoa(revision)},
		"diff":     {string(diff)},
	}
	var out playlist
	err = c.request(ctx, "POST", "/users/"+url.PathEscape(uid)+"/playlists/"+url.PathEscape(kind)+"/change", nil, form, &out)
	return out, err
}

func (c *Client) addLikedTracks(ctx context.Context, uid string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	form := url.Values{}
	for _, id := range ids {
		form.Add("track-ids", id)
	}
	return c.request(ctx, "POST", "/users/"+url.PathEscape(uid)+"/likes/tracks/add-multiple", nil, form, nil)
}

func (c *Client) addLikedArtists(ctx context.Context, uid string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	form := url.Values{}
	for _, id := range ids {
		form.Add("artist-ids", id)
	}
	return c.request(ctx, "POST", "/users/"+url.PathEscape(uid)+"/likes/artists/add-multiple", nil, form, nil)
}

// trackID builds the "id:album" identifier used by the batch tracks endpoint.
func trackID(id, albumID string) string {
	id = strings.TrimSpace(id)
	albumID = strings.TrimSpace(albumID)
	if albumID == "" {
		return id
	}
	return id + ":" + albumID
}
