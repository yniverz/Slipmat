// SPDX-License-Identifier: GPL-3.0-or-later

package rpc

import (
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unicode/utf16"
)

// NFS v2 status codes (RFC 1094).
const (
	NFSOK          = 0
	NFSErrPerm     = 1
	NFSErrNoEnt    = 2
	NFSErrIO       = 5
	NFSErrAccess   = 13
	NFSErrNotDir   = 20
	NFSErrIsDir    = 21
	NFSErrROFS     = 30
	NFSErrNameLong = 63
	NFSErrStale    = 70
)

// NFS v2 procedures.
const (
	nfsNull    = 0
	nfsGetattr = 1
	nfsLookup  = 4
	nfsRead    = 6
	nfsReaddir = 16
	nfsStatfs  = 17
)

// MaxRead caps READ sizes. CDJ-3000s ask for 16 or 32 KiB. [RB7]
const MaxRead = 64 << 10

// NFS serves one directory tree read-only over NFS v2, the way rekordbox
// does for players loading tracks. [RB7]
//
// Players walk the absolute path from the track-info response one LOOKUP
// at a time, starting from the all-zero root handle MOUNT returned. Names
// arrive as UTF-16LE. Nothing outside Root is ever reachable: names are
// single path components, ".." is refused, and symlinks leaving the root
// are refused.
type NFS struct {
	Root string

	mu     sync.Mutex
	byPath map[string]uint64 // relative slash path -> handle id
	byID   map[uint64]string
	nextID uint64
}

// NewNFS serves root.
func NewNFS(root string) (*NFS, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return &NFS{Root: abs, byPath: map[string]uint64{".": 0}, byID: map[uint64]string{0: "."}, nextID: 1}, nil
}

// handle layout: 32 bytes, id in the first 8 (big-endian), rest zero, so the
// root (id 0) is the all-zero handle rekordbox uses. [RB7]
func encodeFH(id uint64) [FHSize]byte {
	var fh [FHSize]byte
	binary.BigEndian.PutUint64(fh[:8], id)
	return fh
}

func (n *NFS) handleFor(rel string) uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	if id, ok := n.byPath[rel]; ok {
		return id
	}
	id := n.nextID
	n.nextID++
	n.byPath[rel], n.byID[id] = id, rel
	return id
}

func (n *NFS) pathFor(fh []byte) (string, uint64, bool) {
	if len(fh) != FHSize {
		return "", 0, false
	}
	for _, b := range fh[8:] {
		if b != 0 {
			return "", 0, false
		}
	}
	id := binary.BigEndian.Uint64(fh[:8])
	n.mu.Lock()
	defer n.mu.Unlock()
	rel, ok := n.byID[id]
	return rel, id, ok
}

// Handle implements Handler.
func (n *NFS) Handle(c *Call) []byte {
	if c.Vers != 2 {
		return ProgMismatch(c.XID, 2, 2)
	}
	r := NewReader(c.Args)
	var w Writer
	switch c.Proc {
	case nfsNull:
		return AcceptedReply(c.XID, AcceptSuccess, nil)
	case nfsGetattr:
		fh := r.Fixed(FHSize)
		if r.Err() != nil {
			return AcceptedReply(c.XID, AcceptGarbageArgs, nil)
		}
		rel, id, ok := n.pathFor(fh)
		if !ok {
			w.Uint32(NFSErrStale)
			break
		}
		st, err := n.stat(rel)
		if err != nil {
			w.Uint32(errStatus(err))
			break
		}
		w.Uint32(NFSOK)
		writeFattr(&w, st, id)
	case nfsLookup:
		fh := r.Fixed(FHSize)
		raw := r.Opaque(1024)
		if r.Err() != nil {
			return AcceptedReply(c.XID, AcceptGarbageArgs, nil)
		}
		dir, _, ok := n.pathFor(fh)
		if !ok {
			w.Uint32(NFSErrStale)
			break
		}
		name, ok := decodeName(raw)
		if !ok {
			w.Uint32(NFSErrNoEnt)
			break
		}
		rel := path.Join(dir, name)
		st, err := n.stat(rel)
		if err != nil {
			w.Uint32(errStatus(err))
			break
		}
		id := n.handleFor(rel)
		w.Uint32(NFSOK)
		fhOut := encodeFH(id)
		w.Fixed(fhOut[:])
		writeFattr(&w, st, id)
	case nfsRead:
		fh := r.Fixed(FHSize)
		off, count := r.Uint32(), r.Uint32()
		r.Uint32() // totalcount (unused)
		if r.Err() != nil {
			return AcceptedReply(c.XID, AcceptGarbageArgs, nil)
		}
		rel, id, ok := n.pathFor(fh)
		if !ok {
			w.Uint32(NFSErrStale)
			break
		}
		st, err := n.stat(rel)
		if err != nil {
			w.Uint32(errStatus(err))
			break
		}
		if st.IsDir() {
			w.Uint32(NFSErrIsDir)
			break
		}
		if count > MaxRead {
			count = MaxRead
		}
		data, err := n.read(rel, int64(off), int(count))
		if err != nil {
			w.Uint32(NFSErrIO)
			break
		}
		w.Uint32(NFSOK)
		writeFattr(&w, st, id)
		w.Opaque(data)
	case nfsStatfs:
		w.Uint32(NFSOK)
		w.Uint32(8192)    // tsize
		w.Uint32(4096)    // bsize
		w.Uint32(1 << 20) // blocks
		w.Uint32(0)       // bfree: read-only
		w.Uint32(0)       // bavail
	case nfsReaddir:
		// Players load by path and never list directories. [RB7]
		w.Uint32(NFSErrAccess)
	case 2, 5, 8, 9, 10, 11, 12, 13, 14, 15: // all modifying procedures
		w.Uint32(NFSErrROFS)
	default:
		return AcceptedReply(c.XID, AcceptProcUnavail, nil)
	}
	return AcceptedReply(c.XID, AcceptSuccess, w.B)
}

