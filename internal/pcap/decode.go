// SPDX-License-Identifier: GPL-3.0-or-later

package pcap

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net/netip"
	"time"
)

// Transport protocol numbers.
const (
	ProtoTCP = 6
	ProtoUDP = 17
)

// TCP flag bits.
const (
	TCPFin = 0x01
	TCPSyn = 0x02
	TCPRst = 0x04
	TCPPsh = 0x08
	TCPAck = 0x10
)

// Packet is a decoded IPv4 UDP datagram or TCP segment.
type Packet struct {
	Time    time.Time
	Proto   uint8 // ProtoUDP or ProtoTCP
	Src     netip.AddrPort
	Dst     netip.AddrPort
	SrcMAC  [6]byte // zero when the link type has no MAC addresses
	Payload []byte

	// TCP only.
	Seq, Ack uint32
	Flags    uint8
}

func (p *Packet) String() string {
	proto := "UDP"
	if p.Proto == ProtoTCP {
		proto = "TCP"
	}
	return fmt.Sprintf("%s %s -> %s len=%d", proto, p.Src, p.Dst, len(p.Payload))
}

// Decoder turns frames into IPv4 UDP/TCP packets, reassembling fragmented
// IPv4 datagrams (NFS READ replies are routinely fragmented) and optionally
// dropping the duplicate copies that macOS pktap captures record for
// broadcast frames.
type Decoder struct {
	// Dedupe drops a packet identical to one seen within DedupeWindow.
	Dedupe       bool
	DedupeWindow time.Duration

	frags  map[fragKey]*fragBuf
	recent []*Packet
}

type fragKey struct {
	src, dst netip.Addr
	id       uint16
	proto    uint8
}

type fragBuf struct {
	first    time.Time
	data     []byte
	have     []bool // per 8-byte block
	total    int    // -1 until the last fragment arrives
	srcMAC   [6]byte
	headerOK bool
}

// NewDecoder returns a decoder with pktap duplicate suppression enabled.
func NewDecoder() *Decoder {
	return &Decoder{Dedupe: true, DedupeWindow: 2 * time.Millisecond, frags: map[fragKey]*fragBuf{}}
}

// Decode returns the packet carried by f, or nil if the frame is not an
// IPv4 UDP/TCP packet (or is an incomplete fragment / duplicate).
func (d *Decoder) Decode(f *Frame) (*Packet, error) {
	ip, mac, err := linkPayload(f.LinkType, f.Data)
	if err != nil || ip == nil {
		return nil, err
	}
	p, err := d.decodeIPv4(f.Time, ip, mac)
	if err != nil || p == nil {
		return p, err
	}
	if d.Dedupe && d.isDuplicate(p) {
		return nil, nil
	}
	return p, nil
}

func (d *Decoder) isDuplicate(p *Packet) bool {
	keep := d.recent[:0]
	dup := false
	for _, q := range d.recent {
		if p.Time.Sub(q.Time) > d.DedupeWindow {
			continue
		}
		keep = append(keep, q)
		if q.Proto == p.Proto && q.Src == p.Src && q.Dst == p.Dst && q.Seq == p.Seq && bytes.Equal(q.Payload, p.Payload) {
			dup = true
		}
	}
	d.recent = append(keep, p)
	return dup
}

// linkPayload strips the link-layer header and returns the IPv4 packet.
func linkPayload(linkType uint32, b []byte) (ip []byte, mac [6]byte, err error) {
	switch linkType {
	case LinkEthernet:
		if len(b) < 14 {
			return nil, mac, nil
		}
		copy(mac[:], b[6:12])
		et := binary.BigEndian.Uint16(b[12:14])
		b = b[14:]
		for et == 0x8100 || et == 0x88a8 { // VLAN tags
			if len(b) < 4 {
				return nil, mac, nil
			}
			et = binary.BigEndian.Uint16(b[2:4])
			b = b[4:]
		}
		if et != 0x0800 {
			return nil, mac, nil
		}
		return b, mac, nil
	case LinkNull, LinkLoop:
		if len(b) < 4 {
			return nil, mac, nil
		}
		fam := binary.LittleEndian.Uint32(b[0:4])
		if linkType == LinkLoop || fam > 0xffff {
			fam = binary.BigEndian.Uint32(b[0:4])
		}
		if fam != 2 { // AF_INET
			return nil, mac, nil
		}
		return b[4:], mac, nil
	case LinkRaw, linkRawAlt1, linkRawAlt2:
		if len(b) > 0 && b[0]>>4 == 4 {
			return b, mac, nil
		}
		return nil, mac, nil
	case LinkSLL:
		if len(b) < 16 || binary.BigEndian.Uint16(b[14:16]) != 0x0800 {
			return nil, mac, nil
		}
		if binary.BigEndian.Uint16(b[4:6]) == 6 {
			copy(mac[:], b[6:12])
		}
		return b[16:], mac, nil
	case LinkSLL2:
		if len(b) < 20 || binary.BigEndian.Uint16(b[0:2]) != 0x0800 {
			return nil, mac, nil
		}
		if b[11] == 6 {
			copy(mac[:], b[12:18])
		}
		return b[20:], mac, nil
	case LinkPKTAP, LinkPKTAP2:
		// Apple pktap header: u32 LE header length, u32 LE inner DLT, ...
		if len(b) < 8 {
			return nil, mac, nil
		}
		hl := binary.LittleEndian.Uint32(b[0:4])
		inner := binary.LittleEndian.Uint32(b[4:8])
		if hl < 8 || int(hl) > len(b) {
			return nil, mac, fmt.Errorf("pktap: bad header length %d", hl)
		}
		if inner == LinkPKTAP || inner == LinkPKTAP2 {
			return nil, mac, nil
		}
		return linkPayload(inner, b[hl:])
	default:
		return nil, mac, fmt.Errorf("unsupported link type %d", linkType)
	}
}

