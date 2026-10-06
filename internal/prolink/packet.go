// SPDX-License-Identifier: GPL-3.0-or-later

// Package prolink is a pure (no I/O) codec for Pro DJ Link UDP packets on
// ports 50000 (announce), 50001 (beat/sync) and 50002 (status).
//
// Provenance of layouts:
//   - [DS]  Deep Symmetry, "DJ Link Ecosystem Analysis" (djl-analysis.deepsymmetry.org)
//   - [DSC] Deep Symmetry CDJ-2000NXS hardware captures (dysentery/doc/assets/captures)
//   - [BL]  beat-link VirtualRekordbox packet templates
//   - [VN]  Vynull (GPL-3.0), proto/ package — server-side findings adapted here
//   - [RB7] our own rekordbox 7 <-> 2x CDJ-3000 (fw 3.22) capture, 2026-10-06
//
// Anything marked only [VN] is a hypothesis until confirmed by [RB7].
package prolink

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"unicode/utf16"
)

// UDP ports.
const (
	PortAnnounce   = 50000
	PortBeat       = 50001
	PortStatus     = 50002
	PortTouchAudio = 50004
)

// Magic is the 10-byte header "Qspt1WmJOL" that starts every packet. [DS]
var Magic = [10]byte{0x51, 0x73, 0x70, 0x74, 0x31, 0x57, 0x6d, 0x4a, 0x4f, 0x4c}

// HeaderLen is the length of magic + kind byte.
const HeaderLen = 11

var (
	ErrShort    = errors.New("prolink: packet too short")
	ErrBadMagic = errors.New("prolink: bad magic")
)

// DeviceType is the device class in keep-alive byte 0x34: 01 for CDJs
// [DS][DSC][RB7], 02 for mixers [DS], 04 for rekordbox [BL][RB7].
// (Byte 0x21 is NOT a device type: CDJ-3000s and rekordbox 7 both send 03.)
type DeviceType uint8

const (
	DeviceCDJ       DeviceType = 0x01
	DeviceMixer     DeviceType = 0x02
	DeviceRekordbox DeviceType = 0x04
)

func (d DeviceType) String() string {
	switch d {
	case DeviceMixer:
		return "mixer"
	case DeviceCDJ:
		return "cdj"
	case DeviceRekordbox:
		return "rekordbox"
	}
	return fmt.Sprintf("type%#02x", uint8(d))
}

// Slot identifies a media slot. [DS]
type Slot uint8

const (
	SlotNone      Slot = 0x00
	SlotCD        Slot = 0x01
	SlotSD        Slot = 0x02
	SlotUSB       Slot = 0x03
	SlotRekordbox Slot = 0x04
)

func (s Slot) String() string {
	switch s {
	case SlotNone:
		return "none"
	case SlotCD:
		return "cd"
	case SlotSD:
		return "sd"
	case SlotUSB:
		return "usb"
	case SlotRekordbox:
		return "rekordbox"
	}
	return fmt.Sprintf("slot%#02x", uint8(s))
}

// Kind returns the packet kind byte (offset 0x0a) after validating magic.
func Kind(b []byte) (uint8, error) {
	if len(b) < HeaderLen {
		return 0, ErrShort
	}
	if !bytes.Equal(b[:10], Magic[:]) {
		return 0, ErrBadMagic
	}
	return b[0x0a], nil
}

// putHeader writes magic + kind.
func putHeader(b []byte, kind uint8) {
	copy(b, Magic[:])
	b[0x0a] = kind
}

// cString reads a NUL-padded ASCII string.
func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.ToValidUTF8(string(b), "?")
}

// putCString writes s NUL-padded into b (truncating).
func putCString(b []byte, s string) {
	clear(b)
	copy(b, s)
}

// utf16BE decodes a NUL-terminated UTF-16BE string.
func utf16BE(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.BigEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

// putUTF16BE writes s as UTF-16BE into b, truncated to leave room for a NUL.
func putUTF16BE(b []byte, s string) {
	clear(b)
	u := utf16.Encode([]rune(s))
	if max := len(b)/2 - 1; len(u) > max {
		u = u[:max]
	}
	for i, c := range u {
		binary.BigEndian.PutUint16(b[2*i:], c)
	}
}

func ip4(b []byte) netip.Addr {
	a, _ := netip.AddrFromSlice(b[:4])
	return a
}

func mac(b []byte) [6]byte {
	var m [6]byte
	copy(m[:], b)
	return m
}

// MACString formats a hardware address.
func MACString(m [6]byte) string {
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", m[0], m[1], m[2], m[3], m[4], m[5])
}

// need returns ErrShort unless len(b) >= n.
func need(b []byte, n int) error {
	if len(b) < n {
		return fmt.Errorf("%w: %d bytes, need %d", ErrShort, len(b), n)
	}
	return nil
}