// decodeName turns a LOOKUP name into a single safe path component.
// CDJs send UTF-16LE [RB7]; plain 8-bit names are accepted too.
func decodeName(raw []byte) (string, bool) {
	var name string
	if len(raw) >= 2 && len(raw)%2 == 0 && looksUTF16LE(raw) {
		u := make([]uint16, 0, len(raw)/2)
		for i := 0; i+1 < len(raw); i += 2 {
			c := binary.LittleEndian.Uint16(raw[i:])
			if c == 0 {
				break
			}
			u = append(u, c)
		}
		name = string(utf16.Decode(u))
	} else {
		name = strings.TrimRight(string(raw), "\x00")
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return "", false
	}
	return name, true
}

// looksUTF16LE: ASCII-range UTF-16LE text has a zero in every odd byte.
func looksUTF16LE(b []byte) bool {
	zeros := 0
	for i := 1; i < len(b); i += 2 {
		if b[i] == 0 {
			zeros++
		}
	}
	return zeros*2 >= len(b)/2
}

var errEscape = errors.New("path escapes the export root")

// stat resolves rel under Root, refusing anything that leaves it.
func (n *NFS) stat(rel string) (fs.FileInfo, error) {
	full := filepath.Join(n.Root, filepath.FromSlash(rel))
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		return nil, err
	}
	if r, err := filepath.Rel(n.Root, real); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return nil, errEscape
	}
	return os.Stat(real)
}

// read opens the file for each request: cheap, and no shared state that
// could be closed under a concurrent reader.
func (n *NFS) read(rel string, off int64, count int) ([]byte, error) {
	f, err := os.Open(filepath.Join(n.Root, filepath.FromSlash(rel)))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, count)
	k, err := f.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:k], nil
}

func errStatus(err error) uint32 {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return NFSErrNoEnt
	case errors.Is(err, fs.ErrPermission), errors.Is(err, errEscape):
		return NFSErrAccess
	case errors.Is(err, syscall.ENOTDIR):
		return NFSErrNotDir
	case errors.Is(err, syscall.ENAMETOOLONG):
		return NFSErrNameLong
	}
	return NFSErrIO
}

// writeFattr writes NFS v2 file attributes in the shape rekordbox uses:
// regular files 0644, directories 0755, block size 4096, fsid 2. [RB7]
func writeFattr(w *Writer, st fs.FileInfo, id uint64) {
	typ, mode, nlink := uint32(1), uint32(0o100644), uint32(1)
	if st.IsDir() {
		typ, mode, nlink = 2, 0o40755, 2
	}
	size := uint32(min(st.Size(), 0xffffffff))
	w.Uint32(typ)
	w.Uint32(mode)
	w.Uint32(nlink)
	w.Uint32(0)    // uid
	w.Uint32(0)    // gid
	w.Uint32(size) // size
	w.Uint32(4096) // blocksize
	w.Uint32(0)    // rdev
	w.Uint32((size + 511) / 512)
	w.Uint32(2)          // fsid
	w.Uint32(uint32(id)) // fileid
	mt := st.ModTime()
	for i := 0; i < 3; i++ { // atime, mtime, ctime
		w.Uint32(uint32(mt.Unix()))
		w.Uint32(uint32(mt.Nanosecond() / 1000))
	}
}
