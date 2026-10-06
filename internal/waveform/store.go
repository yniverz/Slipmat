// SPDX-License-Identifier: GPL-3.0-or-later

package waveform

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/yniverz/slipmat/internal/anlz"
)

// formatVersion changes whenever the calibration or file layout changes, so
// cached waveforms are regenerated.
const formatVersion = 2 // 2: envelope-based PWV7

// Job identifies one audio file version to analyse.
type Job struct {
	TrackID uint32
	Path    string
	Size    int64
	ModTime time.Time
}

// Store generates waveforms in the background and caches them on disk
// (only in Dir; nothing is written next to the music or into rekordbox's
// folders).
type Store struct {
	Dir     string
	Workers int
	Log     *slog.Logger

	mu      sync.Mutex
	jobs    map[uint32]Job
	queue   []uint32 // pending, in order
	urgent  []uint32 // requested by a player: processed first
	mem     map[uint32]*anlz.Analysis
	memList []uint32
	wake    chan struct{}
	done    int
	busy    int
}

// NewStore creates a store.
func NewStore(dir string, workers int, log *slog.Logger) *Store {
	if workers < 1 {
		workers = 1
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Store{Dir: dir, Workers: workers, Log: log, jobs: map[uint32]Job{}, mem: map[uint32]*anlz.Analysis{}, wake: make(chan struct{}, 1)}
}

// Add registers tracks; those without a valid cached waveform are queued.
func (s *Store) Add(jobs []Job) (cached, queued int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range jobs {
		s.jobs[j.TrackID] = j
		if s.validOnDisk(j) {
			cached++
			continue
		}
		s.queue = append(s.queue, j.TrackID)
		queued++
	}
	s.signal()
	return
}

func (s *Store) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Get returns the waveforms for a track if they are ready, without
// blocking. A miss moves the track to the front of the queue.
func (s *Store) Get(id uint32) *anlz.Analysis {
	s.mu.Lock()
	if a := s.mem[id]; a != nil {
		s.mu.Unlock()
		return a
	}
	j, ok := s.jobs[id]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	if r, err := s.load(j); err == nil {
		a := r.Analysis()
		s.remember(id, a)
		return a
	}
	s.mu.Lock()
	s.urgent = append(s.urgent, id)
	s.mu.Unlock()
	s.signal()
	return nil
}

func (s *Store) remember(id uint32, a *anlz.Analysis) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.mem[id]; !ok {
		s.memList = append(s.memList, id)
	}
	s.mem[id] = a
	for len(s.memList) > 32 {
		delete(s.mem, s.memList[0])
		s.memList = s.memList[1:]
	}
}

// Run processes the queue until ctx is cancelled.
func (s *Store) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for w := 0; w < s.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, ok := s.next()
				if !ok {
					select {
					case <-ctx.Done():
						return
					case <-s.wake:
						continue
					case <-time.After(time.Second):
						continue
					}
				}
				s.process(ctx, j)
				if ctx.Err() != nil {
					return
				}
			}
		}()
	}
	wg.Wait()
}

func (s *Store) next() (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.urgent) > 0 || len(s.queue) > 0 {
		var id uint32
		if len(s.urgent) > 0 {
			id, s.urgent = s.urgent[0], s.urgent[1:]
		} else {
			id, s.queue = s.queue[0], s.queue[1:]
		}
		j, ok := s.jobs[id]
		if !ok || s.validOnDisk(j) {
			continue
		}
		return j, true
	}
	return Job{}, false
}

