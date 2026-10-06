// SPDX-License-Identifier: GPL-3.0-or-later

package dbserver

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/yniverz/slipmat/internal/library"
)

func testLib() *library.Library {
	b := library.NewBuilder("/music")
	add := func(rel, title, artist, album, key string, bpm uint32) {
		b.Add(&library.Track{Path: "/music/" + rel, RelPath: rel, Format: library.FormatMP3, Title: title, Artist: artist, Album: album, Key: key, BPM100: bpm, Duration: 217, ModTime: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)})
	}
	add("House/a.mp3", "Alpha", "Artist One", "Album X", "Bbm", 15000)
	add("House/b.mp3", "Beta", "Artist One", "Album X", "", 12800)
	add("House/Deep/c.mp3", "Gamma", "Artist Two", "Album Y", "Am", 12000)
	add("loose.mp3", "Delta", "", "", "", 0)
	return b.Build()
}

func newSession(lib *library.Library) *Session {
	return NewSession(func() *library.Library { return lib }, 17, slog.New(slog.DiscardHandler))
}

const dmstMain = 0x01010401 // device 1, menu 1, slot 4 (rekordbox), type 1
const dmstPopup = 0x01020401
const dmstData = 0x01080401

func req(tx uint32, typ uint16, args ...uint32) *Message {
	m := &Message{TxID: tx, Type: typ}
	for _, a := range args {
		m.Args = append(m.Args, Num(a))
	}
	return m
}

func TestSetupMatchesRekordbox7(t *testing.T) {
	s := newSession(testLib())
	out := s.Handle(req(SetupTxID, ReqSetup, 1, 0x14))
	// rekordbox 7's reply, byte for byte. [RB7]
	want, _ := hex.DecodeString(strings.ReplaceAll("11872349ae11fffffffe1000000f021400000002060611000000111100000014", " ", ""))
	if len(out) != 1 || !bytes.Equal(out[0].Encode(), want) {
		t.Fatalf("setup reply\n got % x\nwant % x", out[0].Encode(), want)
	}
}

