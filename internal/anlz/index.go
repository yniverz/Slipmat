// SPDX-License-Identifier: GPL-3.0-or-later

package anlz

import (
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/unicode/norm"
)

// Set is the analysis files of one track (any may be empty).
type Set struct {
	DAT, EXT, EX2 string
	TrackPath     string    // PPTH of the .DAT
	ModTime       time.Time // of the .DAT
}

// TrackRef identifies a library track for matching.
type TrackRef struct {
	ID      uint32
	RelPath string // slash-separated, relative to the music folder
}

// DefaultDirs returns where analysis is looked for by default: rekordbox's
// own analysis folder and any PIONEER/USBANLZ folder copied into the music
// folder from a USB export.
func DefaultDirs(musicRoot string) []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "Library", "Pioneer", "rekordbox", "share", "PIONEER", "USBANLZ"))
	}
	if musicRoot != "" {
		dirs = append(dirs, musicRoot)
	}
	return dirs
}

// Index maps track IDs to their analysis files.
type Index struct {
	sets map[uint32]Set

	mu    sync.Mutex
	cache map[uint32]*Analysis
	order []uint32
}

// Stats reports what BuildIndex found.
type Stats struct {
	Files, Matched, Ambiguous, Unmatched int
}

// matchKey normalises a file name for comparison: macOS stores names
// decomposed (NFD) while rekordbox writes them composed (NFC).
func matchKey(name string) string { return strings.ToLower(norm.NFC.String(name)) }

type candidate struct {
	set   Set
	parts []string // normalised PPTH path components (nil for "?/name")
}

// BuildIndex scans dirs for ANLZ*.DAT files and matches them to tracks by
// file name. When several tracks share a file name, the PPTH path (USB
// exports record the full path) must identify one of them by its parent
// folders; otherwise the match is skipped as ambiguous. When several
// analyses exist for the same track (re-analysis), the newest wins.
func BuildIndex(dirs []string, tracks []TrackRef, log *slog.Logger) (*Index, Stats) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	byName := map[string][]TrackRef{}
	for _, t := range tracks {
		k := matchKey(path.Base(t.RelPath))
		byName[k] = append(byName[k], t)
	}
	ix := &Index{sets: map[uint32]Set{}, cache: map[uint32]*Analysis{}}
	var st Stats
	seenDir := map[string]bool{}
	for _, dir := range dirs {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil || seenDir[real] {
			continue
		}
		seenDir[real] = true
		filepath.WalkDir(real, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if strings.HasPrefix(d.Name(), ".") && p != real {
					return fs.SkipDir
				}
				return nil
			}
			name := d.Name()
			if !strings.HasPrefix(strings.ToUpper(name), "ANLZ") || !strings.EqualFold(filepath.Ext(name), ".DAT") {
				return nil
			}
			st.Files++
			c, ok := readCandidate(p)
			if !ok {
				log.Debug("unreadable analysis file", "path", p)
				return nil
			}
			ix.match(c, byName, &st, log)
			return nil
		})
	}
	st.Matched = len(ix.sets)
	return ix, st
}

func readCandidate(datPath string) (candidate, bool) {
	f, err := ReadFile(datPath)
	if err != nil {
		return candidate{}, false
	}
	tp := f.TrackPath()
	if tp == "" {
		return candidate{}, false
	}
	info, _ := os.Stat(datPath)
	base := strings.TrimSuffix(datPath, filepath.Ext(datPath))
	set := Set{DAT: datPath, TrackPath: tp}
	if info != nil {
		set.ModTime = info.ModTime()
	}
	for _, ext := range []string{".EXT", ".ext"} {
		if _, err := os.Stat(base + ext); err == nil {
			set.EXT = base + ext
			break
		}
	}
	for _, ext := range []string{".2EX", ".2ex"} {
		if _, err := os.Stat(base + ext); err == nil {
			set.EX2 = base + ext
			break
		}
	}
	c := candidate{set: set}
	if !strings.HasPrefix(tp, "?/") {
		for _, part := range strings.Split(strings.ReplaceAll(tp, "\\", "/"), "/") {
			if part != "" {
				c.parts = append(c.parts, matchKey(part))
			}
		}
	}
	return c, true
}