func (s *Store) process(ctx context.Context, j Job) {
	s.mu.Lock()
	s.busy++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.busy--
		s.mu.Unlock()
	}()
	start := time.Now()
	samples, err := Decode(ctx, j.Path)
	if err != nil {
		if ctx.Err() == nil {
			s.Log.Warn("could not decode track for waveforms", "path", j.Path, "err", err)
		}
		return
	}
	r := Generate(samples)
	if err := s.save(j, r); err != nil {
		s.Log.Warn("could not cache waveforms", "err", err)
	}
	s.remember(j.TrackID, r.Analysis())
	s.mu.Lock()
	s.done++
	left := len(s.queue) + len(s.urgent)
	last := left == 0 && s.busy == 1 // the last worker still running
	done := s.done
	s.mu.Unlock()
	s.Log.Debug("generated waveforms", "path", filepath.Base(j.Path), "took", time.Since(start).Round(time.Millisecond), "left", left)
	if last {
		s.Log.Info("waveform generation finished", "generated", done)
	}
}

// Busy returns how many tracks are being generated right now.
func (s *Store) Busy() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy
}

// Ready reports whether a valid cached waveform exists for the track.
func (s *Store) Ready(id uint32) bool {
	s.mu.Lock()
	j, ok := s.jobs[id]
	s.mu.Unlock()
	return ok && s.validOnDisk(j)
}

// Pending returns how many tracks are still waiting.
func (s *Store) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue) + len(s.urgent)
}

func (s *Store) path(id uint32) string {
	return filepath.Join(s.Dir, fmt.Sprintf("%08x.wf", id))
}

// File layout: "SLWF", version u32, size i64, mtime ns i64, then the seven
// bodies, each prefixed by a u32 length.
func (s *Store) save(j Job, r *Result) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	var b bytes.Buffer
	b.WriteString("SLWF")
	binary.Write(&b, binary.BigEndian, uint32(formatVersion))
	binary.Write(&b, binary.BigEndian, j.Size)
	binary.Write(&b, binary.BigEndian, j.ModTime.UnixNano())
	for _, body := range [][]byte{r.PWAV, r.PWV2, r.PWV3, r.PWV4, r.PWV5, r.PWV6, r.PWV7} {
		binary.Write(&b, binary.BigEndian, uint32(len(body)))
		b.Write(body)
	}
	tmp := s.path(j.TrackID) + ".tmp"
	if err := os.WriteFile(tmp, b.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(j.TrackID))
}

var errStale = errors.New("waveform cache entry is stale")

func (s *Store) readHeader(j Job) (*bytes.Reader, error) {
	raw, err := os.ReadFile(s.path(j.TrackID))
	if err != nil {
		return nil, err
	}
	rd := bytes.NewReader(raw)
	var magic [4]byte
	var ver uint32
	var size, mtime int64
	io.ReadFull(rd, magic[:])
	binary.Read(rd, binary.BigEndian, &ver)
	binary.Read(rd, binary.BigEndian, &size)
	if err := binary.Read(rd, binary.BigEndian, &mtime); err != nil {
		return nil, err
	}
	if string(magic[:]) != "SLWF" || ver != formatVersion || size != j.Size || mtime != j.ModTime.UnixNano() {
		return nil, errStale
	}
	return rd, nil
}

func (s *Store) validOnDisk(j Job) bool {
	_, err := s.readHeader(j)
	return err == nil
}

func (s *Store) load(j Job) (*Result, error) {
	rd, err := s.readHeader(j)
	if err != nil {
		return nil, err
	}
	var bodies [7][]byte
	for i := range bodies {
		var n uint32
		if err := binary.Read(rd, binary.BigEndian, &n); err != nil || n > 64<<20 {
			return nil, errStale
		}
		bodies[i] = make([]byte, n)
		if _, err := io.ReadFull(rd, bodies[i]); err != nil {
			return nil, errStale
		}
	}
	r := &Result{PWAV: bodies[0], PWV2: bodies[1], PWV3: bodies[2], PWV4: bodies[3], PWV5: bodies[4], PWV6: bodies[5], PWV7: bodies[6]}
	if len(r.PWV5) != 2*len(r.PWV3) || len(r.PWV7) != 3*len(r.PWV3) || len(r.PWV4) != 6*1200 || len(r.PWV6) != 3*1200 || len(r.PWAV) != 400 || len(r.PWV2) != 100 {
		return nil, errStale
	}
	return r, nil
}
