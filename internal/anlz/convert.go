// SPDX-License-Identifier: GPL-3.0-or-later

package anlz

import "encoding/binary"

// The functions below turn analysis sections into the blobs rekordbox 7
// sends over the dbserver protocol. Each one was checked byte for byte
// against rekordbox's replies for a track whose analysis files we had. [RB7]

// TagBlob is the blob for 0x2c04/0x2d04 tag requests (PWV4, PWV5, PQT2,
// PSSI, PWV6, PWV7, …): a little-endian length, then the whole section
// including its header, zero-padded to a multiple of 4 (the length counts
// the padding).
func TagBlob(s Section) []byte {
	pad := (4 - len(s.Raw)%4) % 4
	out := binary.LittleEndian.AppendUint32(nil, uint32(len(s.Raw)+pad))
	out = append(out, s.Raw...)
	return append(out, make([]byte, pad)...)
}

// BeatGrid converts PQTZ into the 0x2204 beat grid blob: a 20-byte
// little-endian header (PQTZ's unknown 0x00080000, beat count, beat bytes,
// 1, 1), then 16 bytes per beat: beat-in-bar (u16 LE), BPM × 100 (u16 LE),
// time in ms (u32 LE), eight 0xff bytes.
func BeatGrid(pqtz Section) ([]byte, bool) {
	if pqtz.Header < 24 || len(pqtz.Raw) < 24 {
		return nil, false
	}
	unk := binary.BigEndian.Uint32(pqtz.Raw[16:20])
	body := pqtz.Body()
	n := len(body) / 8
	if c := int(binary.BigEndian.Uint32(pqtz.Raw[20:24])); c < n {
		n = c
	}
	out := make([]byte, 0, 20+16*n)
	for _, v := range []uint32{unk, uint32(n), uint32(16 * n), 1, 1} {
		out = binary.LittleEndian.AppendUint32(out, v)
	}
	for i := 0; i < n; i++ {
		e := body[8*i:]
		out = binary.LittleEndian.AppendUint16(out, binary.BigEndian.Uint16(e[0:2]))
		out = binary.LittleEndian.AppendUint16(out, binary.BigEndian.Uint16(e[2:4]))
		out = binary.LittleEndian.AppendUint32(out, binary.BigEndian.Uint32(e[4:8]))
		out = append(out, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
	}
	return out, true
}

// FirstBPM returns the tempo of the first beat in PQTZ (BPM × 100).
func FirstBPM(pqtz Section) uint32 {
	if len(pqtz.Body()) < 8 {
		return 0
	}
	return uint32(binary.BigEndian.Uint16(pqtz.Body()[2:4]))
}

// Preview builds the 0x2004 waveform preview blob: PWAV's 400 columns as
// (height, whiteness) byte pairs, PWV2's 100-column tiny preview, and four
// trailing bytes (rekordbox sends values we can't reproduce; players ignore
// them — their own 0x2005 uploads are 900 bytes).
func Preview(pwav Section, pwv2 Section, havePWV2 bool) []byte {
	cols := pwav.Body()
	if len(cols) > 400 {
		cols = cols[:400]
	}
	out := make([]byte, 0, 904)
	for _, c := range cols {
		out = append(out, c&0x1f, c>>5)
	}
	out = append(out, make([]byte, 800-len(out))...)
	tiny := make([]byte, 100)
	if havePWV2 {
		copy(tiny, pwv2.Body())
	}
	out = append(out, tiny...)
	return append(out, 0, 0, 0, 0)
}

// Detail builds the 0x2904 scrolling waveform blob from PWV3: a 20-byte
// little-endian header (entries, entry size, entries, 150 entries/s, 1)
// followed by the entries.
func Detail(pwv3 Section) ([]byte, bool) {
	if pwv3.Header < 24 || len(pwv3.Raw) < 24 {
		return nil, false
	}
	entrySize := binary.BigEndian.Uint32(pwv3.Raw[12:16])
	n := binary.BigEndian.Uint32(pwv3.Raw[16:20])
	rate := uint32(binary.BigEndian.Uint16(pwv3.Raw[20:22]))
	body := pwv3.Body()
	if int(n*max(entrySize, 1)) > len(body) {
		n = uint32(len(body)) / max(entrySize, 1)
	}
	out := make([]byte, 0, 20+len(body))
	for _, v := range []uint32{n, entrySize, n, rate, 1} {
		out = binary.LittleEndian.AppendUint32(out, v)
	}
	return append(out, body[:n*max(entrySize, 1)]...), true
}

// SeekIndex converts PVBR (the VBR seek table) for 0x2504: the section
// body with every 32-bit word byte-swapped to little-endian.
func SeekIndex(pvbr Section) []byte {
	body := pvbr.Body()
	out := make([]byte, 0, len(body))
	for i := 0; i+4 <= len(body); i += 4 {
		out = binary.LittleEndian.AppendUint32(out, binary.BigEndian.Uint32(body[i:]))
	}
	return out
}
