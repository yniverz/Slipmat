// SPDX-License-Identifier: GPL-3.0-or-later

package prolink

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

// Port 50000 packet kinds. [DS]
const (
	KindClaim1          uint8 = 0x00 // first-stage claim (MAC)
	KindAssignIntention uint8 = 0x01 // mixer -> device on a channel port
	KindClaim2          uint8 = 0x02 // second-stage claim (IP, MAC, number); rekordbox also repeats it [VN]
	KindAssign          uint8 = 0x03 // mixer channel assignment
	KindClaim3          uint8 = 0x04 // final-stage claim (number)
	KindAssignFinished  uint8 = 0x05 // assignment finished (unicast)
	KindKeepAlive       uint8 = 0x06 // keep-alive
	KindConflict        uint8 = 0x08 // channel conflict (unicast)
	KindHello           uint8 = 0x0a // initial announcement
)

// AnnounceKindName names a port-50000 packet kind.
func AnnounceKindName(k uint8) string {
	switch k {
	case KindClaim1:
		return "claim-1"
	case KindAssignIntention:
		return "assign-intention"
	case KindClaim2:
		return "claim-2"
	case KindAssign:
		return "assign"
	case KindClaim3:
		return "claim-3"
	case KindAssignFinished:
		return "assign-finished"
	case KindKeepAlive:
		return "keep-alive"
	case KindConflict:
		return "conflict"
	case KindHello:
		return "hello"
	}
	return fmt.Sprintf("unknown-%#02x", k)
}

// Announce is a decoded port-50000 packet. Common header layout [DS][DSC]:
//
//	00-09 magic, 0a kind, 0b 00, 0c-1f name (NUL padded), 20 0x01,
//	21 device type / structure byte, 22-23 packet length (BE), 24.. payload
//
// Fields not carried by a given kind are left zero.
type Announce struct {
	Kind       uint8
	Name       string
	DeviceType DeviceType // byte 0x21
	Length     uint16     // declared packet length (bytes 0x22-0x23)

	DeviceNumber uint8      // claim-2 (0x2e), claim-3/keep-alive/conflict/finished (0x24)
	Counter      uint8      // N: claim-1 (0x24), claim-2 (0x2f), claim-3 (0x25)
	Class        uint8      // hello (0x24), claim-1 (0x25), claim-2 (0x30), keep-alive (0x25)
	AutoAssign   uint8      // claim-2 (0x31): 01 auto, 02 specific [DS]
	MAC          [6]byte    // claim-1 (0x26), claim-2 (0x28), keep-alive (0x26)
	IP           netip.Addr // claim-2 (0x24), keep-alive (0x2c), conflict (0x25)
	Peers        uint8      // keep-alive (0x30)
	Tail         []byte     // keep-alive bytes 0x31.. (meaning partly unknown)
}

// DecodeAnnounce parses a port-50000 packet. Unknown kinds decode the
// common header only (no error) so the monitor can still show them.
func DecodeAnnounce(b []byte) (*Announce, error) {
	k, err := Kind(b)
	if err != nil {
		return nil, err
	}
	if err := need(b, 0x24); err != nil {
		return nil, err
	}
	a := &Announce{
		Kind:       k,
		Name:       cString(b[0x0c:0x20]),
		DeviceType: DeviceType(b[0x21]),
		Length:     binary.BigEndian.Uint16(b[0x22:0x24]),
	}
	switch k {
	case KindHello:
		if err := need(b, 0x25); err != nil {
			return nil, err
		}
		a.Class = b[0x24]
	case KindClaim1:
		if err := need(b, 0x2c); err != nil {
			return nil, err
		}
		a.Counter, a.Class, a.MAC = b[0x24], b[0x25], mac(b[0x26:0x2c])
	case KindClaim2:
		if err := need(b, 0x32); err != nil {
			return nil, err
		}
		a.IP, a.MAC = ip4(b[0x24:0x28]), mac(b[0x28:0x2e])
		a.DeviceNumber, a.Counter, a.Class, a.AutoAssign = b[0x2e], b[0x2f], b[0x30], b[0x31]
	case KindClaim3:
		if err := need(b, 0x26); err != nil {
			return nil, err
		}
		a.DeviceNumber, a.Counter = b[0x24], b[0x25]
	case KindKeepAlive:
		if err := need(b, 0x31); err != nil {
			return nil, err
		}
		a.DeviceNumber, a.Class = b[0x24], b[0x25]
		a.MAC, a.IP, a.Peers = mac(b[0x26:0x2c]), ip4(b[0x2c:0x30]), b[0x30]
		a.Tail = append([]byte(nil), b[0x31:]...)
	case KindConflict:
		if err := need(b, 0x29); err != nil {
			return nil, err
		}
		a.DeviceNumber, a.IP = b[0x24], ip4(b[0x25:0x29])
	case KindAssignFinished, KindAssign:
		if err := need(b, 0x25); err != nil {
			return nil, err
		}
		a.DeviceNumber = b[0x24]
	}
	return a, nil
}

