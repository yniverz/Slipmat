// SPDX-License-Identifier: GPL-3.0-or-later

package prolink

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// loadGolden reads a testdata .hex fixture (first line is a comment).
func loadGolden(t testing.TB, rel string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", rel))
	if err != nil {
		t.Fatal(err)
	}
	var hexs strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		hexs.WriteString(strings.Join(strings.Fields(line), ""))
	}
	b, err := hex.DecodeString(hexs.String())
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return b
}

func goldenFiles(t testing.TB) []string {
	files, _ := filepath.Glob(filepath.Join("testdata", "*", "*.hex"))
	if len(files) == 0 {
		t.Fatal("no golden fixtures")
	}
	return files
}

func portOf(path string) uint16 {
	p, _ := strconv.Atoi(strings.SplitN(filepath.Base(path), "-", 2)[0])
	return uint16(p)
}

func TestDescribeAllGolden(t *testing.T) {
	for _, f := range goldenFiles(t) {
		rel, _ := filepath.Rel("testdata", f)
		b := loadGolden(t, rel)
		d, err := Describe(portOf(f), b)
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		t.Logf("%s: %s", rel, d)
	}
}

func TestDecodeKeepAliveGolden(t *testing.T) {
	a, err := DecodeAnnounce(loadGolden(t, "dsc/50000-06-54.hex"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != KindKeepAlive || a.Name != "CDJ-2000nexus" || a.DeviceType != DeviceCDJ || a.Length != 54 {
		t.Fatalf("header: %+v", a)
	}
	if a.DeviceNumber != 1 || a.IP.String() != "169.254.103.172" || MACString(a.MAC) != "74:5e:1c:56:67:ac" || a.Peers != 1 {
		t.Fatalf("fields: %v", a)
	}
}

func TestDecodeClaimsGolden(t *testing.T) {
	c1, err := DecodeAnnounce(loadGolden(t, "dsc/50000-00-44.hex"))
	if err != nil || c1.Kind != KindClaim1 || c1.Counter != 1 || MACString(c1.MAC) != "74:5e:1c:56:67:ac" {
		t.Fatalf("claim-1: %v %v", c1, err)
	}
	c2, err := DecodeAnnounce(loadGolden(t, "dsc/50000-02-50.hex"))
	if err != nil || c2.Kind != KindClaim2 || c2.DeviceNumber != 1 || c2.AutoAssign != 0x02 || c2.IP.String() != "169.254.103.172" {
		t.Fatalf("claim-2: %v %v", c2, err)
	}
	c3, err := DecodeAnnounce(loadGolden(t, "dsc/50000-04-38.hex"))
	if err != nil || c3.Kind != KindClaim3 || c3.DeviceNumber != 1 || c3.Counter != 1 {
		t.Fatalf("claim-3: %v %v", c3, err)
	}
}

func TestDecodeStatusGolden(t *testing.T) {
	q, err := DecodeStatus(loadGolden(t, "dsc/50002-05-48.hex"))
	if err != nil || q.MediaQuery == nil {
		t.Fatalf("media query: %v %v", q, err)
	}
	if q.Device != 2 || q.MediaQuery.Target != 1 || q.MediaQuery.Slot != SlotUSB || q.MediaQuery.IP.String() != "169.254.202.84" {
		t.Fatalf("media query fields: %v", q)
	}
	r, err := DecodeStatus(loadGolden(t, "dsc/50002-06-192.hex"))
	if err != nil || r.MediaResponse == nil {
		t.Fatalf("media response: %v %v", r, err)
	}
	if m := r.MediaResponse; m.Player != 1 || m.Slot != SlotUSB || m.Tracks != 651 || m.TrackType != 1 {
		t.Fatalf("media response fields: %+v", m)
	}
	s, err := DecodeStatus(loadGolden(t, "dsc/50002-0a-284.hex"))
	if err != nil || s.CDJ == nil {
		t.Fatalf("cdj status: %v %v", s, err)
	}
	if s.Device != 2 || s.CDJ.Firmware != "1.44" || s.CDJ.PacketSize != 0x11c {
		t.Fatalf("cdj status fields: %v", s)
	}
}

func TestDecodeBeatGolden(t *testing.T) {
	p, err := DecodeBeat(loadGolden(t, "dsc/50001-28-96.hex"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindBeat || p.Device != 1 || p.BPM != 132.01 || p.BeatInBar != 1 {
		t.Fatalf("beat: %v", p)
	}
}

// TestNoPanicOnTruncation feeds every prefix and some corruptions of every
// golden packet to the decoders; they must return errors, never panic.
func TestNoPanicOnTruncation(t *testing.T) {
	for _, f := range goldenFiles(t) {
		rel, _ := filepath.Rel("testdata", f)
		b := loadGolden(t, rel)
		for port := uint16(PortAnnounce); port <= PortTouchAudio; port++ {
			for n := 0; n <= len(b); n++ {
				Describe(port, b[:n])
			}
			c := append([]byte(nil), b...)
			for i := HeaderLen; i < len(c); i++ {
				c[i] ^= 0xff
			}
			Describe(port, c)
		}
	}
}

func FuzzDescribe(f *testing.F) {
	for _, path := range goldenFiles(f) {
		rel, _ := filepath.Rel("testdata", path)
		f.Add(portOf(path), loadGolden(f, rel))
	}
	f.Fuzz(func(t *testing.T, port uint16, b []byte) {
		Describe(port, b)
	})
}
