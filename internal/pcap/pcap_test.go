// SPDX-License-Identifier: GPL-3.0-or-later

package pcap

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

// buildUDPFrame builds an Ethernet/IPv4/UDP frame. ipID/fragOff/more allow
// building fragments of a larger datagram (payload is then the raw IP body).
func buildIPv4(src, dst [4]byte, proto uint8, id uint16, fragOff int, more bool, body []byte) []byte {
	ip := make([]byte, 20+len(body))
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(len(ip)))
	binary.BigEndian.PutUint16(ip[4:6], id)
	ff := uint16(fragOff / 8)
	if more {
		ff |= 0x2000
	}
	binary.BigEndian.PutUint16(ip[6:8], ff)
	ip[8] = 64
	ip[9] = proto
	copy(ip[12:16], src[:])
	copy(ip[16:20], dst[:])
	copy(ip[20:], body)
	return ip
}

func udpBody(sport, dport uint16, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint16(b[0:2], sport)
	binary.BigEndian.PutUint16(b[2:4], dport)
	binary.BigEndian.PutUint16(b[4:6], uint16(len(b)))
	copy(b[8:], payload)
	return b
}

func ether(ip []byte) []byte {
	f := make([]byte, 14+len(ip))
	copy(f[0:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	copy(f[6:12], []byte{0x02, 0x11, 0x22, 0x33, 0x44, 0x55})
	binary.BigEndian.PutUint16(f[12:14], 0x0800)
	copy(f[14:], ip)
	return f
}

func classicPcap(frames ...[]byte) []byte {
	var b bytes.Buffer
	h := make([]byte, 24)
	binary.LittleEndian.PutUint32(h[0:4], 0xa1b2c3d4)
	binary.LittleEndian.PutUint16(h[4:6], 2)
	binary.LittleEndian.PutUint16(h[6:8], 4)
	binary.LittleEndian.PutUint32(h[16:20], 65535)
	binary.LittleEndian.PutUint32(h[20:24], LinkEthernet)
	b.Write(h)
	for i, f := range frames {
		r := make([]byte, 16)
		binary.LittleEndian.PutUint32(r[0:4], 1700000000)
		binary.LittleEndian.PutUint32(r[4:8], uint32(i*10000)) // 10ms apart
		binary.LittleEndian.PutUint32(r[8:12], uint32(len(f)))
		binary.LittleEndian.PutUint32(r[12:16], uint32(len(f)))
		b.Write(r)
		b.Write(f)
	}
	return b.Bytes()
}

func ngBlock(typ uint32, body []byte) []byte {
	pad := (4 - len(body)%4) % 4
	total := 12 + len(body) + pad
	b := make([]byte, total)
	binary.LittleEndian.PutUint32(b[0:4], typ)
	binary.LittleEndian.PutUint32(b[4:8], uint32(total))
	copy(b[8:], body)
	binary.LittleEndian.PutUint32(b[total-4:], uint32(total))
	return b
}

func pcapng(linkType uint16, frames ...[]byte) []byte {
	var b bytes.Buffer
	shb := make([]byte, 16)
	binary.LittleEndian.PutUint32(shb[0:4], 0x1a2b3c4d)
	binary.LittleEndian.PutUint16(shb[4:6], 1)
	binary.LittleEndian.PutUint64(shb[8:16], ^uint64(0))
	b.Write(ngBlock(ngSHB, shb))
	idb := make([]byte, 8)
	binary.LittleEndian.PutUint16(idb[0:2], linkType)
	// if_tsresol = 9 (nanoseconds)
	idb = append(idb, 9, 0, 1, 0, 9, 0, 0, 0, 0, 0, 0, 0)
	b.Write(ngBlock(ngIDB, idb))
	for _, f := range frames {
		epb := make([]byte, 20+len(f))
		ts := uint64(1700000000) * 1e9
		binary.LittleEndian.PutUint32(epb[4:8], uint32(ts>>32))
		binary.LittleEndian.PutUint32(epb[8:12], uint32(ts))
		binary.LittleEndian.PutUint32(epb[12:16], uint32(len(f)))
		binary.LittleEndian.PutUint32(epb[16:20], uint32(len(f)))
		copy(epb[20:], f)
		b.Write(ngBlock(ngEPB, epb))
	}
	return b.Bytes()
}

var (
	ipA = [4]byte{169, 254, 1, 2}
	ipB = [4]byte{169, 254, 255, 255}
)

func TestClassicPcapUDP(t *testing.T) {
	payload := []byte("Qspt1WmJOL\x06hello")
	f := ether(buildIPv4(ipA, ipB, ProtoUDP, 1, 0, false, udpBody(50000, 50000, payload)))
	pkts, err := ReadAll(bytes.NewReader(classicPcap(f)))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkts) != 1 {
		t.Fatalf("got %d packets", len(pkts))
	}
	p := pkts[0]
	if p.Proto != ProtoUDP || p.Src != netip.MustParseAddrPort("169.254.1.2:50000") || p.Dst.Port() != 50000 {
		t.Fatalf("bad packet %v", p)
	}
	if !bytes.Equal(p.Payload, payload) {
		t.Fatalf("payload mismatch: %q", p.Payload)
	}
	if p.SrcMAC != [6]byte{0x02, 0x11, 0x22, 0x33, 0x44, 0x55} {
		t.Fatalf("mac %x", p.SrcMAC)
	}
}

func TestPcapngPKTAP(t *testing.T) {
	payload := []byte("dbserver bytes")
	tcp := make([]byte, 20+len(payload))
	binary.BigEndian.PutUint16(tcp[0:2], 1051)
	binary.BigEndian.PutUint16(tcp[2:4], 60000)
	binary.BigEndian.PutUint32(tcp[4:8], 1234)
	tcp[12] = 5 << 4
	tcp[13] = TCPAck | TCPPsh
	copy(tcp[20:], payload)
	eth := ether(buildIPv4(ipA, [4]byte{169, 254, 9, 9}, ProtoTCP, 2, 0, false, tcp))
	// Apple pktap: header length 108, inner DLT = Ethernet.
	hdr := make([]byte, 108)
	binary.LittleEndian.PutUint32(hdr[0:4], 108)
	binary.LittleEndian.PutUint32(hdr[4:8], LinkEthernet)
	frame := append(hdr, eth...)
	pkts, err := ReadAll(bytes.NewReader(pcapng(LinkPKTAP2, frame, frame)))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkts) != 1 {
		t.Fatalf("pktap duplicate not suppressed: got %d packets", len(pkts))
	}
	p := pkts[0]
	if p.Proto != ProtoTCP || p.Seq != 1234 || p.Flags != TCPAck|TCPPsh || !bytes.Equal(p.Payload, payload) {
		t.Fatalf("bad tcp packet %+v", p)
	}
	if p.Time.Unix() != 1700000000 {
		t.Fatalf("bad time %v", p.Time)
	}
}

