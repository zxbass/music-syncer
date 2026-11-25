package matcher

import (
	"testing"

	"github.com/zxbass/music-syncer/internal/domain"
)

func track(title string, artists ...string) domain.Track {
	return domain.Track{Provider: domain.Yandex, ID: title, Title: title, Artists: artists, DurationMS: 180000}
}

func TestNormalize(t *testing.T) {
	tests := map[string]string{
		"Ёлка & Пёс":         "елка and пес",
		"Beyoncé — Halo":     "beyonce halo",
		"Don't Stop Me Now!": "dont stop me now",
		"  AC/DC  ":          "ac dc",
		"Мумий Тролль":       "мумий тролль",
	}
	for input, want := range tests {
		if got := Normalize(input); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestTranslitKeys(t *testing.T) {
	keys := TitleKeys("Песня без слов")
	want := "pesnya bez slov"
	found := false
	for _, key := range keys {
		if key == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("TitleKeys = %v, want to contain %q", keys, want)
	}

	artistKeys := ArtistKeys("Мумий Тролль")
	found = false
	for _, key := range artistKeys {
		if key == "mumiy troll" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ArtistKeys = %v, want to contain transliteration", artistKeys)
	}
}

func TestTitleSimilarityVersions(t *testing.T) {
	tests := []struct {
		name     string
		a, b     string
		minScore float64
		maxScore float64
		conflict bool
	}{
		{"exact", "Song", "Song", 1, 1, false},
		{"remaster", "Song", "Song (Remastered 2011)", 0.9, 1, false},
		{"dash remaster", "Song", "Song - 2011 Remaster", 0.9, 1, false},
		{"feat", "Song (feat. Guest)", "Song", 0.9, 1, false},
		{"live", "Song", "Song (Live)", 0, 0.7, true},
		{"remix", "Song", "Song (Bootleg Remix)", 0, 0.7, true},
		{"different songs", "Song One", "Song Two", 0, 0.75, false},
		{"parts differ", "Song (Part 2)", "Song (Part 3)", 0, 0.5, true},
		{"transliteration", "Песня без слов", "Pesnya bez slov", 0.9, 1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TitleSimilarity(tc.a, tc.b)
			if got.Score < tc.minScore || got.Score > tc.maxScore {
				t.Errorf("score = %.3f, want [%.2f, %.2f]", got.Score, tc.minScore, tc.maxScore)
			}
			if got.StrongConflict != tc.conflict {
				t.Errorf("strong conflict = %v, want %v", got.StrongConflict, tc.conflict)
			}
		})
	}
}

func TestArtistSimilarity(t *testing.T) {
	tests := []struct {
		a, b []string
		min  float64
	}{
		{[]string{"Daft Punk"}, []string{"Daft Punk"}, 0.99},
		{[]string{"The Weeknd"}, []string{"Weeknd"}, 0.9},
		{[]string{"Daft Punk"}, []string{"Daft Punk", "Pharrell Williams"}, 0.8},
		{[]string{"Мумий Тролль"}, []string{"Mumiy Troll"}, 0.9},
		{[]string{"Artist A"}, []string{"Artist B"}, 0.0},
	}
	for _, tc := range tests {
		if got := ArtistSimilarity(tc.a, tc.b); got < tc.min {
			t.Errorf("ArtistSimilarity(%v, %v) = %.3f, want >= %.2f", tc.a, tc.b, got, tc.min)
		}
	}
}

func TestDurationScore(t *testing.T) {
	if got := DurationScore(180000, 181000); got != 1 {
		t.Errorf("close durations: got %.2f, want 1", got)
	}
	if got := DurationScore(180000, 190000); got < 0.5 {
		t.Errorf("10s difference: got %.2f, want >= 0.5", got)
	}
	if got := DurationScore(180000, 0); got != 0.5 {
		t.Errorf("unknown duration: got %.2f, want 0.5", got)
	}
}

func TestBestAndAccept(t *testing.T) {
	m := New(DefaultThreshold)
	src := domain.Track{
		Provider: domain.Yandex, ID: "1", Title: "Blinding Lights",
		Artists: []string{"The Weeknd"}, Album: "After Hours", DurationMS: 200040,
	}
	candidates := []domain.Track{
		{Provider: domain.Spotify, ID: "wrong", Title: "Blinding Lights (Live)", Artists: []string{"The Weeknd"}, DurationMS: 230000},
		{Provider: domain.Spotify, ID: "right", Title: "Blinding Lights", Artists: []string{"The Weeknd"}, Album: "After Hours", DurationMS: 200040},
		{Provider: domain.Spotify, ID: "cover", Title: "Blinding Lights", Artists: []string{"Some Cover Band"}, DurationMS: 200000},
	}
	best, ok := m.Best(src, candidates)
	if !ok {
		t.Fatal("expected a match")
	}
	if best.Track.ID != "right" {
		t.Fatalf("matched %q, want right", best.Track.ID)
	}
	if best.Score < 0.95 {
		t.Errorf("score = %.3f, want >= 0.95", best.Score)
	}

	liveOnly := []domain.Track{
		{Provider: domain.Spotify, ID: "live", Title: "Blinding Lights (Live)", Artists: []string{"The Weeknd"}, DurationMS: 200040},
	}
	if _, ok := m.Best(src, liveOnly); ok {
		t.Fatal("live version must not be accepted for a studio recording")
	}

	partSrc := domain.Track{Provider: domain.Yandex, ID: "p2", Title: "Song (Part 2)", Artists: []string{"Artist"}, DurationMS: 180000}
	partCand := []domain.Track{
		{Provider: domain.Spotify, ID: "p3", Title: "Song (Part 3)", Artists: []string{"Artist"}, DurationMS: 180000},
	}
	if _, ok := m.Best(partSrc, partCand); ok {
		t.Fatal("a different part must not be accepted")
	}
}

func TestISRCMatchBypassesTitleThresholds(t *testing.T) {
	m := New(DefaultThreshold)
	src := domain.Track{
		Provider: domain.Yandex, ID: "1", Title: "Song (Live)", Artists: []string{"Artist"},
		DurationMS: 180000, ISRC: "USRC12345678",
	}
	cand := domain.Track{
		Provider: domain.Spotify, ID: "s1", Title: "Song - Studio Version", Artists: []string{"Artist"},
		DurationMS: 181000, ISRC: "usrc12345678",
	}
	best, ok := m.Best(src, []domain.Track{cand})
	if !ok || best.Track.ID != "s1" || !best.ISRCMatch {
		t.Fatalf("ISRC match not accepted: %+v, ok=%v", best, ok)
	}
}

func TestBestSkipsRejectedTopCandidate(t *testing.T) {
	m := New(DefaultThreshold)
	src := domain.Track{
		Provider: domain.Yandex, ID: "1", Title: "Song",
		Artists: []string{"Artist"}, DurationMS: 180000,
	}
	candidates := []domain.Track{
		{Provider: domain.Spotify, ID: "long", Title: "Song", Artists: []string{"Artist"}, DurationMS: 300000},
		{Provider: domain.Spotify, ID: "studio", Title: "Song (Original)", Artists: []string{"Artist"}, DurationMS: 180000},
	}
	best, ok := m.Best(src, candidates)
	if !ok {
		t.Fatal("expected the acceptable second candidate")
	}
	if best.Track.ID != "studio" {
		t.Fatalf("matched %q, want studio", best.Track.ID)
	}
}

func TestBestArtistReturnsNearMiss(t *testing.T) {
	m := New(DefaultThreshold)
	src := domain.Artist{Provider: domain.Yandex, ID: "1", Name: "Abcdef"}
	candidates := []domain.Artist{{Provider: domain.Spotify, ID: "a1", Name: "Abcxyz"}}
	best, ok := m.BestArtist(src, candidates)
	if ok {
		t.Fatalf("unexpected match: %+v", best)
	}
	if best.Artist.ID != "a1" || best.Score <= 0 {
		t.Fatalf("near miss was discarded: %+v", best)
	}
}

func TestBestArtist(t *testing.T) {
	m := New(DefaultThreshold)
	src := domain.Artist{Provider: domain.Yandex, ID: "1", Name: "Мумий Тролль"}
	candidates := []domain.Artist{
		{Provider: domain.Spotify, ID: "a", Name: "Mumiy Troll"},
		{Provider: domain.Spotify, ID: "b", Name: "Mumiy Troll Tribute"},
	}
	best, ok := m.BestArtist(src, candidates)
	if !ok || best.Artist.ID != "a" {
		t.Fatalf("got %+v, ok=%v; want artist a", best, ok)
	}
}
