// SPDX-License-Identifier: GPL-3.0-or-later

package anlz

import (
	"bytes"
	"fmt"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// section builds a tagged section with a header of hdrLen bytes; extra
// header fields go after the 12-byte tag/lengths prefix.
func section(tag string, hdrLen int, hdrFields []byte, body []byte) []byte {
	b := make([]byte, hdrLen)
	copy(b, tag)
	binary.BigEndian.PutUint32(b[4:], uint32(hdrLen))
	binary.BigEndian.PutUint32(b[8:], uint32(hdrLen+len(body)))
	copy(b[12:], hdrFields)
	return append(b, body...)
}

func file(sections ...[]byte) []byte {
	total := 28
	for _, s := range sections {
		total += len(s)
	}
	b := make([]byte, 28)
	copy(b, "PMAI")
	binary.BigEndian.PutUint32(b[4:], 28)
	binary.BigEndian.PutUint32(b[8:], uint32(total))
	for _, s := range sections {
		b = append(b, s...)
	}
	return b
}

func ppth(p string) []byte {
	var body []byte
	for _, c := range utf16.Encode([]rune(p)) {
		body = binary.BigEndian.AppendUint16(body, c)
	}
	body = append(body, 0, 0)
	return section("PPTH", 16, binary.BigEndian.AppendUint32(nil, uint32(len(body))), body)
}

func pqtz(beats ...[3]uint32) []byte {
	var body []byte
	for _, b := range beats {
		body = binary.BigEndian.AppendUint16(body, uint16(b[0]))
		body = binary.BigEndian.AppendUint16(body, uint16(b[1]))
		body = binary.BigEndian.AppendUint32(body, b[2])
	}
	hdr := binary.BigEndian.AppendUint32(nil, 0)
	hdr = binary.BigEndian.AppendUint32(hdr, 0x00080000)
	hdr = binary.BigEndian.AppendUint32(hdr, uint32(len(beats)))
	return section("PQTZ", 24, hdr, body)
}

func TestParseAndConvert(t *testing.T) {
	pwav := bytes.Repeat([]byte{0x93}, 400) // height 0x13, whiteness 4
	b := file(ppth("?/Déjà Vu.mp3"), pqtz([3]uint32{1, 12800, 120}, [3]uint32{2, 12800, 589}), section("PWAV", 20, nil, pwav), section("PWV2", 20, nil, bytes.Repeat([]byte{7}, 100)))
	f, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if f.TrackPath() != "?/Déjà Vu.mp3" {
		t.Fatalf("PPTH %q", f.TrackPath())
	}
	q, _ := f.Section("PQTZ")
	grid, ok := BeatGrid(q)
	if !ok || len(grid) != 20+32 {
		t.Fatalf("grid %d bytes", len(grid))
	}
	// Header and first beat exactly as rekordbox 7 encodes them. [RB7]
	wantHead := "0000080002000000200000000100000001000000" + "0100" + "0032" + "78000000" + "ffffffffffffffff"
	if got := bytes.Clone(grid[:36]); hexs(got) != wantHead {
		t.Fatalf("grid head\n got %s\nwant %s", hexs(got), wantHead)
	}
	if FirstBPM(q) != 12800 {
		t.Fatal("bpm")
	}
	w, _ := f.Section("PWAV")
	w2, _ := f.Section("PWV2")
	p := Preview(w, w2, true)
	if len(p) != 904 || p[0] != 0x13 || p[1] != 4 || p[800] != 7 {
		t.Fatalf("preview: len %d % x", len(p), p[:4])
	}
	tb := TagBlob(w)
	if binary.LittleEndian.Uint32(tb) != 420 || !bytes.Equal(tb[4:], w.Raw) {
		t.Fatal("tag blob")
	}
	odd := Section{Tag: "PWV7", Header: 12, Raw: make([]byte, 13)}
	if tb := TagBlob(odd); len(tb) != 4+16 || binary.LittleEndian.Uint32(tb) != 16 {
		t.Fatalf("tag blob padding: % x", tb)
	}
}

func hexs(b []byte) string {
	const h = "0123456789abcdef"
	var s strings.Builder
	for _, c := range b {
		s.WriteByte(h[c>>4])
		s.WriteByte(h[c&15])
	}
	return s.String()
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, b := range [][]byte{nil, []byte("PMAI"), []byte("XXXX00000000"), file(section("PQTZ", 24, nil, nil))[:40]} {
		f, err := Parse(b)
		if err == nil && f == nil {
			t.Errorf("nil file without error for % x", b)
		}
	}
	// Section claiming to be longer than the file: ignored, no panic.
	bad := file(section("PWAV", 20, nil, make([]byte, 10)))
	binary.BigEndian.PutUint32(bad[28+8:], 9999)
	if f, err := Parse(bad); err != nil || len(f.Sections) != 0 {
		t.Fatalf("bad section: %v %v", f, err)
	}
	if _, ok := BeatGrid(Section{Tag: "PQTZ", Header: 12, Raw: make([]byte, 12)}); ok {
		t.Fatal("short PQTZ accepted")
	}
}

var setCounter int

func writeSet(t *testing.T, dir, trackPath string, mod time.Time) string {
	setCounter++
	d := filepath.Join(dir, "PIONEER", "USBANLZ", "P001", fmt.Sprintf("%08d", setCounter))
	os.MkdirAll(d, 0o755)
	p := filepath.Join(d, "ANLZ0000.DAT")
	os.WriteFile(p, file(ppth(trackPath), pqtz([3]uint32{1, 12000, 0})), 0o644)
	os.WriteFile(filepath.Join(d, "ANLZ0000.EXT"), file(ppth(trackPath)), 0o644)
	os.Chtimes(p, mod, mod)
	return p
}

func TestIndexMatching(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir()) // the index reports resolved paths
	now := time.Now()
	tracks := []TrackRef{
		{1, "house 26/Déjà Vu.mp3"},
		{2, "a/Intro.mp3"},
		{3, "b/Intro.mp3"},
		{4, "c/Unique.mp3"},
	}
	// rekordbox app folder style ("?/name"), NFD on disk vs NFC in PPTH.
	writeSet(t, dir, "?/Déjà Vu.mp3", now)
	// Ambiguous name without folder information: must be skipped.
	writeSet(t, dir, "?/Intro.mp3", now)
	// USB export with a full path identifies b/Intro.mp3.
	usb := writeSet(t, dir, "/Contents/b/Intro.mp3", now)
	// Two analyses of the same track: the newer wins.
	writeSet(t, dir, "?/Unique.mp3", now.Add(-time.Hour))
	newer := writeSet(t, dir, "/Contents/Artist/c/Unique.mp3", now)

	ix, st := BuildIndex([]string{dir}, tracks, nil)
	if _, ok := ix.Set(1); !ok {
		t.Error("NFC/NFD name not matched")
	}
	if _, ok := ix.Set(2); ok {
		t.Error("ambiguous ?/Intro.mp3 matched a/Intro.mp3")
	}
	if s, ok := ix.Set(3); !ok || s.DAT != usb {
		t.Errorf("USB path did not select b/Intro.mp3: %+v", s)
	}
	if s, ok := ix.Set(4); !ok || s.DAT != newer || s.EXT == "" {
		t.Errorf("newer analysis not chosen: %+v", s)
	}
	if st.Ambiguous != 1 || st.Files != 5 {
		t.Errorf("stats %+v", st)
	}
	a := ix.Load(1)
	if _, ok := a.Section("DAT", "PQTZ"); !ok || ix.Load(99) != nil {
		t.Error("load")
	}
}