func TestRootMenuItemsMatchRekordbox7(t *testing.T) {
	s := newSession(testLib())
	if out := s.Handle(req(975, ReqRootMenu, dmstMain, 0, 0x05cfffff)); out[0].String() != "success(4000) txid=975 args=[0x1000 0x4]" {
		t.Fatalf("root menu: %v", out[0])
	}
	out := s.Handle(req(976, ReqRenderMenu, dmstMain, 0, 4, 0, 4, 0xc, 1, 0))
	if len(out) != 6 || out[0].Type != RespMenuHeader || out[5].Type != RespMenuFooter {
		t.Fatalf("render: %v", out)
	}
	// Exactly as rekordbox 7 sent its ARTIST entry (decoded form). [RB7]
	want := `menu-item(4101) txid=976 args=[0x0 0x2 0x12 "\ufffaARTIST\ufffb" 0x2 "" 0x81 0x0 0x0 0x0 0x0 0x0 0x0 0x2 "" 0x0]`
	if got := out[1].String(); got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

func TestBrowseArtistAlbumTrack(t *testing.T) {
	lib := testLib()
	s := newSession(lib)
	if out := s.Handle(req(1, ReqArtistMenu, dmstMain, 0)); out[0].NumArg(1) != 2 {
		t.Fatalf("artists: %v", out)
	}
	items := s.Handle(req(2, ReqRenderMenu, dmstMain, 0, 64, 0, 2, 0xc, 1, 0))
	artistID := items[1].NumArg(1)
	if items[1].Args[3].Str != "Artist One" || items[1].NumArg(6) != ItemArtist {
		t.Fatalf("artist item: %v", items[1])
	}
	s.Handle(req(3, ReqAlbumsByArtist, dmstPopup, 0, artistID))
	albums := s.Handle(req(4, ReqRenderMenu, dmstPopup, 0, 64, 0, 1, 0xc, 1, 0))
	if len(albums) != 3 || albums[1].Args[3].Str != "Album X" {
		t.Fatalf("albums by artist: %v", albums)
	}
	albumID := albums[1].NumArg(1)
	s.Handle(req(5, ReqTracksByAlbum, dmstPopup, 0, albumID))
	tracks := s.Handle(req(6, ReqRenderMenu, dmstPopup, 0, 64, 0, 2, 0xc, 1, 0))
	if len(tracks) != 4 {
		t.Fatalf("tracks by album: %v", tracks)
	}
	row := tracks[1]
	// Track row layout as rekordbox 7: title, scoped flag, artwork = id,
	// 0x100, key column, BPM. [RB7]
	if row.Args[3].Str != "Alpha" || row.NumArg(6) != ItemTitle || row.NumArg(7) != FlagScoped || row.NumArg(8) != row.NumArg(1) ||
		row.NumArg(10) != 0x100 || row.NumArg(11) != 5 || row.Args[14].Str != "Bbm" || row.NumArg(15) != 15000 {
		t.Fatalf("track row: %v", row)
	}
}

func TestPipelinedMetadataAndArtwork(t *testing.T) {
	lib := testLib()
	s := newSession(lib)
	tr := lib.Tracks()[0]
	if out := s.Handle(req(988, ReqTrackMetadata, dmstPopup, tr.ID)); out[0].NumArg(1) != 16 {
		t.Fatalf("metadata: %v", out)
	}
	art := s.Handle(req(989, ReqArtwork, dmstData, tr.ID, 1))
	if got := art[0].String(); got != "artwork-data(4002) txid=989 args=[0x2003 0x32 0x0 blob[]]" {
		t.Fatalf("artwork not-found must match rekordbox 7: %s", got)
	}
	tag := s.Handle(&Message{TxID: 990, Type: ReqAnalysisTag, Args: []Arg{Num(dmstData), Num(tr.ID), Num(0x34565750), Num(0x00545845)}})
	if tag[0].Type != RespAnalysisTag || tag[0].NumArg(1) != StatusNotFound {
		t.Fatalf("analysis tag: %v", tag)
	}
	// The render for menu location 2 still returns the metadata.
	out := s.Handle(req(991, ReqRenderMenu, dmstPopup, 0, 16, 0, 16, 0xc, 1, 0))
	if len(out) != 18 {
		t.Fatalf("want 16 items + header/footer, got %d", len(out))
	}
	types := []uint32{}
	for _, m := range out[1:17] {
		types = append(types, m.NumArg(6))
	}
	want := []uint32{0x04, 0x07, 0x02, 0x0b, 0x0d, 0x0f, 0x0a, 0x13, 0x06, 0x2e, 0x23, 0x10, 0x11, 0x0e, 0x28, 0x29}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("metadata item types %x, want %x (rekordbox 7 order)", types, want)
		}
	}
	if out[4].NumArg(1) != 217 || out[10].Args[3].Str != "2026-10-06" {
		t.Fatalf("duration/date: %v %v", out[4], out[10])
	}
	if out := s.Handle(req(992, ReqTrackMetadata, dmstPopup, 12345)); out[0].NumArg(1) != 0xffffffff {
		t.Fatalf("unknown track must report 0xffffffff: %v", out)
	}
}

func TestPlaylistFolderTree(t *testing.T) {
	lib := testLib()
	s := newSession(lib)
	s.Handle(req(1, ReqPlaylist, dmstMain, 0, 0, 1))
	root := s.Handle(req(2, ReqRenderMenu, dmstMain, 0, 64, 0, 64, 0xc, 1, 0))
	// "▶ (tracks in the music folder)" playlist + "House" folder (has a sub-folder).
	if len(root) != 4 || root[1].NumArg(6) != ItemPlaylist || root[2].Args[3].Str != "House" || root[2].NumArg(6) != ItemFolder {
		t.Fatalf("root: %v", root)
	}
	houseID := root[2].NumArg(1)
	s.Handle(req(3, ReqPlaylist, dmstMain, 0, houseID, 1))
	house := s.Handle(req(4, ReqRenderMenu, dmstMain, 0, 64, 0, 64, 0xc, 1, 0))
	if len(house) != 4 || house[1].Args[3].Str != "▶ House" || house[2].Args[3].Str != "Deep" || house[2].NumArg(6) != ItemPlaylist {
		t.Fatalf("House: %v", house)
	}
	s.Handle(req(5, ReqPlaylist, dmstMain, 0, houseID, 0))
	tracks := s.Handle(req(6, ReqRenderMenu, dmstMain, 0, 64, 0, 64, 0xc, 1, 0))
	if len(tracks) != 4 || tracks[1].Args[3].Str != "Alpha" || tracks[2].NumArg(9) != 2 {
		t.Fatalf("House tracks: %v", tracks)
	}
}

