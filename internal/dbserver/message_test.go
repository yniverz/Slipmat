// SPDX-License-Identifier: GPL-3.0-or-later

package dbserver

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	msgs := []*Message{
		{TxID: SetupTxID, Type: ReqSetup, Args: []Arg{Num(1)}},
		{TxID: 7, Type: RespMenuItem, Args: []Arg{Num(0), Num(0x23), Num(10), Str("Ünïcödé ♫ Title"), Num(2), Str(""), Num(4), Num(0), Num(0), Num(0), Num(0), Num(0)}},
		{TxID: 9, Type: RespBeatGrid, Args: []Arg{Num(ReqBeatGrid), Num(0), Num(4), Blob([]byte{1, 2, 3, 4}), Num(0)}},
		{TxID: 10, Type: RespMenuItem, Args: make16()},
	}
	var stream []byte
	for _, m := range msgs {
		stream = append(stream, m.Encode()...)
	}
	rd := NewReader(bytes.NewReader(stream))
	for i, want := range msgs {
		got, err := rd.ReadMessage()
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if got.String() != want.String() {
			t.Fatalf("message %d:\n got %s\nwant %s", i, got, want)
		}
	}
	if _, err := rd.ReadMessage(); err != io.EOF {
		t.Fatalf("want EOF at end, got %v", err)
	}
}

func make16() []Arg {
	a := make([]Arg, 16)
	for i := range a {
		a[i] = Num(uint32(i))
	}
	a[3], a[5], a[14] = Str("a"), Str("b"), Str("c")
	return a
}

func TestEmptyBlobOmitted(t *testing.T) {
	m := &Message{TxID: 1, Type: RespArtwork, Args: []Arg{Num(ReqArtwork), Num(0), Num(0), Blob(nil)}}
	enc := m.Encode()
	if bytes.Contains(enc, []byte{fieldBlob, 0, 0, 0, 0}) {
		t.Fatalf("empty blob after a zero length must be omitted: % x", enc)
	}
	got, n, err := Parse(enc)
	if err != nil || n != len(enc) || len(got.Args) != 4 || got.Args[3].Kind != KindBlob || len(got.Args[3].Blob) != 0 {
		t.Fatalf("decode: %v %d %v", got, n, err)
	}
}

func TestParseIncremental(t *testing.T) {
	enc := (&Message{TxID: 3, Type: ReqRenderMenu, Args: []Arg{Num(0x01010401), Num(0), Num(64), Num(0), Num(64), Num(0)}}).Encode()
	for n := 0; n < len(enc); n++ {
		_, _, err := Parse(enc[:n])
		if err == nil || !(errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)) {
			t.Fatalf("prefix %d: want EOF-ish error, got %v", n, err)
		}
	}
	two := append(append([]byte{}, enc...), enc...)
	_, n, err := Parse(two)
	if err != nil || n != len(enc) {
		t.Fatalf("consumed %d of %d: %v", n, len(enc), err)
	}
}

func TestRejectsGarbage(t *testing.T) {
	for _, b := range [][]byte{
		{0x11, 0, 0, 0, 1, 0x11}, // wrong magic
		{0x99},                   // unknown field type
		append((&Message{Type: 1}).Encode()[:5], 0x14), // truncated
	} {
		if _, _, err := Parse(b); err == nil {
			t.Errorf("% x parsed", b)
		}
	}
	// Huge declared lengths must not allocate.
	huge := []byte{0x11, 0x87, 0x23, 0x49, 0xae, 0x11, 0, 0, 0, 1, 0x10, 0, 1, 0x0f, 1, 0x14, 0xff, 0xff, 0xff, 0xff}
	if _, _, err := Parse(huge); err == nil {
		t.Error("huge blob accepted")
	}
}

func FuzzParse(f *testing.F) {
	f.Add((&Message{TxID: 1, Type: ReqRootMenu, Args: []Arg{Num(0x01010401), Num(0)}}).Encode())
	f.Add((&Message{TxID: 2, Type: RespMenuItem, Args: make16()}).Encode())
	f.Fuzz(func(t *testing.T, b []byte) {
		m, n, err := Parse(b)
		if err == nil && (n <= 0 || n > len(b) || m == nil) {
			t.Fatalf("bad consumption %d of %d", n, len(b))
		}
	})
}