func TestFragmentReassembly(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 600) // 9600 bytes, like an NFS READ reply
	body := udpBody(2049, 800, payload)
	var frames [][]byte
	const chunk = 1480
	// Send fragments out of order to exercise reassembly.
	var order []int
	for off := 0; off < len(body); off += chunk {
		order = append(order, off)
	}
	order[0], order[len(order)-1] = order[len(order)-1], order[0]
	for _, off := range order {
		end := min(off+chunk, len(body))
		frames = append(frames, ether(buildIPv4(ipA, ipB, ProtoUDP, 77, off, end < len(body), body[off:end])))
	}
	pkts, err := ReadAll(bytes.NewReader(classicPcap(frames...)))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkts) != 1 {
		t.Fatalf("got %d packets, want 1 reassembled", len(pkts))
	}
	if !bytes.Equal(pkts[0].Payload, payload) {
		t.Fatalf("reassembled payload mismatch (len %d)", len(pkts[0].Payload))
	}
}

func TestGarbageInput(t *testing.T) {
	for _, in := range [][]byte{nil, {1, 2, 3}, bytes.Repeat([]byte{0xff}, 100)} {
		if _, err := ReadAll(bytes.NewReader(in)); err == nil {
			t.Errorf("expected error for %x", in)
		}
	}
	// Truncated record must return an error, not panic.
	good := classicPcap(ether(buildIPv4(ipA, ipB, ProtoUDP, 1, 0, false, udpBody(1, 2, []byte("x")))))
	if _, err := ReadAll(bytes.NewReader(good[:len(good)-3])); err == nil {
		t.Error("expected error for truncated capture")
	}
	// Malformed frames are skipped, not fatal.
	dec := NewDecoder()
	for _, f := range [][]byte{{}, {0x45}, ether([]byte{0x45, 0, 0, 10}), ether(buildIPv4(ipA, ipB, ProtoUDP, 1, 0, false, []byte{1}))} {
		dec.Decode(&Frame{LinkType: LinkEthernet, Data: f})
	}
}

// TestDysenteryCaptures parses Deep Symmetry's CDJ-2000NXS hardware captures
// when SLIPMAT_DYSENTERY_CAPTURES points at dysentery/doc/assets/captures.
func TestDysenteryCaptures(t *testing.T) {
	dir := os.Getenv("SLIPMAT_DYSENTERY_CAPTURES")
	if dir == "" {
		t.Skip("SLIPMAT_DYSENTERY_CAPTURES not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*", "run.pcap*"))
	if len(files) == 0 {
		t.Fatalf("no captures under %s", dir)
	}
	for _, fn := range files {
		f, err := os.Open(fn)
		if err != nil {
			t.Fatal(err)
		}
		rd, err := NewReader(f)
		if err != nil {
			t.Fatalf("%s: %v", fn, err)
		}
		dec := NewDecoder()
		n, udp, tcp := 0, 0, 0
		for {
			fr, err := rd.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("%s: %v", fn, err)
			}
			n++
			p, _ := dec.Decode(fr)
			if p == nil {
				continue
			}
			if p.Proto == ProtoUDP {
				udp++
			} else {
				tcp++
			}
		}
		f.Close()
		t.Logf("%s: %d frames, %d udp, %d tcp", filepath.Base(filepath.Dir(fn)), n, udp, tcp)
		if udp == 0 {
			t.Errorf("%s: no UDP decoded", fn)
		}
	}
}