func TestRenderPagingAndRobustness(t *testing.T) {
	s := newSession(testLib())
	s.Handle(req(1, ReqTrackMenu, dmstMain, 0))
	page := s.Handle(req(2, ReqRenderMenu, dmstMain, 2, 1, 0, 4, 0xc, 1, 0))
	if len(page) != 3 || page[1].Args[3].Str != "Delta" { // Alpha, Beta, Delta, Gamma
		t.Fatalf("page: %v", page)
	}
	for _, m := range []*Message{
		req(3, ReqRenderMenu, dmstMain, 99, 10),   // offset past end
		req(4, ReqRenderMenu, 0x01090401, 0, 10),  // nothing prepared at location 9
		req(5, 0x7777, dmstMain),                  // unknown request
		{TxID: 6, Type: ReqTrackMenu},             // no arguments
		req(7, ReqTracksByAlbum, dmstMain, 0, 42), // unknown album
	} {
		if out := s.Handle(m); len(out) == 0 {
			t.Fatalf("%v: no reply", m)
		}
	}
}

// TestServerLoopback runs discovery and a session over real TCP sockets.
func TestServerLoopback(t *testing.T) {
	srv := &Server{Lib: func() *library.Library { return testLib() }, Device: 17, Log: slog.New(slog.DiscardHandler)}
	if err := srv.Listen(netip.MustParseAddr("127.0.0.1")); err != nil {
		t.Skipf("cannot listen (is rekordbox running?): %v", err)
	}
	ctx := t.Context()
	go srv.Serve(ctx)

	c, err := net.Dial("tcp4", "127.0.0.1:12523")
	if err != nil {
		t.Fatal(err)
	}
	c.Write(discoveryQuery[:2]) // the CDJ-3000 splits it into 2 + 17 bytes [RB7]
	time.Sleep(10 * time.Millisecond)
	c.Write(discoveryQuery[2:])
	var port [2]byte
	if _, err := io.ReadFull(c, port[:]); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if p := binary.BigEndian.Uint16(port[:]); p != srv.Port() {
		t.Fatalf("discovery said %d, server is on %d", p, srv.Port())
	}

	db, err := net.Dial("tcp4", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), srv.Port()).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetDeadline(time.Now().Add(5 * time.Second))
	db.Write([]byte{0x11, 0, 0, 0, 1})
	rd := NewReader(db)
	if g, err := rd.ReadGreeting(); err != nil || g != 1 {
		t.Fatalf("greeting %d %v", g, err)
	}
	// Pipeline three requests in one write, like a CDJ.
	var batch []byte
	for _, m := range []*Message{req(SetupTxID, ReqSetup, 1, 0x14), req(1, ReqTrackMenu, dmstMain, 0), req(2, ReqRenderMenu, dmstMain, 0, 64, 0, 4, 0xc, 1, 0)} {
		batch = append(batch, m.Encode()...)
	}
	db.Write(batch)
	want := []uint16{ReqSetup, RespSuccess, RespMenuHeader, RespMenuItem, RespMenuItem, RespMenuItem, RespMenuItem, RespMenuFooter}
	for i, typ := range want {
		m, err := rd.ReadMessage()
		if err != nil || m.Type != typ {
			t.Fatalf("reply %d: %v %v (want %04x)", i, m, err, typ)
		}
	}
	// Garbage closes the connection but must not take the server down.
	db.Write([]byte{0x99, 0x99})
	if _, err := rd.ReadMessage(); err == nil {
		t.Fatal("expected the server to close the connection after garbage")
	}
	db2, err := net.Dial("tcp4", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), srv.Port()).String())
	if err != nil {
		t.Fatalf("server stopped accepting after a bad client: %v", err)
	}
	db2.Close()
}
