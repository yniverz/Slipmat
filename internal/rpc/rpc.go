// SPDX-License-Identifier: GPL-3.0-or-later

// Package rpc implements the ONC RPC (RFC 5531) and XDR (RFC 4506) pieces
// a rekordbox source needs: the portmapper (on rekordbox's non-standard UDP
// port 50111), the MOUNT v1 daemon and, later, NFS v2 (RFC 1094).
//
// Handlers are pure functions from a decoded call to reply bytes so they can
// be tested against captured rekordbox traffic (testdata/).
package rpc

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Program numbers.
const (
	ProgPortmap = 100000
	ProgNFS     = 100003
	ProgMount   = 100005
)

// Message types and reply/accept states (RFC 5531).
const (
	msgCall  = 0
	msgReply = 1

	replyAccepted = 0

	AcceptSuccess      = 0
	AcceptProgUnavail  = 1
	AcceptProgMismatch = 2
	AcceptProcUnavail  = 3
	AcceptGarbageArgs  = 4
	AcceptSystemErr    = 5
)

var ErrNotCall = errors.New("rpc: not a call")

// Call is a decoded RPC call header plus its raw arguments.
type Call struct {
	XID  uint32
	Prog uint32
	Vers uint32
	Proc uint32
	Args []byte // after credentials and verifier
}

func (c *Call) String() string {
	return fmt.Sprintf("%s v%d proc %d xid=%08x", ProgName(c.Prog), c.Vers, c.Proc, c.XID)
}

// ProgName names a program number.
func ProgName(p uint32) string {
	switch p {
	case ProgPortmap:
		return "portmap"
	case ProgNFS:
		return "nfs"
	case ProgMount:
		return "mount"
	}
	return fmt.Sprintf("prog%d", p)
}

// ParseCall decodes an RPC call message. Credentials (CDJs send AUTH_UNIX)
// and verifier are skipped.
func ParseCall(b []byte) (*Call, error) {
	r := NewReader(b)
	xid := r.Uint32()
	if r.Uint32() != msgCall {
		if r.Err() != nil {
			return nil, r.Err()
		}
		return nil, ErrNotCall
	}
	if v := r.Uint32(); v != 2 && r.Err() == nil {
		return nil, fmt.Errorf("rpc: unsupported RPC version %d", v)
	}
	c := &Call{XID: xid, Prog: r.Uint32(), Vers: r.Uint32(), Proc: r.Uint32()}
	for i := 0; i < 2; i++ { // credentials, verifier
		r.Uint32() // flavor
		r.Opaque(400)
	}
	if r.Err() != nil {
		return nil, fmt.Errorf("rpc: bad call header: %w", r.Err())
	}
	c.Args = r.Rest()
	return c, nil
}

// AcceptedReply builds a reply with an AUTH_NULL verifier, the given accept
// state and (for success) result bytes. This is the exact shape of
// rekordbox's replies. [RB7]
func AcceptedReply(xid uint32, state uint32, result []byte) []byte {
	w := &Writer{}
	w.Uint32(xid)
	w.Uint32(msgReply)
	w.Uint32(replyAccepted)
	w.Uint32(0) // verifier flavor AUTH_NULL
	w.Uint32(0) // verifier length
	w.Uint32(state)
	w.Bytes(result) // for AcceptProgMismatch: lowest and highest supported version
	return w.B
}

// ProgMismatch builds a PROG_MISMATCH reply advertising versions low..high.
func ProgMismatch(xid, low, high uint32) []byte {
	var w Writer
	w.Uint32(low)
	w.Uint32(high)
	return AcceptedReply(xid, AcceptProgMismatch, w.B)
}

// Reader decodes XDR. Errors are sticky: after the first short read every
// accessor returns zero and Err reports the problem.
type Reader struct {
	b   []byte
	off int
	err error
}

func NewReader(b []byte) *Reader { return &Reader{b: b} }

func (r *Reader) Err() error { return r.err }

func (r *Reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || r.off+n > len(r.b) {
		r.err = fmt.Errorf("xdr: short read (%d bytes at offset %d of %d)", n, r.off, len(r.b))
		return nil
	}
	p := r.b[r.off : r.off+n]
	r.off += n
	return p
}

func (r *Reader) Uint32() uint32 {
	p := r.take(4)
	if p == nil {
		return 0
	}
	return binary.BigEndian.Uint32(p)
}

// Opaque reads variable-length opaque data of at most max bytes.
func (r *Reader) Opaque(max int) []byte {
	n := int(r.Uint32())
	if r.err == nil && n > max {
		r.err = fmt.Errorf("xdr: opaque length %d exceeds %d", n, max)
		return nil
	}
	p := r.take(n)
	r.take((4 - n%4) % 4)
	return p
}

// Fixed reads n bytes of fixed-length opaque data.
func (r *Reader) Fixed(n int) []byte {
	p := r.take(n)
	r.take((4 - n%4) % 4)
	return p
}

// String reads an XDR string of at most max bytes.
func (r *Reader) String(max int) string { return string(r.Opaque(max)) }

// Rest returns the unread bytes.
func (r *Reader) Rest() []byte {
	if r.err != nil {
		return nil
	}
	return r.b[r.off:]
}

// Writer encodes XDR.
type Writer struct{ B []byte }

func (w *Writer) Uint32(v uint32) { w.B = binary.BigEndian.AppendUint32(w.B, v) }

func (w *Writer) Bytes(p []byte) { w.B = append(w.B, p...) }

// Opaque writes variable-length opaque data with padding.
func (w *Writer) Opaque(p []byte) {
	w.Uint32(uint32(len(p)))
	w.Fixed(p)
}

// Fixed writes fixed-length opaque data with padding.
func (w *Writer) Fixed(p []byte) {
	w.B = append(w.B, p...)
	w.B = append(w.B, make([]byte, (4-len(p)%4)%4)...)
}

func (w *Writer) String(s string) { w.Opaque([]byte(s)) }
