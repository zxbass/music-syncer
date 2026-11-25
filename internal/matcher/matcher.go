// Package matcher finds the same recording in another music catalog.
//
// Matching is deliberately conservative: a wrong link is worse than an
// unmatched track, because the unmatched track is visible in the report while
// a wrong link silently pollutes a playlist.
package matcher

import (
	"regexp"
	"sort"
	"strings"

	"github.com/zxbass/music-syncer/internal/domain"
)

// DefaultThreshold is the minimum score accepted by Best.
const DefaultThreshold = 0.72

// Matcher scores track candidates.
type Matcher struct {
	Threshold  float64
	MinTitle   float64
	MinArtist  float64
	DurationOK int // milliseconds; larger differences are rejected when known
}

// New creates a matcher with the given threshold. A non-positive threshold
// selects DefaultThreshold.
func New(threshold float64) *Matcher {
	if threshold <= 0 || threshold > 1 {
		threshold = DefaultThreshold
	}
	return &Matcher{
		Threshold:  threshold,
		MinTitle:   0.60,
		MinArtist:  0.45,
		DurationOK: 30_000,
	}
}

// Candidate is a scored search result.
type Candidate struct {
	Track         domain.Track `json:"track"`
	Score         float64      `json:"score"`
	TitleScore    float64      `json:"title_score"`
	ArtistScore   float64      `json:"artist_score"`
	DurationScore float64      `json:"duration_score"`
	// ISRCMatch means both sides carry the same ISRC. This is a strong
	// identity signal and bypasses the usual thresholds.
	ISRCMatch bool `json:"isrc_match,omitempty"`
	// StrongVersionConflict means one side is e.g. a live/remix/acoustic
	// recording and the other is not. Such candidates are never accepted.
	StrongVersionConflict bool `json:"strong_version_conflict,omitempty"`
}

// Rank scores and sorts candidates. Rejected candidates are still returned so
// reports can show near misses.
func (m *Matcher) Rank(src domain.Track, candidates []domain.Track) []Candidate {
	ranked := make([]Candidate, 0, len(candidates))
	for _, cand := range candidates {
		if cand.ID == "" {
			continue
		}
		ranked = append(ranked, m.Score(src, cand))
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		if ranked[i].TitleScore != ranked[j].TitleScore {
			return ranked[i].TitleScore > ranked[j].TitleScore
		}
		if ranked[i].ArtistScore != ranked[j].ArtistScore {
			return ranked[i].ArtistScore > ranked[j].ArtistScore
		}
		return ranked[i].Track.ID < ranked[j].Track.ID
	})
	return ranked
}

// Best returns the best acceptable candidate.
func (m *Matcher) Best(src domain.Track, candidates []domain.Track) (Candidate, bool) {
	ranked := m.Rank(src, candidates)
	if len(ranked) == 0 {
		return Candidate{}, false
	}
	if m.Accept(ranked[0], src) {
		return ranked[0], true
	}
	return Candidate{}, false
}

// Score computes a single candidate's score.
func (m *Matcher) Score(src, cand domain.Track) Candidate {
	title := TitleSimilarity(src.Title, cand.Title)
	artist := ArtistSimilarity(src.Artists, cand.Artists)
	duration := DurationScore(src.DurationMS, cand.DurationMS)
	sameISRC := src.ISRC != "" && cand.ISRC != "" && strings.EqualFold(src.ISRC, cand.ISRC)
	score := 0.55*title.Score + 0.33*artist + 0.12*duration
	if src.Album != "" && cand.Album != "" && Normalize(src.Album) == Normalize(cand.Album) {
		score += 0.03
	}
	if sameISRC {
		score = 1
	}
	if score > 1 {
		score = 1
	}
	return Candidate{
		Track:                 cand,
		Score:                 score,
		TitleScore:            title.Score,
		ArtistScore:           artist,
		DurationScore:         duration,
		ISRCMatch:             sameISRC,
		StrongVersionConflict: title.StrongConflict && !sameISRC,
	}
}

// Accept applies hard rules on top of the score.
func (m *Matcher) Accept(c Candidate, src domain.Track) bool {
	if c.ISRCMatch {
		return true
	}
	if c.Score < m.Threshold {
		return false
	}
	if c.TitleScore < m.MinTitle {
		return false
	}
	if c.ArtistScore < m.MinArtist {
		return false
	}
	if c.StrongVersionConflict {
		return false
	}
	if src.DurationMS > 0 && c.Track.DurationMS > 0 {
		diff := src.DurationMS - c.Track.DurationMS
		if diff < 0 {
			diff = -diff
		}
		if diff > m.DurationOK {
			return false
		}
	}
	return true
}