func (d *Decoder) decodeIPv4(t time.Time, b []byte, mac [6]byte) (*Packet, error) {
	if len(b) < 20 || b[0]>>4 != 4 {
		return nil, nil
	}
	ihl := int(b[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(b[2:4]))
	if ihl < 20 || total < ihl || len(b) < ihl {
		return nil, fmt.Errorf("ipv4: bad header (ihl=%d total=%d caplen=%d)", ihl, total, len(b))
	}
	if total > len(b) {
		total = len(b) // snaplen-truncated
	}
	proto := b[9]
	if proto != ProtoUDP && proto != ProtoTCP {
		return nil, nil
	}
	src, _ := netip.AddrFromSlice(b[12:16])
	dst, _ := netip.AddrFromSlice(b[16:20])
	flagsFrag := binary.BigEndian.Uint16(b[6:8])
	more := flagsFrag&0x2000 != 0
	off := int(flagsFrag&0x1fff) * 8
	body := b[ihl:total]
	if more || off != 0 {
		body = d.reassemble(t, fragKey{src, dst, binary.BigEndian.Uint16(b[4:6]), proto}, off, more, body, mac)
		if body == nil {
			return nil, nil
		}
	}
	return transport(t, proto, src, dst, body, mac)
}

func (d *Decoder) reassemble(t time.Time, k fragKey, off int, more bool, body []byte, mac [6]byte) []byte {
	if d.frags == nil {
		d.frags = map[fragKey]*fragBuf{}
	}
	// Expire stale buffers.
	for key, fb := range d.frags {
		if t.Sub(fb.first) > 30*time.Second {
			delete(d.frags, key)
		}
	}
	fb := d.frags[k]
	if fb == nil {
		fb = &fragBuf{first: t, total: -1}
		d.frags[k] = fb
	}
	end := off + len(body)
	if end > 65535 {
		delete(d.frags, k)
		return nil
	}
	if end > len(fb.data) {
		fb.data = append(fb.data, make([]byte, end-len(fb.data))...)
		fb.have = append(fb.have, make([]bool, (end+7)/8-len(fb.have))...)
	}
	copy(fb.data[off:], body)
	for i := off / 8; i < (end+7)/8; i++ {
		fb.have[i] = true
	}
	if off == 0 {
		fb.srcMAC = mac
		fb.headerOK = true
	}
	if !more {
		fb.total = end
	}
	if fb.total < 0 || !fb.headerOK {
		return nil
	}
	for i := 0; i < (fb.total+7)/8; i++ {
		if !fb.have[i] {
			return nil
		}
	}
	delete(d.frags, k)
	return fb.data[:fb.total]
}

func transport(t time.Time, proto uint8, src, dst netip.Addr, b []byte, mac [6]byte) (*Packet, error) {
	p := &Packet{Time: t, Proto: proto, SrcMAC: mac}
	switch proto {
	case ProtoUDP:
		if len(b) < 8 {
			return nil, fmt.Errorf("udp: short header")
		}
		l := int(binary.BigEndian.Uint16(b[4:6]))
		if l < 8 || l > len(b) {
			l = len(b)
		}
		p.Src = netip.AddrPortFrom(src, binary.BigEndian.Uint16(b[0:2]))
		p.Dst = netip.AddrPortFrom(dst, binary.BigEndian.Uint16(b[2:4]))
		p.Payload = b[8:l]
	case ProtoTCP:
		if len(b) < 20 {
			return nil, fmt.Errorf("tcp: short header")
		}
		doff := int(b[12]>>4) * 4
		if doff < 20 || doff > len(b) {
			return nil, fmt.Errorf("tcp: bad data offset %d", doff)
		}
		p.Src = netip.AddrPortFrom(src, binary.BigEndian.Uint16(b[0:2]))
		p.Dst = netip.AddrPortFrom(dst, binary.BigEndian.Uint16(b[2:4]))
		p.Seq = binary.BigEndian.Uint32(b[4:8])
		p.Ack = binary.BigEndian.Uint32(b[8:12])
		p.Flags = b[13]
		p.Payload = b[doff:]
	}
	return p, nil
}

// ReadAll decodes every IPv4 UDP/TCP packet in a capture stream.
func ReadAll(r io.Reader) ([]*Packet, error) {
	rd, err := NewReader(r)
	if err != nil {
		return nil, err
	}
	dec := NewDecoder()
	var out []*Packet
	for {
		f, err := rd.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		p, err := dec.Decode(f)
		if err != nil || p == nil {
			continue
		}
		out = append(out, p)
	}
}