// String renders a one-line summary for logs.
func (a *Announce) String() string {
	s := fmt.Sprintf("%s name=%q type=%s len=%d", AnnounceKindName(a.Kind), a.Name, a.DeviceType, a.Length)
	switch a.Kind {
	case KindHello:
		s += fmt.Sprintf(" class=%#02x", a.Class)
	case KindClaim1:
		s += fmt.Sprintf(" n=%d class=%#02x mac=%s", a.Counter, a.Class, MACString(a.MAC))
	case KindClaim2:
		s += fmt.Sprintf(" dev=%d n=%d class=%#02x auto=%#02x ip=%s mac=%s", a.DeviceNumber, a.Counter, a.Class, a.AutoAssign, a.IP, MACString(a.MAC))
	case KindClaim3:
		s += fmt.Sprintf(" dev=%d n=%d", a.DeviceNumber, a.Counter)
	case KindKeepAlive:
		s += fmt.Sprintf(" dev=%d class=%#02x ip=%s mac=%s peers=%d tail=% x", a.DeviceNumber, a.Class, a.IP, MACString(a.MAC), a.Peers, a.Tail)
	case KindConflict:
		s += fmt.Sprintf(" dev=%d ip=%s", a.DeviceNumber, a.IP)
	case KindAssign, KindAssignFinished:
		s += fmt.Sprintf(" dev=%d", a.DeviceNumber)
	}
	return s
}

// announceHeader builds the common port-50000 header for a packet of size n.
func announceHeader(kind uint8, n int, name string, typ DeviceType) []byte {
	b := make([]byte, n)
	putHeader(b, kind)
	putCString(b[0x0c:0x20], name)
	b[0x20] = 0x01
	b[0x21] = byte(typ)
	binary.BigEndian.PutUint16(b[0x22:0x24], uint16(n))
	return b
}

// Identity is what a device says about itself on the link.
type Identity struct {
	Name         string
	DeviceNumber uint8
	MAC          [6]byte
	IP           netip.Addr
}

// RekordboxClass is the device class byte rekordbox uses in claim packets
// (claim-1 0x25, claim-2 0x30). [VN]
const RekordboxClass = 0x04

// EncodeRekordboxClaim1 builds a first-stage claim as rekordbox sends it. [VN]
func EncodeRekordboxClaim1(id Identity, counter uint8) []byte {
	b := announceHeader(KindClaim1, 0x2c, id.Name, DeviceRekordbox)
	b[0x24] = counter
	b[0x25] = RekordboxClass
	copy(b[0x26:0x2c], id.MAC[:])
	return b
}

// EncodeRekordboxClaim2 builds a second-stage claim for device number dev. [VN]
// Counter is N; rekordbox sends auto-assign (0x01).
func EncodeRekordboxClaim2(id Identity, dev, counter uint8) []byte {
	b := announceHeader(KindClaim2, 0x32, id.Name, DeviceRekordbox)
	ip := id.IP.As4()
	copy(b[0x24:0x28], ip[:])
	copy(b[0x28:0x2e], id.MAC[:])
	b[0x2e] = dev
	b[0x2f] = counter
	b[0x30] = RekordboxClass
	b[0x31] = 0x01
	return b
}

// EncodeRekordboxKeepAlive builds a rekordbox keep-alive (0x06, 54 bytes).
// Bytes 0x25 = 01 and 0x31..0x35 = 01 00 00 04 08 are identical in [BL]
// (captured from rekordbox) and [VN]. Peers is the number of devices seen,
// including ourselves.
func EncodeRekordboxKeepAlive(id Identity, peers uint8) []byte {
	b := announceHeader(KindKeepAlive, 0x36, id.Name, DeviceRekordbox)
	b[0x24] = id.DeviceNumber
	b[0x25] = 0x01
	copy(b[0x26:0x2c], id.MAC[:])
	ip := id.IP.As4()
	copy(b[0x2c:0x30], ip[:])
	b[0x30] = peers
	copy(b[0x31:0x36], []byte{0x01, 0x00, 0x00, 0x04, 0x08})
	return b
}
