// SPDX-License-Identifier: GPL-3.0-or-later

// Package anlz reads rekordbox analysis files (ANLZ0000.DAT/.EXT/.2EX) and
// converts their sections into the blobs the dbserver protocol sends.
//
// File layout [DS][crate-digger, as documentation]: a "PMAI" header, then
// tagged sections, each starting with a 4-byte tag, a 4-byte header length
// and a 4-byte total length (all big-endian). The reply formats were
// derived by comparing rekordbox 7's dbserver replies with the analysis
// files of the same track [RB7]; see convert.go.
package anlz

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf16"
)

// Section is one tagged section, including its header.
type Section struct {
	Tag    string
	Header int    // header length
	Raw    []byte // whole section (header + body)
}

// Body returns the section bytes after its header.
func (s Section) Body() []byte { return s.Raw[s.Header:] }

// File is a parsed analysis file.
type File struct {
	Path     string
	Sections []Section
}

const maxFile = 64 << 20

var ErrNotANLZ = errors.New("anlz: not an analysis file")

// Parse decodes analysis file contents. Truncated or inconsistent sections
// end the parse; the sections read so far are kept.
func Parse(b []byte) (*File, error) {
	if len(b) < 12 || string(b[:4]) != "PMAI" {
		return nil, ErrNotANLZ
	}
	f := &File{}
	off := int(binary.BigEndian.Uint32(b[4:8]))
	for off+12 <= len(b) {
		tag := string(b[off : off+4])
		hl := int(binary.BigEndian.Uint32(b[off+4 : off+8]))
		tl := int(binary.BigEndian.Uint32(b[off+8 : off+12]))
		if tl < 12 || hl < 12 || hl > tl || off+tl > len(b) {
			break
		}
		f.Sections = append(f.Sections, Section{Tag: tag, Header: hl, Raw: b[off : off+tl]})
		off += tl
	}
	return f, nil
}

// ReadFile reads and parses an analysis file.
func ReadFile(path string) (*File, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Size() > maxFile {
		return nil, fmt.Errorf("anlz: %s is implausibly large (%d bytes)", path, st.Size())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	f.Path = path
	return f, nil
}

// Section returns the first section with tag.
func (f *File) Section(tag string) (Section, bool) {
	if f == nil {
		return Section{}, false
	}
	for _, s := range f.Sections {
		if s.Tag == tag {
			return s, true
		}
	}
	return Section{}, false
}

// TrackPath returns the audio path recorded in the PPTH section: the full
// path on a USB export ("/Contents/…"), or "?/<file name>" in rekordbox's
// own analysis folder. [RB7]
func (f *File) TrackPath() string {
	s, ok := f.Section("PPTH")
	if !ok {
		return ""
	}
	body := s.Raw[s.Header:]
	if s.Header >= 16 { // header holds the path length at offset 12
		n := int(binary.BigEndian.Uint32(s.Raw[12:16]))
		if n <= len(body) {
			body = body[:n]
		}
	}
	u := make([]uint16, 0, len(body)/2)
	for i := 0; i+1 < len(body); i += 2 {
		c := binary.BigEndian.Uint16(body[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return strings.TrimSpace(string(utf16.Decode(u)))
}