// TestAgainstRekordbox7 compares our conversions with rekordbox 7's
// actual dbserver replies for one track. Local data only (not committed):
// SLIPMAT_RB7_ANLZ = the track's ANLZ0000 path without extension,
// SLIPMAT_RB7_BLOBS = directory of reply blobs named "<txid>-<type>-<arg>.bin".
func TestAgainstRekordbox7(t *testing.T) {
	base, blobs := os.Getenv("SLIPMAT_RB7_ANLZ"), os.Getenv("SLIPMAT_RB7_BLOBS")
	if base == "" || blobs == "" {
		t.Skip("SLIPMAT_RB7_ANLZ / SLIPMAT_RB7_BLOBS not set")
	}
	load := func(ext string) *File {
		f, err := ReadFile(base + "." + ext)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	a := &Analysis{DAT: load("DAT"), EXT: load("EXT"), EX2: load("2EX")}
	blob := func(tx string) []byte {
		m, _ := filepath.Glob(filepath.Join(blobs, tx+"-*"))
		if len(m) != 1 {
			t.Fatalf("blob %s missing", tx)
		}
		b, _ := os.ReadFile(m[0])
		return b
	}
	sec := func(kind, tag string) Section {
		s, ok := a.Section(kind, tag)
		if !ok {
			t.Fatalf("%s %s missing", kind, tag)
		}
		return s
	}
	grid, _ := BeatGrid(sec("DAT", "PQTZ"))
	detail, _ := Detail(sec("EXT", "PWV3"))
	pwv2 := sec("DAT", "PWV2")
	for name, c := range map[string]struct {
		got  []byte
		want []byte
	}{
		"beat grid": {grid, blob("1188")},
		"PQT2":      {TagBlob(sec("EXT", "PQT2")), blob("1189")},
		"PVBR":      {SeekIndex(sec("DAT", "PVBR")), blob("1190")},
		"PWV6":      {TagBlob(sec("2EX", "PWV6")), blob("1191")},
		"preview":   {Preview(sec("DAT", "PWAV"), pwv2, true)[:900], blob("1192")[:900]},
		"PWV7":      {TagBlob(sec("2EX", "PWV7")), blob("1193")},
		"PWV5":      {TagBlob(sec("EXT", "PWV5")), blob("1194")},
		"detail":    {detail, blob("1195")},
		"PSSI":      {TagBlob(sec("EXT", "PSSI")), blob("1196")},
	} {
		if !bytes.Equal(c.got, c.want) {
			t.Errorf("%s differs from rekordbox 7 (%d vs %d bytes)", name, len(c.got), len(c.want))
		}
	}
}
