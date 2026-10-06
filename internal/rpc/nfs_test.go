// SPDX-License-Identifier: GPL-3.0-or-later

package rpc

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unicode/utf16"
)

// utf16le encodes a LOOKUP name the way a CDJ-3000 does. [RB7]
func utf16le(s string) []byte {
	var b []byte
	for _, c := range utf16.Encode([]rune(s)) {
		b = append(b, byte(c), byte(c>>8))
	}
	return b
}

// nfsCall builds a call with AUTH_UNIX credentials like the CDJ's.
func nfsCall(xid, proc uint32, args []byte) *Call {
	var w Writer
	w.Uint32(xid)
	w.Uint32(0)
	w.Uint32(2)
	w.Uint32(ProgNFS)
	w.Uint32(2)
	w.Uint32(proc)
	w.Uint32(1) // AUTH_UNIX
	w.Opaque(make([]byte, 20))
	w.Uint32(0)
	w.Uint32(0)
	w.Bytes(args)
	c, err := ParseCall(w.B)
	if err != nil {
		panic(err)
	}
	return c
}

// nfsReply returns (status, rest) of a successful RPC reply.
func nfsReply(t *testing.T, b []byte) (uint32, *Reader) {
	t.Helper()
	r := NewReader(b)
	for i := 0; i < 5; i++ {
		r.Uint32()
	}
	if st := r.Uint32(); st != AcceptSuccess {
		t.Fatalf("rpc accept state %d", st)
	}
	return r.Uint32(), r
}

func lookup(t *testing.T, n *NFS, dir []byte, name []byte) (uint32, []byte) {
	var w Writer
	w.Fixed(dir)
	w.Opaque(name)
	st, r := nfsReply(t, n.Handle(nfsCall(1, nfsLookup, w.B)))
	if st != NFSOK {
		return st, nil
	}
	return st, r.Fixed(FHSize)
}

func setupTree(t *testing.T) (*NFS, string) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "house 26"), 0o755)
	data := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64 KiB
	os.WriteFile(filepath.Join(root, "house 26", "Déjà Vu (128k).mp3"), data, 0o644)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644)
	os.Symlink(outside, filepath.Join(root, "escape"))
	n, err := NewNFS(root)
	if err != nil {
		t.Fatal(err)
	}
	return n, root
}

func TestNFSLookupAndRead(t *testing.T) {
	n, _ := setupTree(t)
	var root [FHSize]byte
	// Odd garbage after the name, as seen from the CDJ ("Users" + "r\0"). [RB7]
	name := append(utf16le("house 26"), 'r', 0)
	var w Writer
	w.Fixed(root[:])
	w.Uint32(uint32(len(utf16le("house 26"))))
	w.Bytes(name)
	st, r := nfsReply(t, n.Handle(nfsCall(1, nfsLookup, w.B)))
	if st != NFSOK {
		t.Fatalf("lookup dir: status %d", st)
	}
	dirFH := r.Fixed(FHSize)
	if typ := r.Uint32(); typ != 2 {
		t.Fatalf("dir type %d", typ)
	}
	st, fileFH := lookup(t, n, dirFH, utf16le("Déjà Vu (128k).mp3"))
	if st != NFSOK {
		t.Fatalf("lookup file: status %d", st)
	}
	for _, c := range []struct{ off, count uint32 }{{0, 0x8000}, {0x8000, 0x4000}, {65536 - 100, 0x4000}, {1 << 20, 0x4000}} {
		var w Writer
		w.Fixed(fileFH)
		w.Uint32(c.off)
		w.Uint32(c.count)
		w.Uint32(0)
		st, r := nfsReply(t, n.Handle(nfsCall(2, nfsRead, w.B)))
		if st != NFSOK {
			t.Fatalf("read %v: status %d", c, st)
		}
		for i := 0; i < 17; i++ {
			r.Uint32()
		}
		got := r.Opaque(MaxRead)
		want := min(int(c.count), max(0, 65536-int(c.off)))
		if len(got) != want {
			t.Fatalf("read %v: %d bytes, want %d", c, len(got), want)
		}
	}
}

func TestNFSRefusesEscapes(t *testing.T) {
	n, _ := setupTree(t)
	var root [FHSize]byte
	for _, name := range []string{"..", ".", "a/b", "escape", ""} {
		st, fh := lookup(t, n, root[:], utf16le(name))
		if name == "escape" && st == NFSOK {
			// The symlink itself resolves outside the root: must be refused.
			t.Fatalf("symlink out of the root was accepted (fh %x)", fh)
		}
		if name != "escape" && st == NFSOK {
			t.Fatalf("lookup %q accepted", name)
		}
	}
	bogus := bytes.Repeat([]byte{0xff}, FHSize)
	var w Writer
	w.Fixed(bogus)
	if st, _ := nfsReply(t, n.Handle(nfsCall(3, nfsGetattr, w.B))); st != NFSErrStale {
		t.Fatalf("bogus handle: status %d", st)
	}
	// Writes are refused.
	if st, _ := nfsReply(t, n.Handle(nfsCall(4, 8, root[:]))); st != NFSErrROFS {
		t.Fatalf("write: status %d", st)
	}
}

// TestLargeReplyOverUDP checks that a 32 KiB READ reply leaves a UDP
// socket with an enlarged send buffer (macOS defaults to 9 KiB).
func TestLargeReplyOverUDP(t *testing.T) {
	n, _ := setupTree(t)
	srvConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	srvConn.SetWriteBuffer(4 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&Server{Conn: srvConn, Programs: map[uint32]Handler{ProgNFS: n}, Log: slog.New(slog.DiscardHandler), Workers: 4}).Serve(ctx)

	cli, err := net.DialUDP("udp4", nil, srvConn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	cli.SetReadBuffer(1 << 20)
	var root [FHSize]byte
	_, dirFH := lookup(t, n, root[:], utf16le("house 26"))
	_, fileFH := lookup(t, n, dirFH, utf16le("Déjà Vu (128k).mp3"))
	var args Writer
	args.Fixed(fileFH)
	args.Uint32(0)
	args.Uint32(0x8000)
	args.Uint32(0)
	var w Writer
	w.Uint32(77)
	w.Uint32(0)
	w.Uint32(2)
	w.Uint32(ProgNFS)
	w.Uint32(2)
	w.Uint32(nfsRead)
	w.Uint32(0)
	w.Uint32(0)
	w.Uint32(0)
	w.Uint32(0)
	w.Bytes(args.B)
	cli.Write(w.B)
	cli.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 70000)
	k, err := cli.Read(buf)
	if err != nil {
		t.Fatalf("no reply to a 32 KiB read: %v", err)
	}
	if k < 0x8000 {
		t.Fatalf("reply only %d bytes", k)
	}
}
