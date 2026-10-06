// SPDX-License-Identifier: GPL-3.0-or-later

package prolink

// Encoders for the port-50002 packets a rekordbox source sends. Layouts were
// adapted from Vynull's proto/status.go (GPL-3.0) [VN] and beat-link [BL],
// and each one is now verified byte-for-byte against rekordbox 7 [RB7]
// (see encode_test.go).

import "encoding/binary"

// statusHeader builds the common port-50002 header (name at 0x0b).
func statusHeader(kind uint8, n int, name string, subtype, dev uint8, remaining uint16) []byte {
	b := make([]byte, n)
	putHeader(b, kind)
	putCString(b[0x0b:0x1f], name)
	b[0x1f] = 0x01
	b[0x20] = subtype
	b[0x21] = dev
	binary.BigEndian.PutUint16(b[0x22:0x24], remaining)
	return b
}

// EncodeRBHello builds the 0x11 announce (296 bytes) carrying the computer
// name, sent in answer to a CDJ's 0x10 query. [BL][VN][RB7]
func EncodeRBHello(name string, dev uint8, host string) []byte {
	b := statusHeader(KindRBHello, 0x128, name, 0x01, dev, 0x0104)
	b[0x24] = dev
	b[0x25] = 0x01
	putUTF16BE(b[0x28:0x128], host)
	return b
}

// EncodeRBStatus builds the 0x16 packet (48 bytes). rekordbox 7 sends it
// unprompted to each CDJ when its export changes (startup, quit); the CDJs
// then re-read the MOUNT export list. [RB7]
func EncodeRBStatus(name string, dev uint8) []byte {
	return statusHeader(KindRBStatus, 0x30, name, 0x01, dev, 0)
}

// EncodeRBMixerStatus builds the 0x29 status rekordbox broadcasts (56 bytes):
// "always playing (F=c0), not master/synced", broadcast every ~100 ms. [DS][VN][RB7]
func EncodeRBMixerStatus(name string, dev uint8) []byte {
	b := statusHeader(KindMixerStatus, 0x38, name, 0x01, dev, 0x0038)
	b[0x24] = dev
	b[0x27] = 0xc0
	b[0x29] = 0x10 // pitch 0x00100000 = +0%
	b[0x31] = 0x10
	b[0x35] = 0x09
	b[0x36] = 0xff
	return b
}

// MediaInfo describes the collection we advertise in a media response.
type MediaInfo struct {
	Name      string
	Tracks    uint16
	Playlists uint16
	Settings  bool // My Settings available
}

// EncodeMediaResponse builds the 0x06 media response (192 bytes). [DS][VN][RB7]
func EncodeMediaResponse(name string, dev uint8, slot Slot, m MediaInfo) []byte {
	b := statusHeader(KindMediaResponse, 0xc0, name, 0x01, dev, 0x009c)
	b[0x27] = dev
	b[0x2b] = byte(slot)
	putUTF16BE(b[0x2c:0x6c], m.Name)
	binary.BigEndian.PutUint16(b[0xa6:0xa8], m.Tracks)
	b[0xaa] = 0x01 // track type: rekordbox (analysed)
	if m.Settings {
		b[0xab] = 0x01
	}
	binary.BigEndian.PutUint16(b[0xae:0xb0], m.Playlists)
	return b
}

// EncodeLinkActivate builds the 0x47 link activation (72 bytes) rekordbox
// sends in answer to a CDJ's 0x46 link ping. Bytes 0x30-0x38 are 01 in
// rekordbox 7 (Vynull reads 0x30-0x35 as a DEVSETTING block; unconfirmed).
// [RB7]
func EncodeLinkActivate(name string, dev uint8, slot Slot) []byte {
	b := statusHeader(KindLinkActivate, 0x48, name, 0x01, dev, 0x0024)
	b[0x24] = dev
	b[0x25] = byte(slot)
	copy(b[0x28:0x2c], []byte{0x12, 0x34, 0x56, 0x78})
	b[0x2f] = 0x01
	for i := 0x30; i <= 0x38; i++ {
		b[i] = 0x01
	}
	return b
}
