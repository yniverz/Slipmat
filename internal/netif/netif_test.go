// SPDX-License-Identifier: GPL-3.0-or-later

package netif

import (
	"bytes"
	"net/netip"
	"strings"
	"testing"
)

func TestBroadcastOf(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.103/24": "192.168.1.255",
		"169.254.7.9/16":   "169.254.255.255",
		"10.0.0.5/8":       "10.255.255.255",
		"10.1.2.3/30":      "10.1.2.3",
	} {
		if got := broadcastOf(netip.MustParsePrefix(in)).String(); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}

var sample = []Interface{
	{Name: "en13", Label: "USB 10/100/1G/2.5G LAN", Prefix: netip.MustParsePrefix("192.168.1.103/24")},
	{Name: "en0", Label: "Wi-Fi", Prefix: netip.MustParsePrefix("10.1.1.34/24"), Wireless: true},
}

func TestFind(t *testing.T) {
	for spec, want := range map[string]string{"en13": "en13", "wi-fi": "en0", "USB": "en13", "10.1.1.34": "en0"} {
		got, err := Find(sample, spec)
		if err != nil || got.Name != want {
			t.Errorf("Find(%q) = %v, %v; want %s", spec, got.Name, err, want)
		}
	}
	if _, err := Find(sample, "en99"); err == nil {
		t.Error("expected error for unknown interface")
	}
}

func TestMenu(t *testing.T) {
	var out bytes.Buffer
	seen := map[string][]Sighting{"en13": {{Name: "CDJ-3000", Device: 1, IP: netip.MustParseAddr("192.168.1.50")}}}
	got, err := Menu(strings.NewReader("x\n7\n1\n"), &out, sample, seen)
	if err != nil || got.Name != "en13" {
		t.Fatalf("got %v %v", got, err)
	}
	if !strings.Contains(out.String(), "CDJ-3000 #1") || !strings.Contains(out.String(), "wireless") {
		t.Fatalf("menu output missing details:\n%s", out.String())
	}
	if _, err := Menu(strings.NewReader(""), &out, sample, nil); err == nil {
		t.Fatal("expected error on EOF")
	}
}

func TestListDoesNotFail(t *testing.T) {
	ifs, err := List()
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range ifs {
		if !i.Prefix.Addr().Is4() || i.Broadcast == (netip.Addr{}) {
			t.Errorf("bad interface %+v", i)
		}
		t.Logf("%s wireless=%v", i, i.Wireless)
	}
}
