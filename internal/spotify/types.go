package spotify

import (
	"time"

	"github.com/zxbass/music-syncer/internal/domain"
)

type user struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
}

type image struct {
	URL string `json:"url"`
}

type artist struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	ExternalURLs map[string]string `json:"external_urls"`
}

type album struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	ReleaseDate string   `json:"release_date"`
	Artists     []artist `json:"artists"`
}

type track struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	URI          string            `json:"uri"`
	DurationMS   int               `json:"duration_ms"`
	Explicit     bool              `json:"explicit"`
	ISRC         string            `json:"isrc"`
	IsLocal      bool              `json:"is_local"`
	Type         string            `json:"type"`
	Artists      []artist          `json:"artists"`
	Album        album             `json:"album"`
	ExternalURLs map[string]string `json:"external_urls"`
}

func (t *track) toDomain() domain.Track {
	if t == nil {
		return domain.Track{}
	}
	artists := make([]string, 0, len(t.Artists))
	for _, a := range t.Artists {
		if a.Name != "" {
			artists = append(artists, a.Name)
		}
	}
	return domain.Track{
		Provider:   domain.Spotify,
		ID:         t.ID,
		AlbumID:    t.Album.ID,
		Title:      t.Name,
		Artists:    artists,
		Album:      t.Album.Name,
		DurationMS: t.DurationMS,
		ISRC:       t.ISRC,
		Explicit:   t.Explicit,
		URL:        t.ExternalURLs["spotify"],
	}
}

type playlist struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	Public        bool              `json:"public"`
	Collaborative bool              `json:"collaborative"`
	SnapshotID    string            `json:"snapshot_id"`
	Owner         *user             `json:"owner"`
	ExternalURLs  map[string]string `json:"external_urls"`
	Items         struct {
		Total int            `json:"total"`
		Items []playlistItem `json:"items"`
	} `json:"items"`
	// Tracks is the pre-February-2026 name of Items. It is still returned by
	// apps in extended quota mode.
	Tracks struct {
		Total int `json:"total"`
	} `json:"tracks"`
}

func (p playlist) trackCount() int {
	if p.Items.Total > 0 {
		return p.Items.Total
	}
	return p.Tracks.Total
}

type playlistItem struct {
	AddedAt string `json:"added_at"`
	Track   *track `json:"track"`
	Item    *track `json:"item"`
	IsLocal bool   `json:"is_local"`
}

func (i playlistItem) value() *track {
	if i.Item != nil {
		return i.Item
	}
	return i.Track
}

type playlistPage struct {
	Items  []playlist `json:"items"`
	Total  int        `json:"total"`
	Next   *string    `json:"next"`
	Offset int        `json:"offset"`
}

type playlistItemsPage struct {
	Items  []playlistItem `json:"items"`
	Total  int            `json:"total"`
	Next   *string        `json:"next"`
	Offset int            `json:"offset"`
}

type savedTrackItem struct {
	AddedAt string `json:"added_at"`
	Track   *track `json:"track"`
	Item    *track `json:"item"`
}

func (i savedTrackItem) value() *track {
	if i.Item != nil {
		return i.Item
	}
	return i.Track
}

type savedTracksPage struct {
	Items  []savedTrackItem `json:"items"`
	Total  int              `json:"total"`
	Next   *string          `json:"next"`
	Offset int              `json:"offset"`
}

type followedArtistsPage struct {
	Artists struct {
		Items   []artist `json:"items"`
		Next    *string  `json:"next"`
		Cursors struct {
			After string `json:"after"`
		} `json:"cursors"`
	} `json:"artists"`
}

type recentlyPlayedPage struct {
	Items []struct {
		Track    *track    `json:"track"`
		PlayedAt time.Time `json:"played_at"`
		Context  *struct {
			Type string `json:"type"`
			URI  string `json:"uri"`
		} `json:"context"`
	} `json:"items"`
}

type searchTracksPage struct {
	Tracks struct {
		Items []track `json:"items"`
		Total int     `json:"total"`
	} `json:"tracks"`
}

type searchArtistsPage struct {
	Artists struct {
		Items []artist `json:"items"`
		Total int      `json:"total"`
	} `json:"artists"`
}
