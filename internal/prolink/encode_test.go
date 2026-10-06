// SPDX-License-Identifier: GPL-3.0-or-later

package prolink

import (
	"bytes"
	"net/netip"
	"testing"
)

var testID = Identity{
	Name:         "rekordbox",
	DeviceNumber: 0x17,
	MAC:          [6]byte{0x18, 0x3e, 0xef, 0xda, 0x5b, 0xca},
	IP:           netip.MustParseAddr("192.168.2.11"),
}

// beat-link's VirtualRekordbox keep-alive template, itself taken from a
// capture of real rekordbox. [BL]
var blKeepAlive = []byte{
	0x51, 0x73, 0x70, 0x74, 0x31, 0x57, 0x6d, 0x4a, 0x4f, 0x4c, 0x06, 0x00, 0x72, 0x65, 0x6b, 0x6f, 0x72,
	0x64, 0x62, 0x6f, 0x78, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x03,
	0x00, 0x36, 0x17, 0x01, 0x18, 0x3e, 0xef, 0xda, 0x5b, 0xca, 0xc0, 0xa8,
	0x02, 0x0b, 0x04, 0x01, 0x00, 0x00, 0x04, 0x08,
}

func TestRekordboxKeepAliveMatchesBeatLink(t *testing.T) {
	got := EncodeRekordboxKeepAlive(testID, 4)
	if !bytes.Equal(got, blKeepAlive) {
		t.Fatalf("keep-alive mismatch\n got % x\nwant % x", got, blKeepAlive)
	}
	a, err := DecodeAnnounce(got)
	if err != nil {
		t.Fatal(err)
	}
	if a.DeviceType != DeviceRekordbox || a.DeviceNumber != 0x17 || a.IP != testID.IP || a.MAC != testID.MAC || a.Peers != 4 {
		t.Fatalf("round trip: %v", a)
	}
}

func TestRBHelloMatchesBeatLinkHeader(t *testing.T) {
	// First 0x28 bytes of beat-link's 0x11 template (device 0x17). [BL]
	want := []byte{
		0x51, 0x73, 0x70, 0x74, 0x31, 0x57, 0x6d, 0x4a, 0x4f, 0x4c, 0x11, 0x72, 0x65, 0x6b, 0x6f, 0x72,
		0x64, 0x62, 0x6f, 0x78, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
		0x01, 0x17, 0x01, 0x04, 0x17, 0x01, 0x00, 0x00,
	}
	got := EncodeRBHello("rekordbox", 0x17, "macbook pro")
	if len(got) != 0x128 {
		t.Fatalf("len %d", len(got))
	}
	if !bytes.Equal(got[:0x28], want) {
		t.Fatalf("header mismatch\n got % x\nwant % x", got[:0x28], want)
	}
	s, err := DecodeStatus(got)
	if err != nil || s.HostName != "macbook pro" || s.Device != 0x17 {
		t.Fatalf("round trip: %v %v", s, err)
	}
}

func TestClaimEncodersRoundTrip(t *testing.T) {
	a, err := DecodeAnnounce(EncodeRekordboxClaim1(testID, 2))
	if err != nil || a.Kind != KindClaim1 || a.Counter != 2 || a.Class != RekordboxClass || a.MAC != testID.MAC || a.Length != 0x2c {
		t.Fatalf("claim-1: %v %v", a, err)
	}
	a, err = DecodeAnnounce(EncodeRekordboxClaim2(testID, 17, 3))
	if err != nil || a.Kind != KindClaim2 || a.DeviceNumber != 17 || a.Counter != 3 || a.IP != testID.IP || a.AutoAssign != 1 || a.Length != 0x32 {
		t.Fatalf("claim-2: %v %v", a, err)
	}
}

func TestStatusEncodersRoundTrip(t *testing.T) {
	m := MediaInfo{Name: "Slipmat Library", Tracks: 1234, Playlists: 7, Settings: true}
	s, err := DecodeStatus(EncodeMediaResponse("Slipmat", 17, SlotRekordbox, m))
	if err != nil || s.MediaResponse == nil {
		t.Fatalf("media response: %v %v", s, err)
	}
	if r := s.MediaResponse; r.Player != 17 || r.Slot != SlotRekordbox || r.Name != m.Name || r.Tracks != 1234 || r.Playlists != 7 || r.Settings != 1 {
		t.Fatalf("media response fields: %+v", r)
	}
	if s.Length != 0x9c || s.Name != "Slipmat" {
		t.Fatalf("media response header: %v", s)
	}

	s, err = DecodeStatus(EncodeLinkActivate("Slipmat", 17, SlotRekordbox, DefaultDevSetting))
	if err != nil || s.Kind != KindLinkActivate || s.Slot != SlotRekordbox || len(s.Payload) != 0x24 {
		t.Fatalf("link activate: %v %v", s, err)
	}
	for _, b := range [][]byte{EncodeRBStatus("Slipmat", 17), EncodeRBMixerStatus("Slipmat", 17)} {
		s, err := DecodeStatus(b)
		if err != nil || s.Device != 17 || s.Name != "Slipmat" {
			t.Fatalf("status: %v %v", s, err)
		}
	}
}

func TestLongNamesTruncate(t *testing.T) {
	long := "A very long device name that exceeds twenty bytes"
	a, _ := DecodeAnnounce(EncodeRekordboxKeepAlive(Identity{Name: long, IP: testID.IP}, 1))
	if a.Name != long[:20] {
		t.Fatalf("name %q", a.Name)
	}
	host := string(bytes.Repeat([]byte("x"), 400))
	s, _ := DecodeStatus(EncodeRBHello("x", 1, host))
	if len(s.HostName) != 127 {
		t.Fatalf("host name len %d", len(s.HostName))
	}
}
