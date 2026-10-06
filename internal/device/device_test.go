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

type bcastLog struct {
	mu  sync.Mutex
	out []sent
}

func newTestDevice(t *testing.T) (*Device, *bcastLog, *fakeConn) {
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
	bl := &bcastLog{}
	d.bcast = func(port int, pkt []byte) error {
		bl.mu.Lock()
		defer bl.mu.Unlock()
		bl.out = append(bl.out, sent{netip.AddrPortFrom(d.cfg.Interface.Broadcast, uint16(port)), append([]byte(nil), pkt...)})
		return nil
	}
	d.after = func(_ time.Duration, f func()) { f() }
	st := &fakeConn{port: prolink.PortStatus}
	d.status = st
	return d, bl, st
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
		t.Fatalf("want only 0x11 (no 0x16 after hello), sent %d packets", len(out))
	}
	for _, o := range out {
		if o.to != netip.AddrPortFrom(cdjIP, prolink.PortStatus) {
			t.Fatalf("reply must go to the CDJ's port 50002, went to %s", o.to)
		}
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
	if len(out) != 1 {
		t.Fatalf("want one media response (rekordbox 7 sends one), got %d", len(out))
	}
	s := decodeOne(t, out[0])
	if s.MediaResponse == nil || s.MediaResponse.Player != 17 || s.MediaResponse.Slot != prolink.SlotRekordbox || s.MediaResponse.Tracks != 3 {
		t.Fatalf("bad media response: %v", s)
	}
}

func TestLinkPingAnsweredWithActivation(t *testing.T) {
	d, _, st := newTestDevice(t)
	ping := cdjPacket(prolink.KindLinkPing, 0x28, map[int]byte{0x24: 2, 0x25: 4, 0x27: 0xc0})
	for i := 0; i < 2; i++ {
		d.handle(in(prolink.PortStatus, ping))
		out := st.take()
		if len(out) != 1 || decodeOne(t, out[0]).Kind != prolink.KindLinkActivate {
			t.Fatalf("ping %d must be answered with 0x47, got %v", i+1, out)
		}
	}
}

func TestExportsFollowAvailability(t *testing.T) {
	d, _, _ := newTestDevice(t)
	if len(d.Exports()) != 0 {
		t.Fatal("exports must be empty before we have claimed")
	}
	d.available.Store(true)
	e := d.Exports()
	if len(e) != 1 || e[0].Dir != "/" || len(e[0].Groups) != 1 || e[0].Groups[0] != "192.168.1.103/255.255.255.0" {
		t.Fatalf("exports %+v", e)
	}
}

func TestQuitSequence(t *testing.T) {
	d, bl, st := newTestDevice(t)
	d.available.Store(true)
	d.handle(in(prolink.PortStatus, cdjPacket(prolink.KindRBHelloQuery, 0x2c, nil)))
	st.take()
	d.quit(make(chan inbound))
	if d.available.Load() {
		t.Fatal("still available after quit")
	}
	out := st.take()
	if len(out) != 1 || decodeOne(t, out[0]).Kind != prolink.KindRBStatus {
		t.Fatalf("quit must send 0x16 to each player, got %v", out)
	}
	last := bl.out[len(bl.out)-1]
	a, err := prolink.DecodeAnnounce(last.pkt)
	if err != nil || a.Kind != prolink.KindConflict || a.DeviceNumber != 17 || a.IP != d.id.IP {
		t.Fatalf("quit must end with a leave broadcast, got %v %v", a, err)
	}
}

func TestMalformedPacketsIgnored(t *testing.T) {
	d, bl, st := newTestDevice(t)
	for _, b := range [][]byte{nil, {1, 2, 3}, []byte("Qspt1WmJOL"), append(append([]byte{}, prolink.Magic[:]...), 0x05, 0, 0)} {
		d.handle(in(prolink.PortStatus, b))
		d.handle(in(prolink.PortAnnounce, b))
	}
	if len(st.take())+len(bl.out) != 0 {
		t.Fatal("replied to malformed input")
	}
}

func TestClaimSequence(t *testing.T) {
	d, bl, _ := newTestDevice(t)
	start := time.Now()
	d.claim(t.Context())
	if want := 3 + claimRounds*len(rekordboxSlots); len(bl.out) != want {
		t.Fatalf("sent %d claim packets, want %d", len(bl.out), want)
	}
	if el := time.Since(start); el < 3*time.Second {
		t.Fatalf("claims went out too fast (%v); rekordbox spaces them 100 ms apart", el)
	}
	for i, s := range bl.out {
		if s.to != netip.MustParseAddrPort("192.168.1.255:50000") {
			t.Fatalf("claim sent to %s", s.to)
		}
		a, err := prolink.DecodeAnnounce(s.pkt)
		if err != nil || a.Version != prolink.RekordboxVersion {
			t.Fatalf("bad claim %v %v", a, err)
		}
		if i >= 3 && a.DeviceNumber != rekordboxSlots[(i-3)%len(rekordboxSlots)] {
			t.Fatalf("claim %d for device %d", i, a.DeviceNumber)
		}
	}
}
