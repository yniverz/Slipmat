// SPDX-License-Identifier: GPL-3.0-or-later

// Package dbserver implements the Pro DJ Link "remote database" protocol a
// rekordbox source answers on TCP: port discovery on 12523, then menus,
// track metadata and analysis data on a dynamic port.
//
// Wire format [DS]: a stream of type-tagged fields
//
//	0f n1 | 10 n2 | 11 n4      numbers (1, 2, 4 bytes, big-endian)
//	14 len4 bytes              binary blob
//	26 len4 utf16be            string; len counts UTF-16 units incl. trailing NUL
//
// A message is: number4 magic 0x872349ae, number4 TxID, number2 type,
// number1 argc, blob of argument tags (one per argument [RB7]; older players
// padded it to 12 bytes [DS]), then argc fields. rekordbox 7 menu items
// have 16 arguments.
// Argument tags: 02 string, 03 blob, 06 number.
package dbserver

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
)

// Magic starts every message.
const Magic = 0x872349ae

// SetupTxID is the transaction ID of the context-setup message.
const SetupTxID = 0xfffffffe

// Field type bytes.
const (
	fieldNum1   = 0x0f
	fieldNum2   = 0x10
	fieldNum4   = 0x11
	fieldBlob   = 0x14
	fieldString = 0x26
)

// Argument tags in the message header.
const (
	TagString = 0x02
	TagBlob   = 0x03
	TagNumber = 0x06
)

// Limits protecting us from malformed or hostile input.
const (
	maxArgs      = 32
	maxBlob      = 16 << 20 // waveforms and artwork are well below this
	maxStringLen = 1 << 16  // UTF-16 units
)

// ArgKind distinguishes argument types.
type ArgKind uint8

const (
	KindNumber ArgKind = iota
	KindString
	KindBlob
)

// Arg is one message argument.
type Arg struct {
	Kind ArgKind
	Num  uint32
	Str  string
	Blob []byte
}

// Num returns a number argument.
func Num(v uint32) Arg { return Arg{Kind: KindNumber, Num: v} }

// Str returns a string argument.
func Str(s string) Arg { return Arg{Kind: KindString, Str: s} }

// Blob returns a blob argument.
func Blob(b []byte) Arg { return Arg{Kind: KindBlob, Blob: b} }

func (a Arg) String() string {
	switch a.Kind {
	case KindString:
		return fmt.Sprintf("%q", a.Str)
	case KindBlob:
		if len(a.Blob) <= 16 {
			return fmt.Sprintf("blob[% x]", a.Blob)
		}
		return fmt.Sprintf("blob[%d bytes]", len(a.Blob))
	}
	if a.Num > 0xffff {
		return fmt.Sprintf("%#08x", a.Num)
	}
	return fmt.Sprintf("%#x", a.Num)
}

func (a Arg) tag() byte {
	switch a.Kind {
	case KindString:
		return TagString
	case KindBlob:
		return TagBlob
	}
	return TagNumber
}

// Message is one dbserver message.
type Message struct {
	TxID uint32
	Type uint16
	Args []Arg
}

func (m *Message) String() string {
	parts := make([]string, len(m.Args))
	for i, a := range m.Args {
		parts[i] = a.String()
	}
	return fmt.Sprintf("%s txid=%d args=[%s]", TypeName(m.Type), m.TxID, strings.Join(parts, " "))
}

// Arg returns argument i as a number (0 if absent or not a number).
func (m *Message) NumArg(i int) uint32 {
	if i < len(m.Args) && m.Args[i].Kind == KindNumber {
		return m.Args[i].Num
	}
	return 0
}

