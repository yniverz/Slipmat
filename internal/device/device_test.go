// SPDX-License-Identifier: GPL-3.0-or-later

package device

import (
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/yniverz/slipmat/internal/logx"
	"github.com/yniverz/slipmat/internal/netif"
	"github.com/yniverz/slipmat/internal/prolink"
)

type sent struct {
	to  netip.AddrPort
	pkt []byte
}

type fakeConn struct {
	port int
	mu   sync.Mutex
	out  []sent
}

func (f *fakeConn) WriteToUDPAddrPort(b []byte, a netip.AddrPort) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.out = append(f.out, sent{a, append([]byte(nil), b...)})
	return len(b), nil
}

func (f *fakeConn) LocalAddr() net.Addr { return &net.UDPAddr{Port: f.port} }

func (f *fakeConn) take() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.out
	f.out = nil
	return o
}

var cdjIP = netip.MustParseAddr("192.168.1.50")

func newTestDevice(t *testing.T) (*Device, *fakeConn, *fakeConn) {
	t.Helper()
	logx.Setup(io.Discard, 0)
	d, err := New(Config{
		Interface: netif.Interface{
			Name:      "en13",
			Prefix:    netip.MustParsePrefix("192.168.1.103/24"),
			Broadcast: netip.MustParseAddr("192.168.1.255"),
			MAC:       [6]byte{0x34, 0x99, 0x71, 0xeb, 0x31, 0xe5},
		},
		Host:  "Slipmat",
		Media: prolink.MediaInfo{Name: "Slipmat", Tracks: 3},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ann, st := &fakeConn{port: prolink.PortAnnounce}, &fakeConn{port: prolink.PortStatus}
	d.announce, d.status, d.beat = ann, st, &fakeConn{port: prolink.PortBeat}
	return d, ann, st
}

// cdjPacket builds a port-50002 packet as a CDJ (device 2) would send it.
func cdjPacket(kind uint8, n int, payload map[int]byte) []byte {
	b := make([]byte, n)
	copy(b, prolink.Magic[:])
	b[0x0a] = kind
	copy(b[0x0b:], "CDJ-3000")
	b[0x1f], b[0x20], b[0x21] = 1, 0, 2
	b[0x23] = byte(n - 0x24)
	for off, v := range payload {
		b[off] = v
	}
	return b
}

func in(port int, b []byte) inbound {
	return inbound{t: time.Now(), port: port, src: netip.AddrPortFrom(cdjIP, 41234), b: b}
}

func decodeOne(t *testing.T, s sent) *prolink.Status {
	t.Helper()
	st, err := prolink.DecodeStatus(s.pkt)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestHelloQueryAnsweredWith0x11(t *testing.T) {
	d, _, st := newTestDevice(t)
	d.handle(in(prolink.PortStatus, cdjPacket(prolink.KindRBHelloQuery, 0x2c, nil)))
	out := st.take()
	if len(out) != 1 {
		t.Fatalf("sent %d packets", len(out))
	}
	if out[0].to != netip.AddrPortFrom(cdjIP, prolink.PortStatus) {
		t.Fatalf("reply must go to the CDJ's port 50002, went to %s", out[0].to)
	}
	if s := decodeOne(t, out[0]); s.Kind != prolink.KindRBHello || s.HostName != "Slipmat" || s.Device != 17 {
		t.Fatalf("bad hello: %v", s)
	}
}

func TestMediaQuery(t *testing.T) {
	d, _, st := newTestDevice(t)
	q := func(target byte) []byte {
		return cdjPacket(prolink.KindMediaQuery, 0x30, map[int]byte{0x24: 192, 0x25: 168, 0x26: 1, 0x27: 50, 0x2b: target, 0x2f: byte(prolink.SlotRekordbox)})
	}
	d.handle(in(prolink.PortStatus, q(3))) // another player's media
	if out := st.take(); len(out) != 0 {
		t.Fatalf("answered a query for another device: %d packets", len(out))
	}
	d.handle(in(prolink.PortStatus, q(17)))
	out := st.take()
	if len(out) != 2 {
		t.Fatalf("want media response sent twice, got %d", len(out))
	}
	s := decodeOne(t, out[0])
	if s.MediaResponse == nil || s.MediaResponse.Player != 17 || s.MediaResponse.Slot != prolink.SlotRekordbox || s.MediaResponse.Tracks != 3 {
		t.Fatalf("bad media response: %v", s)
	}
}

func TestLinkPingSequence(t *testing.T) {
	d, _, st := newTestDevice(t)
	ping := cdjPacket(prolink.KindLinkPing, 0x30, nil)
	if d.isLinked() {
		t.Fatal("linked before any ping")
	}
	d.handle(in(prolink.PortStatus, ping))
	if out := st.take(); len(out) != 1 || decodeOne(t, out[0]).Kind != prolink.KindRBStatus {
		t.Fatalf("first ping must be answered with 0x16, got %v", out)
	}
	if !d.isLinked() {
		t.Fatal("not linked after first ping")
	}
	for i := 0; i < 3; i++ {
		d.handle(in(prolink.PortStatus, ping))
		out := st.take()
		if len(out) != 1 || decodeOne(t, out[0]).Kind != prolink.KindLinkActivate {
			t.Fatalf("ping %d must be answered with 0x47, got %v", i+2, out)
		}
	}
}

func TestMalformedPacketsIgnored(t *testing.T) {
	d, ann, st := newTestDevice(t)
	for _, b := range [][]byte{nil, {1, 2, 3}, []byte("Qspt1WmJOL"), append(append([]byte{}, prolink.Magic[:]...), 0x05, 0, 0)} {
		d.handle(in(prolink.PortStatus, b))
		d.handle(in(prolink.PortAnnounce, b))
	}
	if len(st.take())+len(ann.take()) != 0 {
		t.Fatal("replied to malformed input")
	}
}

func TestClaimSequence(t *testing.T) {
	d, ann, _ := newTestDevice(t)
	if err := d.claim(t.Context()); err != nil {
		t.Fatal(err)
	}
	out := ann.take()
	if want := 3*2 + 6*len(rekordboxSlots)*2; len(out) != want {
		t.Fatalf("sent %d claim packets, want %d", len(out), want)
	}
	for _, s := range out {
		if s.to != netip.MustParseAddrPort("192.168.1.255:50000") {
			t.Fatalf("claim sent to %s", s.to)
		}
		a, err := prolink.DecodeAnnounce(s.pkt)
		if err != nil || a.DeviceType != prolink.DeviceRekordbox {
			t.Fatalf("bad claim %v %v", a, err)
		}
	}
}
