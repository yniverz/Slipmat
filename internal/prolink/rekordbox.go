// SPDX-License-Identifier: GPL-3.0-or-later

package prolink

// Encoders for the port-50002 packets a rekordbox source sends.
// Byte layouts are adapted from Vynull's proto/status.go (GPL-3.0) [VN];
// the 0x11 hello is cross-checked against beat-link's template [BL].
// All of these await confirmation from a rekordbox 7 <-> CDJ-3000 capture.

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
// name shown on the CDJ. [BL][VN]
func EncodeRBHello(name string, dev uint8, host string) []byte {
	b := statusHeader(KindRBHello, 0x128, name, 0x01, dev, 0x0104)
	b[0x24] = dev
	b[0x25] = 0x01
	putUTF16BE(b[0x28:0x128], host)
	return b
}

// EncodeRBStatus builds the 0x16 short status (48 bytes). [VN]
func EncodeRBStatus(name string, dev uint8) []byte {
	return statusHeader(KindRBStatus, 0x30, name, 0x01, dev, 0)
}

// EncodeRBMixerStatus builds the 0x29 status rekordbox broadcasts (56 bytes):
// "always playing (F=c0), not master/synced". [DS][VN]
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

// EncodeMediaResponse builds the 0x06 media response (192 bytes). [DS][VN]
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

// DevSetting is the 6-byte DEVSETTING block embedded in link packets. [VN]
//
//	[0] ? (01)  [1] overview 01 half / 02 full  [2] colour 01 blue / 03 RGB / 04 3-band
//	[3] ? (01)  [4] key 01 classic / 02 alphanumeric  [5] waveform position 01 centre / 02 left
type DevSetting [6]byte

// DefaultDevSetting matches Vynull's defaults (full overview, RGB, alphanumeric, centre).
var DefaultDevSetting = DevSetting{0x01, 0x02, 0x03, 0x01, 0x02, 0x01}

// EncodeLinkActivate builds the 0x47 link activation (72 bytes). [VN]
func EncodeLinkActivate(name string, dev uint8, slot Slot, ds DevSetting) []byte {
	b := statusHeader(KindLinkActivate, 0x48, name, 0x01, dev, 0x0024)
	b[0x24] = dev
	b[0x25] = byte(slot)
	copy(b[0x28:0x2c], []byte{0x12, 0x34, 0x56, 0x78})
	b[0x2f] = 0x01
	copy(b[0x30:0x36], ds[:])
	return b
}
