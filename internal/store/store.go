// Package store persists match decisions and links between playlists so
// repeated synchronizations are fast and idempotent.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zxbass/music-syncer/internal/domain"
)

// Version is the state file format version.
const Version = 1

// Data is the serialized state.
type Data struct {
	Version   int                     `json:"version"`
	Matches   map[string]Match        `json:"matches,omitempty"`
	Playlists map[string]PlaylistLink `json:"playlists,omitempty"`
	Artists   map[string]ArtistLink   `json:"artists,omitempty"`
}

// Match is a cached source -> target track mapping.
type Match struct {
	TargetID      string    `json:"target_id"`
	TargetAlbumID string    `json:"target_album_id,omitempty"`
	Title         string    `json:"title,omitempty"`
	Artists       []string  `json:"artists,omitempty"`
	DurationMS    int       `json:"duration_ms,omitempty"`
	Confidence    float64   `json:"confidence,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// PlaylistLink maps a source playlist to a target playlist.
type PlaylistLink struct {
	TargetID   string    `json:"target_id"`
	TargetName string    `json:"target_name,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ArtistLink maps a source artist to a target artist.
type ArtistLink struct {
	TargetID  string    `json:"target_id"`
	Name      string    `json:"name,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store is a JSON-backed state store. It is safe for concurrent use.
type Store struct {
	path string

	mu    sync.Mutex
	data  Data
	dirty bool
}

// Open loads a store. A missing file creates an empty store.
func Open(path string) (*Store, error) {
	s := &Store{path: path, data: newData()}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}
	if len(raw) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	if s.data.Version > Version {
		return nil, fmt.Errorf("state %s has version %d, this binary supports %d", path, s.data.Version, Version)
	}
	if s.data.Version == 0 {
		s.data.Version = Version
	}
	normalize(&s.data)
	return s, nil
}

func newData() Data {
	return Data{
		Version:   Version,
		Matches:   map[string]Match{},
		Playlists: map[string]PlaylistLink{},
		Artists:   map[string]ArtistLink{},
	}
}

func normalize(data *Data) {
	if data.Matches == nil {
		data.Matches = map[string]Match{}
	}
	if data.Playlists == nil {
		data.Playlists = map[string]PlaylistLink{}
	}
	if data.Artists == nil {
		data.Artists = map[string]ArtistLink{}
	}
}

// Path returns the state file path.
func (s *Store) Path() string { return s.path }

// Save writes the state atomically. It is a no-op when nothing changed.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*.json")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	s.dirty = false
	return nil
}

// MatchKey builds the cache key for a source track.
func MatchKey(src, dst domain.Name, srcID string) string {
	return string(src) + ":track:" + srcID + "->" + string(dst)
}

// LookupMatch returns a cached match.
func (s *Store) LookupMatch(src, dst domain.Name, srcID string) (Match, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.data.Matches[MatchKey(src, dst, srcID)]
	return m, ok
}

// PutMatch stores a match.
func (s *Store) PutMatch(src, dst domain.Name, srcID string, track domain.Track, confidence float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Matches[MatchKey(src, dst, srcID)] = Match{
		TargetID:      track.ID,
		TargetAlbumID: track.AlbumID,
		Title:         track.Title,
		Artists:       append([]string(nil), track.Artists...),
		DurationMS:    track.DurationMS,
		Confidence:    confidence,
		UpdatedAt:     time.Now().UTC(),
	}
	s.dirty = true
}

// PlaylistKey builds the link key for a source playlist.
func PlaylistKey(src, dst domain.Name, srcID string) string {
	return string(src) + ":playlist:" + srcID + "->" + string(dst)
}

// LookupPlaylist returns a cached playlist link.
func (s *Store) LookupPlaylist(src, dst domain.Name, srcID string) (PlaylistLink, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	link, ok := s.data.Playlists[PlaylistKey(src, dst, srcID)]
	return link, ok
}

// PutPlaylist stores a playlist link.
func (s *Store) PutPlaylist(src, dst domain.Name, srcID, targetID, targetName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Playlists[PlaylistKey(src, dst, srcID)] = PlaylistLink{
		TargetID:   targetID,
		TargetName: targetName,
		UpdatedAt:  time.Now().UTC(),
	}
	s.dirty = true
}

// ArtistKey builds the cache key for a source artist.
func ArtistKey(src, dst domain.Name, srcID string) string {
	return string(src) + ":artist:" + srcID + "->" + string(dst)
}

// LookupArtist returns a cached artist link.
func (s *Store) LookupArtist(src, dst domain.Name, srcID string) (ArtistLink, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	link, ok := s.data.Artists[ArtistKey(src, dst, srcID)]
	return link, ok
}

// PutArtist stores an artist link.
func (s *Store) PutArtist(src, dst domain.Name, srcID, targetID, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Artists[ArtistKey(src, dst, srcID)] = ArtistLink{
		TargetID:  targetID,
		Name:      name,
		UpdatedAt: time.Now().UTC(),
	}
	s.dirty = true
}

// Stats describes the number of cached entries.
type Stats struct {
	Matches   int `json:"matches"`
	Playlists int `json:"playlists"`
	Artists   int `json:"artists"`
}

// Stats returns cache sizes.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Matches:   len(s.data.Matches),
		Playlists: len(s.data.Playlists),
		Artists:   len(s.data.Artists),
	}
}

// Clear resets the state and removes the state file.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = newData()
	s.dirty = false
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
