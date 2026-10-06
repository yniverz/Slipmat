// SPDX-License-Identifier: GPL-3.0-or-later

package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Extensions of files we scan. The CDJ-3000 plays MP3, AAC, ALAC, FLAC,
// WAV and AIFF.
var audioExt = map[string]bool{
	".mp3": true, ".m4a": true, ".mp4": true, ".aac": true,
	".flac": true, ".wav": true, ".aif": true, ".aiff": true,
}

// Probe holds what a Prober learns about a file.
type Probe struct {
	Format                                           Format
	Title, Artist, Album, Genre, Key, Comment, Label string
	Year                                             int
	BPM100                                           uint32
	Duration                                         uint32
	Bitrate, SampleRate, SampleDepth                 uint32
}

// Prober reads metadata from an audio file.
type Prober interface {
	Probe(ctx context.Context, path string) (*Probe, error)
}

// ScanOptions configure Scan.
type ScanOptions struct {
	Prober    Prober
	CachePath string // metadata cache file ("" = no cache)
	Workers   int
	Log       *slog.Logger
}

// cacheEntry is one cached probe result, valid while size and mtime match.
type cacheEntry struct {
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
	Probe   *Probe    `json:"probe"`
}

const cacheVersion = 1

type cacheFile struct {
	Version int                    `json:"version"`
	Entries map[string]*cacheEntry `json:"entries"` // by absolute path
}

// Scan walks root, probes every audio file (using the cache where
// possible) and builds a Library. Unreadable or unsupported files are
// logged and skipped; they never abort the scan.
func Scan(ctx context.Context, root string, opt ScanOptions) (*Library, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("music folder %q is not a readable directory", root)
	}
	log := opt.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opt.Workers <= 0 {
		opt.Workers = min(runtime.NumCPU(), 8)
	}

	type job struct {
		path, rel string
		info      fs.FileInfo
	}
	var jobs []job
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			log.Warn("skipping unreadable path", "path", p, "err", err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") && p != root {
			if d.IsDir() {
				return fs.SkipDir // .Trashes, .Spotlight, AppleDouble dirs
			}
			return nil // ._ files and other dotfiles
		}
		if d.IsDir() || !audioExt[strings.ToLower(filepath.Ext(name))] {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		jobs = append(jobs, job{p, filepath.ToSlash(rel), info})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].rel < jobs[j].rel })

	cache := loadCache(opt.CachePath)
	results := make([]*Track, len(jobs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	next := make(chan int)
	probed, cached, failed := 0, 0, 0
	for w := 0; w < opt.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				j := jobs[i]
				mu.Lock()
				ce := cache.Entries[j.path]
				mu.Unlock()
				var p *Probe
				if ce != nil && ce.Size == j.info.Size() && ce.ModTime.Equal(j.info.ModTime()) && ce.Probe != nil {
					p = ce.Probe
					mu.Lock()
					cached++
					mu.Unlock()
				} else {
					var err error
					p, err = opt.Prober.Probe(ctx, j.path)
					if err != nil {
						log.Warn("skipping file: cannot read audio metadata", "path", j.rel, "err", err)
						mu.Lock()
						failed++
						mu.Unlock()
						continue
					}
					mu.Lock()
					probed++
					cache.Entries[j.path] = &cacheEntry{Size: j.info.Size(), ModTime: j.info.ModTime(), Probe: p}
					mu.Unlock()
				}
				results[i] = &Track{
					Path: j.path, RelPath: j.rel, Size: j.info.Size(), ModTime: j.info.ModTime(),
					Format: p.Format, Title: p.Title, Artist: p.Artist, Album: p.Album, Genre: p.Genre,
					Key: p.Key, Comment: p.Comment, Label: p.Label, Year: p.Year, BPM100: p.BPM100,
					Duration: p.Duration, Bitrate: p.Bitrate, SampleRate: p.SampleRate, SampleDepth: p.SampleDepth,
				}
			}
		}()
	}
	start := time.Now()
	lastLog := start
	for i := range jobs {
		select {
		case next <- i:
		case <-ctx.Done():
			close(next)
			wg.Wait()
			return nil, ctx.Err()
		}
		if time.Since(lastLog) > 5*time.Second {
			lastLog = time.Now()
			log.Info("scanning", "done", i, "of", len(jobs))
		}
	}
	close(next)
	wg.Wait()

	b := NewBuilder(root)
	for _, t := range results {
		if t != nil {
			b.Add(t)
		}
	}
	lib := b.Build()
	// Drop cache entries for files that no longer exist.
	live := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		live[j.path] = true
	}
	for p := range cache.Entries {
		if !live[p] && strings.HasPrefix(p, root+string(filepath.Separator)) {
			delete(cache.Entries, p)
		}
	}
	if err := saveCache(opt.CachePath, cache); err != nil {
		log.Warn("could not write metadata cache", "path", opt.CachePath, "err", err)
	}
	log.Info("library scanned", "root", root, "tracks", lib.TrackCount(), "probed", probed, "cached", cached, "skipped", failed, "took", time.Since(start).Round(time.Millisecond))
	return lib, nil
}

func loadCache(path string) *cacheFile {
	c := &cacheFile{Version: cacheVersion, Entries: map[string]*cacheEntry{}}
	if path == "" {
		return c
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	var f cacheFile
	if json.Unmarshal(b, &f) != nil || f.Version != cacheVersion || f.Entries == nil {
		return c
	}
	return &f
}

func saveCache(path string, c *cacheFile) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ErrUnsupported is returned for files whose audio the CDJ cannot play.
var ErrUnsupported = errors.New("unsupported audio format")