func (ix *Index) match(c candidate, byName map[string][]TrackRef, st *Stats, log *slog.Logger) {
	name := c.set.TrackPath
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		name = name[i+1:]
	}
	tracks := byName[matchKey(name)]
	var target *TrackRef
	switch {
	case len(tracks) == 0:
		st.Unmatched++
		return
	case len(tracks) == 1:
		target = &tracks[0]
	default:
		// Pick the track sharing the most trailing path components.
		best, bestScore, tie := -1, 0, false
		for i, t := range tracks {
			score := suffixMatch(c.parts, t.RelPath)
			if score > bestScore {
				best, bestScore, tie = i, score, false
			} else if score == bestScore {
				tie = true
			}
		}
		if best < 0 || tie || bestScore < 2 {
			st.Ambiguous++
			log.Warn("analysis matches several tracks with the same file name; skipped", "analysis", c.set.DAT, "track_path", c.set.TrackPath, "candidates", len(tracks))
			return
		}
		target = &tracks[best]
	}
	if old, ok := ix.sets[target.ID]; ok && !c.set.ModTime.After(old.ModTime) {
		return // keep the newer analysis
	}
	ix.sets[target.ID] = c.set
}

// suffixMatch counts how many trailing path components of rel equal parts.
func suffixMatch(parts []string, rel string) int {
	if parts == nil {
		return 0
	}
	rp := strings.Split(rel, "/")
	n := 0
	for i, j := len(parts)-1, len(rp)-1; i >= 0 && j >= 0; i, j = i-1, j-1 {
		if parts[i] != matchKey(rp[j]) {
			break
		}
		n++
	}
	return n
}

// Set returns the analysis files for a track.
func (ix *Index) Set(id uint32) (Set, bool) {
	if ix == nil {
		return Set{}, false
	}
	s, ok := ix.sets[id]
	return s, ok
}

// Len returns the number of tracks with analysis.
func (ix *Index) Len() int {
	if ix == nil {
		return 0
	}
	return len(ix.sets)
}

// IDs returns the matched track IDs, sorted.
func (ix *Index) IDs() []uint32 {
	ids := make([]uint32, 0, len(ix.sets))
	for id := range ix.sets {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Sources of analysis data.
const (
	SourceRekordbox = "rekordbox"
	SourceSlipmat   = "slipmat" // generated waveforms only
)

// Analysis is a track's parsed analysis files.
type Analysis struct {
	Source        string
	DAT, EXT, EX2 *File
}

// FromRekordbox reports whether the data was written by rekordbox.
func (a *Analysis) FromRekordbox() bool { return a != nil && a.Source == SourceRekordbox }

// Section looks a tag up in the given file kind ("DAT", "EXT" or "2EX").
func (a *Analysis) Section(kind, tag string) (Section, bool) {
	if a == nil {
		return Section{}, false
	}
	switch strings.ToUpper(kind) {
	case "DAT":
		return a.DAT.Section(tag)
	case "EXT":
		return a.EXT.Section(tag)
	case "2EX":
		return a.EX2.Section(tag)
	}
	return Section{}, false
}

// Any looks a tag up in DAT, then EXT, then 2EX.
func (a *Analysis) Any(tag string) (Section, bool) {
	for _, k := range []string{"DAT", "EXT", "2EX"} {
		if s, ok := a.Section(k, tag); ok {
			return s, true
		}
	}
	return Section{}, false
}

const cacheTracks = 16

// Load returns the parsed analysis for a track (cached), or nil.
func (ix *Index) Load(id uint32) *Analysis {
	set, ok := ix.Set(id)
	if !ok {
		return nil
	}
	ix.mu.Lock()
	if a := ix.cache[id]; a != nil {
		ix.mu.Unlock()
		return a
	}
	ix.mu.Unlock()
	a := &Analysis{Source: SourceRekordbox}
	a.DAT, _ = ReadFile(set.DAT)
	if set.EXT != "" {
		a.EXT, _ = ReadFile(set.EXT)
	}
	if set.EX2 != "" {
		a.EX2, _ = ReadFile(set.EX2)
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if _, exists := ix.cache[id]; !exists {
		ix.order = append(ix.order, id)
	}
	ix.cache[id] = a
	for len(ix.order) > cacheTracks {
		delete(ix.cache, ix.order[0])
		ix.order = ix.order[1:]
	}
	return a
}