// TitleResult is the outcome of comparing two titles.
type TitleResult struct {
	Score          float64
	StrongConflict bool
	WeakConflict   bool
}

// TitleSimilarity compares track titles, tolerating remaster/feat suffixes
// while rejecting live/remix/acoustic substitutions.
func TitleSimilarity(a, b string) TitleResult {
	ia := parseTitle(a)
	ib := parseTitle(b)

	baseScore := 1.0
	if ia.BaseNorm != ib.BaseNorm {
		baseScore = bestStringSimilarity(ia.BaseRaw, ib.BaseRaw, ia.BaseNorm, ib.BaseNorm)
		if baseScore < 0.25 && ia.BaseNorm != "" && ib.BaseNorm != "" {
			// Try the complete titles too: sometimes one side puts the version
			// marker in the title and the other in a separate field.
			alt := bestStringSimilarity(a, b, Normalize(a), Normalize(b))
			if alt > baseScore {
				baseScore = alt
			}
		}
	}

	result := TitleResult{Score: baseScore}

	partA, partB := partKey(ia.BaseRaw), partKey(ib.BaseRaw)
	if partA != "" && partB != "" && partA != partB {
		result.StrongConflict = true
		result.Score *= 0.4
	}

	strongA, strongB := ia.StrongVersions, ib.StrongVersions
	weakA, weakB := ia.WeakVersions, ib.WeakVersions

	strongMismatch := symmetricDiffCount(strongA, strongB)
	weakMismatch := symmetricDiffCount(weakA, weakB)

	switch {
	case strongMismatch > 0 && len(strongA) > 0 && len(strongB) > 0:
		// Two different versions, e.g. live vs remix.
		result.StrongConflict = true
		result.Score *= 0.45
	case strongMismatch > 0:
		// One version marker is absent on the other side.
		result.StrongConflict = true
		result.Score *= 0.55
	case weakMismatch > 0:
		result.WeakConflict = true
		result.Score *= 0.9
	}
	if result.Score < 0 {
		result.Score = 0
	}
	return result
}

func symmetricDiffCount(a, b map[string]bool) int {
	n := 0
	for k := range a {
		if !b[k] {
			n++
		}
	}
	for k := range b {
		if !a[k] {
			n++
		}
	}
	return n
}

// ArtistSimilarity compares an artist list to another artist list.
func ArtistSimilarity(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0.5
	}
	best := 0.0
	for _, x := range a {
		for _, y := range b {
			if s := artistPairSimilarity(x, y); s > best {
				best = s
			}
		}
	}
	// Also compare the complete credited lines; this catches "A, B" vs
	// separate artist objects with reordered names.
	lineScore := bestStringSimilarity(strings.Join(a, ", "), strings.Join(b, ", "),
		Normalize(strings.Join(a, ", ")), Normalize(strings.Join(b, ", ")))
	if lineScore > best {
		best = lineScore
	}
	return best
}

func artistPairSimilarity(a, b string) float64 {
	na, nb := Normalize(a), Normalize(b)
	if na == "" || nb == "" {
		return 0
	}
	if na == nb {
		return 1
	}
	// Collapse "feat"/"ft" credits: a featuring credit is not a different
	// artist for matching purposes.
	na = stripFeat(na)
	nb = stripFeat(nb)
	if na == nb && na != "" {
		return 1
	}

	best := bestStringSimilarity(a, b, na, nb)
	// "The Weeknd" vs "Weeknd".
	ta, tb := stripLeadingThe(na), stripLeadingThe(nb)
	if ta == tb && ta != "" {
		return 0.97
	}
	if len(ta) >= 4 && len(tb) >= 4 {
		if strings.Contains(ta, tb) || strings.Contains(tb, ta) {
			shortLen := min(len([]rune(ta)), len([]rune(tb)))
			longLen := max(len([]rune(ta)), len([]rune(tb)))
			containment := 0.8 + 0.15*float64(shortLen)/float64(longLen)
			if containment > best {
				best = containment
			}
		}
	}
	return best
}

var featRe = regexp.MustCompile(`\b(feat|feat\.|ft|ft\.|featuring|при уч\.?)\b.*$`)

func stripFeat(s string) string {
	return strings.TrimSpace(featRe.ReplaceAllString(s, ""))
}

func stripLeadingThe(s string) string {
	return strings.TrimSpace(strings.TrimPrefix(s, "the "))
}

// DurationScore compares durations. An unknown duration is neutral.
func DurationScore(a, b int) float64 {
	if a <= 0 || b <= 0 {
		return 0.5
	}
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	switch {
	case diff <= 3000:
		return 1
	case diff <= 8000:
		return 0.85
	case diff <= 15000:
		return 0.6
	case diff <= 30000:
		return 0.25
	default:
		return 0
	}
}

