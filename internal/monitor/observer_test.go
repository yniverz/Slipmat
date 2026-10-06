// SPDX-License-Identifier: GPL-3.0-or-later

package monitor

import (
	"bytes"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yniverz/slipmat/internal/logx"
	"github.com/yniverz/slipmat/internal/prolink"
)

func newTestObserver(t *testing.T, verbosity int) (*Observer, *bytes.Buffer) {
	var buf bytes.Buffer
	logx.Setup(&buf, verbosity)
	return NewObserver(logx.Component("link")), &buf
}

var (
	cdjAddr = netip.MustParseAddrPort("192.168.1.50:50000")
	bcast   = netip.MustParseAddrPort("192.168.1.255:50000")
)

func keepAlive(dev uint8, peers uint8) []byte {
	return prolink.EncodeRekordboxKeepAlive(prolink.Identity{Name: "CDJ-3000", DeviceNumber: dev, IP: cdjAddr.Addr()}, peers)
}

func TestPeerJoinDedupeAndExpire(t *testing.T) {
	o, buf := newTestObserver(t, 0)
	t0 := time.Unix(1000, 0)
	o.Packet(t0, cdjAddr, bcast, keepAlive(1, 1))
	o.Packet(t0.Add(time.Second), cdjAddr, bcast, keepAlive(1, 2)) // only peer count changed
	out := buf.String()
	if strings.Count(out, "device joined") != 1 {
		t.Fatalf("want one join:\n%s", out)
	}
	if strings.Count(out, "keep-alive") != 1 {
		t.Fatalf("unchanged keep-alive should be deduplicated:\n%s", out)
	}
	if p := o.Peers(); len(p) != 1 || p[0].Device != 1 || p[0].Name != "CDJ-3000" {
		t.Fatalf("peers: %+v", p)
	}
	o.Expire(t0.Add(30 * time.Second))
	if len(o.Peers()) != 0 || !strings.Contains(buf.String(), "device left") {
		t.Fatalf("peer not expired:\n%s", buf.String())
	}
}

func TestMalformedAndUnknownLogged(t *testing.T) {
	o, buf := newTestObserver(t, 0)
	o.Packet(time.Now(), cdjAddr, bcast, []byte{1, 2, 3})
	unknown := append(append([]byte{}, prolink.Magic[:]...), 0x77)
	unknown = append(unknown, make([]byte, 0x30)...)
	st := netip.AddrPortFrom(cdjAddr.Addr(), prolink.PortStatus)
	o.Packet(time.Now(), cdjAddr, st, unknown)
	o.Packet(time.Now(), cdjAddr, st, unknown)
	out := buf.String()
	if !strings.Contains(out, "malformed packet") {
		t.Fatalf("malformed packet not reported:\n%s", out)
	}
	if strings.Count(out, "first sighting") != 1 {
		t.Fatalf("unknown kind should warn once:\n%s", out)
	}
}

func TestDecodeDysenteryCapture(t *testing.T) {
	dir := os.Getenv("SLIPMAT_DYSENTERY_CAPTURES")
	if dir == "" {
		t.Skip("SLIPMAT_DYSENTERY_CAPTURES not set")
	}
	var buf bytes.Buffer
	logx.Setup(&buf, 0)
	d := &CaptureDecoder{Log: logx.Component("decode"), Obs: NewObserver(logx.Component("link"))}
	matches, _ := filepath.Glob(filepath.Join(dir, "S4b-media-insert", "run.pcap*"))
	if len(matches) == 0 {
		t.Fatal("S4b-media-insert capture not found")
	}
	if err := d.DecodeFile(matches[0]); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"device joined", "rpc call portmap v2 GETPORT for mount", "rpc reply mount MNT", "media-response", "2 device(s) on the link"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in decode output", want)
		}
	}
}
