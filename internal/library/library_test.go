// SPDX-License-Identifier: GPL-3.0-or-later

package library

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProber returns metadata derived from the file name: "Artist - Title.mp3".
type fakeProber struct{ calls int }

func (f *fakeProber) Probe(_ context.Context, path string) (*Probe, error) {
	f.calls++
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if strings.Contains(base, "broken") {
		return nil, errors.New("corrupt")
	}
	artist, title, _ := strings.Cut(base, " - ")
	return &Probe{Format: FormatMP3, Artist: artist, Title: title, Album: filepath.Base(filepath.Dir(path)), Duration: 180, BPM100: 12800}, nil
}

func makeTree(t *testing.T, files ...string) string {
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestScanTreeAndEntities(t *testing.T) {
	root := makeTree(t,
		"House/Alpha - One.mp3",
		"House/Alpha - Two.mp3",
		"House/Deep/Beta - Three.flac",
		"Techno/beta - Four.wav", // artist case differs: same artist
		"Techno/broken.mp3",
		"Techno/notes.txt",
		".hidden/Gamma - Five.mp3",
		"Techno/._Alpha - One.mp3",
		"loose - Six.aiff",
	)
	fp := &fakeProber{}
	lib, err := Scan(context.Background(), root, ScanOptions{Prober: fp, CachePath: filepath.Join(t.TempDir(), "cache.json")})
	if err != nil {
		t.Fatal(err)
	}
	if lib.TrackCount() != 5 {
		var names []string
		for _, tr := range lib.Tracks() {
			names = append(names, tr.RelPath)
		}
		t.Fatalf("want 5 tracks, got %d: %v", lib.TrackCount(), names)
	}
	titles := []string{}
	for _, tr := range lib.Tracks() {
		titles = append(titles, tr.Title)
	}
	if strings.Join(titles, ",") != "Four,One,Six,Three,Two" {
		t.Fatalf("tracks not sorted by title: %v", titles)
	}
	if n := len(lib.Artists()); n != 3 {
		t.Fatalf("want 3 artists (Alpha, beta/Beta, loose), got %d", n)
	}
	for _, a := range lib.Artists() {
		if strings.EqualFold(a.Name, "beta") && len(a.Tracks) != 2 {
			t.Fatalf("Beta should have 2 tracks, has %d", len(a.Tracks))
		}
	}
	root0, _ := lib.Folder(RootFolderID)
	if len(root0.Folders) != 2 || len(root0.Tracks) != 1 {
		t.Fatalf("root folder: %d folders, %d tracks", len(root0.Folders), len(root0.Tracks))
	}
	house, _ := lib.Folder(root0.Folders[0])
	if house.Name != "House" || len(house.Folders) != 1 || len(house.Tracks) != 2 {
		t.Fatalf("House folder: %+v", house)
	}
	for _, tr := range lib.Tracks() {
		if got, ok := lib.Track(tr.ID); !ok || got != tr || tr.ID == 0 {
			t.Fatalf("lookup of %d failed", tr.ID)
		}
	}

	// A rescan reuses the cache and yields identical IDs.
	cache := filepath.Join(t.TempDir(), "c.json")
	fp2 := &fakeProber{}
	lib1, _ := Scan(context.Background(), root, ScanOptions{Prober: fp2, CachePath: cache})
	first := fp2.calls
	lib2, _ := Scan(context.Background(), root, ScanOptions{Prober: fp2, CachePath: cache})
	if fp2.calls != first+1 { // only the broken file is probed again
		t.Fatalf("cache not used: %d probes after rescan (first scan %d)", fp2.calls, first)
	}
	for _, tr := range lib1.Tracks() {
		if t2, ok := lib2.Track(tr.ID); !ok || t2.RelPath != tr.RelPath {
			t.Fatalf("track ID %d not stable across scans", tr.ID)
		}
	}
}

func TestScanMissingRoot(t *testing.T) {
	if _, err := Scan(context.Background(), "/nonexistent/slipmat", ScanOptions{Prober: &fakeProber{}}); err == nil {
		t.Fatal("expected error")
	}
}

// TestFFprobeRealFiles generates short tagged files with ffmpeg in every
// format the CDJ-3000 supports and checks the probe results.
func TestFFprobeRealFiles(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	type tc struct {
		file   string
		args   []string
		format Format
	}
	cases := []tc{
		{"a.mp3", []string{"-c:a", "libmp3lame", "-b:a", "192k", "-id3v2_version", "3"}, FormatMP3},
		{"b.m4a", []string{"-c:a", "aac", "-b:a", "128k"}, FormatAAC},
		{"c.flac", []string{"-c:a", "flac"}, FormatFLAC},
		{"d.wav", []string{"-c:a", "pcm_s16le", "-write_id3v2", "1"}, FormatWAV},
		{"e.aiff", []string{"-c:a", "pcm_s16be", "-write_id3v2", "1"}, FormatAIFF},
	}
	for _, c := range cases {
		args := append([]string{"-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=3", "-ar", "44100",
			"-metadata", "title=Täst Title", "-metadata", "artist=Slip Artist", "-metadata", "album=Mat Album",
			"-metadata", "genre=Techno", "-metadata", "TBPM=128", "-metadata", "BPM=128", "-metadata", "date=2024"}, c.args...)
		args = append(args, filepath.Join(dir, c.file))
		if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %s: %v %s", c.file, err, out)
		}
	}
	os.WriteFile(filepath.Join(dir, "junk.mp3"), []byte("not audio"), 0o644)
	for _, c := range cases {
		p, err := FFprobe{}.Probe(context.Background(), filepath.Join(dir, c.file))
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if p.Format != c.format || p.Title != "Täst Title" || p.Artist != "Slip Artist" || p.Duration != 3 || p.SampleRate != 44100 {
			t.Errorf("%s: %+v", c.file, p)
		}
		if c.format == FormatMP3 && (p.BPM100 != 12800 || p.Year != 2024 || p.Bitrate < 150) {
			t.Errorf("%s: bpm/year/bitrate: %+v", c.file, p)
		}
	}
	if _, err := (FFprobe{}).Probe(context.Background(), filepath.Join(dir, "junk.mp3")); err == nil {
		t.Error("junk file probed without error")
	}
	lib, err := Scan(context.Background(), dir, ScanOptions{Prober: FFprobe{}})
	if err != nil || lib.TrackCount() != len(cases) {
		t.Fatalf("scan: %v tracks=%d", err, lib.TrackCount())
	}
}