// ArtistCandidate is a scored artist search result.
type ArtistCandidate struct {
	Artist domain.Artist `json:"artist"`
	Score  float64       `json:"score"`
}

// BestArtist returns the best artist match above the threshold.
func (m *Matcher) BestArtist(src domain.Artist, candidates []domain.Artist) (ArtistCandidate, bool) {
	best := ArtistCandidate{}
	for _, cand := range candidates {
		if cand.ID == "" {
			continue
		}
		s := artistPairSimilarity(src.Name, cand.Name)
		if s > best.Score {
			best = ArtistCandidate{Artist: cand, Score: s}
		}
	}
	if best.Score >= 0.80 {
		return best, true
	}
	return ArtistCandidate{}, false
}

type titleInfo struct {
	BaseRaw        string
	BaseNorm       string
	StrongVersions map[string]bool
	WeakVersions   map[string]bool
}

var (
	parenthetical = regexp.MustCompile(`[\(\[\{]([^\)\]\}]*)[\)\]\}]`)
	yearSuffix    = regexp.MustCompile(`^(19|20)\d{2}$`)
	partNumber    = regexp.MustCompile(`\b(?:part|pt|vol|volume)\s*([0-9]+|[ivxlc]+)\b`)
)

func partKey(title string) string {
	match := partNumber.FindStringSubmatch(Normalize(title))
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

var strongMarkers = []string{
	"live", "remix", "rmx", "acoustic", "instrumental", "karaoke", "cover",
	"demo", "unplugged", "nightcore", "sped up", "slowed", "8d", "reprise",
	"orchestral", "piano version", "a cappella", "acapella",
}

var weakMarkers = []string{
	"radio edit", "radio version", "single version", "edit", "extended",
	"club mix", "dub", "vip mix", "bootleg", "mashup",
}

var neutralMarkers = []string{
	"remaster", "remastered", "deluxe", "explicit", "clean", "mono", "stereo",
	"album version", "bonus", "anniversary", "expanded", "reissue", "special edition",
	"digital remaster", "digitally remastered", "original mix", "soundtrack",
	"from the soundtrack", "original motion picture soundtrack",
}

func parseTitle(raw string) titleInfo {
	info := titleInfo{
		StrongVersions: map[string]bool{},
		WeakVersions:   map[string]bool{},
	}
	lower := strings.ToLower(strings.TrimSpace(raw))
	if lower == "" {
		return info
	}

	// Inspect parenthetical/bracketed segments. Version markers are removed
	// from the base; meaningful parts such as "(Part 2)" stay in the title.
	base := parenthetical.ReplaceAllStringFunc(lower, func(match string) string {
		if len(match) < 2 {
			return " "
		}
		inner := strings.TrimSpace(match[1 : len(match)-1])
		if inner == "" {
			return " "
		}
		before := len(info.StrongVersions) + len(info.WeakVersions)
		markVersion(inner, info.StrongVersions, info.WeakVersions)
		after := len(info.StrongVersions) + len(info.WeakVersions)
		normalized := Normalize(inner)
		if after > before || isNeutral(inner) || featRe.MatchString(inner) ||
			strings.HasPrefix(normalized, "from ") || normalized == "ost" {
			return " "
		}
		return " " + inner + " "
	})

	// Split " - " suffixes. Version suffixes are removed; non-version parts
	// such as "Part 2" stay in the title.
	parts := regexp.MustCompile(`\s+[-–—]\s+`).Split(base, -1)
	kept := []string{parts[0]}
	for _, part := range parts[1:] {
		cleaned := strings.TrimSpace(part)
		if cleaned == "" {
			continue
		}
		before := len(info.StrongVersions) + len(info.WeakVersions)
		markVersion(cleaned, info.StrongVersions, info.WeakVersions)
		after := len(info.StrongVersions) + len(info.WeakVersions)
		isYear := yearSuffix.MatchString(cleaned)
		isFeat := featRe.MatchString(cleaned)
		if after > before || isYear || isFeat || isNeutral(cleaned) {
			continue
		}
		kept = append(kept, cleaned)
	}
	base = strings.Join(kept, " ")
	base = featRe.ReplaceAllString(base, " ")
	base = strings.TrimSpace(base)
	if base == "" {
		base = lower
	}
	info.BaseRaw = base
	info.BaseNorm = Normalize(base)
	return info
}

func markVersion(segment string, strong, weak map[string]bool) {
	s := " " + Normalize(segment) + " "
	for _, marker := range strongMarkers {
		if strings.Contains(s, " "+marker+" ") {
			strong[marker] = true
		}
	}
	for _, marker := range weakMarkers {
		if strings.Contains(s, " "+marker+" ") {
			weak[marker] = true
		}
	}
}

func isNeutral(segment string) bool {
	s := " " + Normalize(segment) + " "
	for _, marker := range neutralMarkers {
		if strings.Contains(s, " "+marker+" ") {
			return true
		}
	}
	return false
}

func bestStringSimilarity(rawA, rawB, normA, normB string) float64 {
	best := stringSimilarity(normA, normB)
	// Try Cyrillic -> Latin transliteration in both directions.
	trA := Normalize(translit(rawA))
	trB := Normalize(translit(rawB))
	if trA != "" && trB != "" {
		best = max(best, stringSimilarity(trA, trB))
	}
	if trA != "" {
		best = max(best, stringSimilarity(trA, normB))
	}
	if trB != "" {
		best = max(best, stringSimilarity(normA, trB))
	}
	return best
}

func stringSimilarity(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	ra, rb := []rune(a), []rune(b)
	lev := 1 - float64(levenshtein(ra, rb))/float64(max(len(ra), len(rb)))
	jac := tokenJaccard(a, b)
	score := 0.65*lev + 0.35*jac
	// Substring containment is a strong signal for shortened titles.
	if len(ra) >= 4 && len(rb) >= 4 {
		if strings.Contains(a, b) || strings.Contains(b, a) {
			shortLen := min(len(ra), len(rb))
			longLen := max(len(ra), len(rb))
			score = max(score, 0.75+0.2*float64(shortLen)/float64(longLen))
		}
	}
	return score
}

func tokenJaccard(a, b string) float64 {
	ta := strings.Fields(a)
	tb := strings.Fields(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	set := make(map[string]bool, len(ta))
	for _, t := range ta {
		set[t] = true
	}
	common := 0
	for _, t := range tb {
		if set[t] {
			common++
		}
	}
	return float64(common) / float64(len(ta)+len(tb)-common)
}

func levenshtein(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := 0; j <= len(b); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, min(curr[j-1]+1, prev[j-1]+cost))
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

var nonWord = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// Normalize lowercases text, folds common diacritics, converts ё to е and
// replaces all punctuation with single spaces.
func Normalize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == 'ё' {
			r = 'е'
		}
		if folded, ok := diacritics[r]; ok {
			r = folded
		}
		if r == '&' {
			b.WriteString(" and ")
			continue
		}
		if r == '\'' || r == '’' || r == '`' {
			continue
		}
		b.WriteRune(r)
	}
	s = nonWord.ReplaceAllString(b.String(), " ")
	return strings.Join(strings.Fields(s), " ")
}

