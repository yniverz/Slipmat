// SPDX-License-Identifier: GPL-3.0-or-later

// Package previewcache keeps the waveform previews CDJ-3000s compute for
// tracks without rekordbox analysis. While such a track is loaded, the
// player uploads its 900-byte preview (dbserver request 0x2005) every
// ~250 ms as its analysis progresses; we keep the latest one per file and
// serve it for later 0x2004 requests. [hardware]
//
// Entries are tied to the audio file's size and modification time, so an
// edited file never gets a stale preview. Nothing is written outside the
// cache directory.
package previewcache

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Size is the length of a player-made preview: 400 (height, whiteness)
// pairs plus a 100-byte tiny preview. [DS]
const Size = 900

// Key identifies an audio file version.
type Key struct {
	TrackID uint32
	Size    int64
	ModTime time.Time
}

// Cache stores previews as small files in Dir.
type Cache struct {
	Dir string

	mu  sync.Mutex
	mem map[uint32][]byte // last write per track, to skip redundant writes
}

// New returns a cache in dir (created on first write).
func New(dir string) *Cache { return &Cache{Dir: dir, mem: map[uint32][]byte{}} }

func (c *Cache) path(id uint32) string {
	return filepath.Join(c.Dir, hex8(id)+".pv")
}

func hex8(v uint32) string {
	const h = "0123456789abcdef"
	b := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		b[i] = h[v&15]
		v >>= 4
	}
	return string(b)
}

// header: size (8 bytes), mtime unix nanos (8 bytes), then the preview.
const headerLen = 16

// Put stores a preview for k. Malformed previews are rejected.
func (c *Cache) Put(k Key, preview []byte) error {
	if len(preview) != Size {
		return errors.New("previewcache: preview must be 900 bytes")
	}
	c.mu.Lock()
	if old := c.mem[k.TrackID]; string(old) == string(preview) {
		c.mu.Unlock()
		return nil
	}
	c.mem[k.TrackID] = append([]byte(nil), preview...)
	c.mu.Unlock()
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return err
	}
	b := make([]byte, headerLen, headerLen+Size)
	binary.BigEndian.PutUint64(b[0:8], uint64(k.Size))
	binary.BigEndian.PutUint64(b[8:16], uint64(k.ModTime.UnixNano()))
	b = append(b, preview...)
	tmp := c.path(k.TrackID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.path(k.TrackID))
}

// Get returns the stored preview for k, or nil if there is none or the
// file has changed since it was stored.
func (c *Cache) Get(k Key) []byte {
	b, err := os.ReadFile(c.path(k.TrackID))
	if err != nil || len(b) != headerLen+Size {
		return nil
	}
	if int64(binary.BigEndian.Uint64(b[0:8])) != k.Size || int64(binary.BigEndian.Uint64(b[8:16])) != k.ModTime.UnixNano() {
		return nil
	}
	return b[headerLen:]
}