// Encode serializes m.
//
// An empty blob argument is omitted from the wire when the preceding
// argument is the number 0 (its length): this is how players and rekordbox
// send "no data" (e.g. menu items without artwork data). [DS][BL]
func (m *Message) Encode() []byte {
	var b []byte
	b = appendNum4(b, Magic)
	b = appendNum4(b, m.TxID)
	b = appendNum2(b, m.Type)
	b = append(b, fieldNum1, byte(len(m.Args)))
	// The tag blob has exactly one tag per argument in everything rekordbox 7
	// and the CDJ-3000 send [RB7] (older players padded it to 12 [DS]).
	tags := make([]byte, len(m.Args))
	for i, a := range m.Args {
		tags[i] = a.tag()
	}
	b = appendBlob(b, tags)
	for i, a := range m.Args {
		switch a.Kind {
		case KindNumber:
			b = appendNum4(b, a.Num)
		case KindString:
			b = appendString(b, a.Str)
		case KindBlob:
			if len(a.Blob) == 0 && i > 0 && m.Args[i-1].Kind == KindNumber && m.Args[i-1].Num == 0 {
				continue
			}
			b = appendBlob(b, a.Blob)
		}
	}
	return b
}

func appendNum4(b []byte, v uint32) []byte {
	return binary.BigEndian.AppendUint32(append(b, fieldNum4), v)
}

func appendNum2(b []byte, v uint16) []byte {
	return binary.BigEndian.AppendUint16(append(b, fieldNum2), v)
}

func appendBlob(b, p []byte) []byte {
	b = binary.BigEndian.AppendUint32(append(b, fieldBlob), uint32(len(p)))
	return append(b, p...)
}

func appendString(b []byte, s string) []byte {
	u := utf16.Encode([]rune(s))
	b = binary.BigEndian.AppendUint32(append(b, fieldString), uint32(len(u)+1))
	for _, c := range u {
		b = binary.BigEndian.AppendUint16(b, c)
	}
	return append(b, 0, 0)
}

// Errors.
var (
	ErrBadMagic = errors.New("dbserver: bad message magic")
	ErrBadField = errors.New("dbserver: unexpected field")
)

// Reader reads fields and messages from a stream.
type Reader struct {
	r *bufio.Reader
}

// NewReader wraps r.
func NewReader(r io.Reader) *Reader { return &Reader{r: bufio.NewReaderSize(r, 64<<10)} }

// field is a decoded wire field.
type field struct {
	typ  byte
	num  uint32
	blob []byte
	str  string
}

func (rd *Reader) readField() (field, error) {
	t, err := rd.r.ReadByte()
	if err != nil {
		return field{}, err
	}
	f := field{typ: t}
	switch t {
	case fieldNum1:
		v, err := rd.r.ReadByte()
		f.num = uint32(v)
		return f, unexpectedEOF(err)
	case fieldNum2:
		var p [2]byte
		_, err := io.ReadFull(rd.r, p[:])
		f.num = uint32(binary.BigEndian.Uint16(p[:]))
		return f, unexpectedEOF(err)
	case fieldNum4:
		var p [4]byte
		_, err := io.ReadFull(rd.r, p[:])
		f.num = binary.BigEndian.Uint32(p[:])
		return f, unexpectedEOF(err)
	case fieldBlob, fieldString:
		var p [4]byte
		if _, err := io.ReadFull(rd.r, p[:]); err != nil {
			return f, unexpectedEOF(err)
		}
		n := binary.BigEndian.Uint32(p[:])
		if t == fieldString {
			if n > maxStringLen {
				return f, fmt.Errorf("dbserver: string of %d units too long", n)
			}
			raw := make([]byte, 2*n)
			if _, err := io.ReadFull(rd.r, raw); err != nil {
				return f, unexpectedEOF(err)
			}
			u := make([]uint16, 0, n)
			for i := 0; i+1 < len(raw); i += 2 {
				c := binary.BigEndian.Uint16(raw[i:])
				if c == 0 {
					break
				}
				u = append(u, c)
			}
			f.str = string(utf16.Decode(u))
			return f, nil
		}
		if n > maxBlob {
			return f, fmt.Errorf("dbserver: blob of %d bytes too long", n)
		}
		f.blob = make([]byte, n)
		_, err := io.ReadFull(rd.r, f.blob)
		return f, unexpectedEOF(err)
	}
	return f, fmt.Errorf("%w: type byte %#02x", ErrBadField, t)
}

func unexpectedEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

func (rd *Reader) readNum() (uint32, error) {
	f, err := rd.readField()
	if err != nil {
		return 0, err
	}
	if f.typ != fieldNum1 && f.typ != fieldNum2 && f.typ != fieldNum4 {
		return 0, fmt.Errorf("%w: wanted a number, got %#02x", ErrBadField, f.typ)
	}
	return f.num, nil
}

// ReadGreeting reads the initial 4-byte number field (value 1).
func (rd *Reader) ReadGreeting() (uint32, error) { return rd.readNum() }

// ReadMessage reads one message. io.EOF is returned only at a clean
// message boundary.
func (rd *Reader) ReadMessage() (*Message, error) {
	magic, err := rd.readNum()
	if err != nil {
		return nil, err
	}
	if magic != Magic {
		return nil, fmt.Errorf("%w: %#08x", ErrBadMagic, magic)
	}
	m := &Message{}
	var argc uint32
	var typ uint32
	for _, p := range []*uint32{&m.TxID, &typ, &argc} {
		if *p, err = rd.readNum(); err != nil {
			return nil, unexpectedEOF(err)
		}
	}
	m.Type = uint16(typ)
	if argc > maxArgs {
		return nil, fmt.Errorf("dbserver: %d arguments is too many", argc)
	}
	tf, err := rd.readField()
	if err != nil {
		return nil, unexpectedEOF(err)
	}
	if tf.typ != fieldBlob {
		return nil, fmt.Errorf("%w: wanted tag blob, got %#02x", ErrBadField, tf.typ)
	}
	tags := tf.blob
	if int(argc) > len(tags) {
		return nil, fmt.Errorf("dbserver: %d arguments but only %d tags", argc, len(tags))
	}
	for i := 0; i < int(argc); i++ {
		tag := tags[i]
		// An empty blob is omitted when the previous number (its length) is 0.
		if tag == TagBlob && i > 0 && m.Args[i-1].Kind == KindNumber && m.Args[i-1].Num == 0 {
			if next, err := rd.r.Peek(1); err != nil || next[0] != fieldBlob {
				m.Args = append(m.Args, Blob(nil))
				continue
			}
		}
		f, err := rd.readField()
		if err != nil {
			return nil, unexpectedEOF(err)
		}
		switch {
		case tag == TagNumber && (f.typ == fieldNum1 || f.typ == fieldNum2 || f.typ == fieldNum4):
			m.Args = append(m.Args, Num(f.num))
		case tag == TagString && f.typ == fieldString:
			m.Args = append(m.Args, Str(f.str))
		case tag == TagBlob && f.typ == fieldBlob:
			m.Args = append(m.Args, Blob(f.blob))
		default:
			return nil, fmt.Errorf("%w: argument %d has tag %#02x but field %#02x", ErrBadField, i, tag, f.typ)
		}
	}
	return m, nil
}

// Parse decodes one message from the start of b and returns the number of
// bytes it occupied. A message cut short returns io.ErrUnexpectedEOF (or
// io.EOF for empty input), so stream reassembly can wait for more data.
func Parse(b []byte) (*Message, int, error) {
	br := bytes.NewReader(b)
	rd := NewReader(br)
	m, err := rd.ReadMessage()
	return m, len(b) - br.Len() - rd.r.Buffered(), err
}

// ParseNum decodes one number field (used for the greeting and the
// 12523 discovery exchange) and returns the bytes consumed.
func ParseNum(b []byte) (uint32, int, error) {
	br := bytes.NewReader(b)
	rd := NewReader(br)
	v, err := rd.readNum()
	return v, len(b) - br.Len() - rd.r.Buffered(), err
}