// TitleKeys returns lookup keys for an already-normalized title. It strips
// version suffixes and includes a transliteration of Cyrillic titles.
func TitleKeys(title string) []string {
	info := parseTitle(title)
	keys := []string{info.BaseNorm}
	tr := Normalize(translit(info.BaseRaw))
	if tr != "" && tr != info.BaseNorm {
		keys = append(keys, tr)
	}
	return keys
}

// ArtistKeys returns normalized lookup keys for an artist name.
func ArtistKeys(name string) []string {
	n := Normalize(name)
	keys := []string{n}
	if tr := Normalize(translit(name)); tr != "" && tr != n {
		keys = append(keys, tr)
	}
	return keys
}

var translitMap = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e",
	'ж': "zh", 'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m",
	'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u",
	'ф': "f", 'х': "kh", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "shch",
	'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
	'і': "i", 'ї': "yi", 'є': "ye", 'ґ': "g",
}

func translit(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if mapped, ok := translitMap[r]; ok {
			b.WriteString(mapped)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

var diacritics = map[rune]rune{
	'à': 'a', 'á': 'a', 'â': 'a', 'ä': 'a', 'ã': 'a', 'å': 'a', 'ā': 'a',
	'ç': 'c', 'ć': 'c', 'č': 'c',
	'è': 'e', 'é': 'e', 'ê': 'e', 'ë': 'e', 'ē': 'e', 'ę': 'e',
	'ì': 'i', 'í': 'i', 'î': 'i', 'ï': 'i', 'ī': 'i',
	'ñ': 'n', 'ń': 'n',
	'ò': 'o', 'ó': 'o', 'ô': 'o', 'ö': 'o', 'õ': 'o', 'ø': 'o', 'ō': 'o',
	'ù': 'u', 'ú': 'u', 'û': 'u', 'ü': 'u', 'ū': 'u',
	'ý': 'y', 'ÿ': 'y',
	'š': 's', 'ś': 's', 'ž': 'z', 'ź': 'z', 'ż': 'z',
	'ł': 'l', 'đ': 'd', 'ð': 'd', 'þ': 'y',
}
