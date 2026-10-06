// SPDX-License-Identifier: GPL-3.0-or-later

package rpc

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var h strings.Builder
	for _, l := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(l, "#") {
			h.WriteString(strings.TrimSpace(l))
		}
	}
	b, err := hex.DecodeString(h.String())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func call(t *testing.T, name string) *Call {
	t.Helper()
	c, err := ParseCall(fixture(t, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return c
}

func rb7Mount(ready bool) *Mount {
	return &Mount{Exports: func() []Export {
		if !ready {
			return nil
		}
		return []Export{{Dir: "/", Groups: []string{"192.168.1.103/255.255.255.0"}}}
	}}
}

// Replay the CDJ-3000's calls from the rekordbox 7 capture and require
// rekordbox's replies byte for byte.
func TestReplayRekordbox7(t *testing.T) {
	pm := &Portmap{}
	pm.Set(ProgMount, 1, ProtoUDP, 35008)
	pm.Set(ProgNFS, 2, ProtoUDP, 2049)

	c := call(t, "cdj-50111-76.hex")
	if c.Prog != ProgPortmap || c.Proc != 3 {
		t.Fatalf("unexpected call %v", c)
	}
	if got, want := pm.Handle(c), fixture(t, "rb-from50111-28.hex"); !bytes.Equal(got, want) {
		t.Errorf("GETPORT mount\n got % x\nwant % x", got, want)
	}

	exp := call(t, "cdj-35008-60.hex")
	if got, want := rb7Mount(true).Handle(exp), fixture(t, "rb-from35008-80.hex"); !bytes.Equal(got, want) {
		t.Errorf("EXPORT (ready)\n got % x\nwant % x", got, want)
	}
	if got, want := rb7Mount(false).Handle(exp), fixture(t, "rb-from35008-28.hex"); !bytes.Equal(got, want) {
		t.Errorf("EXPORT (not ready)\n got % x\nwant % x", got, want)
	}

	var events []string
	m := rb7Mount(true)
	m.Event = func(proc, path string) { events = append(events, proc+" "+path) }
	mnt := call(t, "cdj-35008-68.hex")
	if got, want := m.Handle(mnt), fixture(t, "rb-from35008-60.hex"); !bytes.Equal(got, want) {
		t.Errorf("MNT\n got % x\nwant % x", got, want)
	}
	if len(events) != 1 || events[0] != "MNT /" {
		t.Errorf("events %v", events)
	}
}

func TestErrorReplies(t *testing.T) {
	m := rb7Mount(true)
	mnt := call(t, "cdj-35008-68.hex")

	bad := *mnt
	bad.Args = []byte{0, 0, 0, 9, '/'}
	if r := m.Handle(&bad); NewReader(r[20:]).Uint32() != AcceptGarbageArgs {
		t.Error("truncated MNT path must give GARBAGE_ARGS")
	}
	other := *mnt
	var w Writer
	w.String("/Volumes/x")
	other.Args = w.B
	r := NewReader(m.Handle(&other)[24:])
	if r.Uint32() != MountNoEnt {
		t.Error("MNT of an unexported path must fail")
	}
	v3 := *mnt
	v3.Vers = 3
	if r := m.Handle(&v3); NewReader(r[20:]).Uint32() != AcceptProgMismatch {
		t.Error("MOUNT v3 must get PROG_MISMATCH")
	}
	unknown := *mnt
	unknown.Proc = 42
	if r := m.Handle(&unknown); NewReader(r[20:]).Uint32() != AcceptProcUnavail {
		t.Error("unknown proc must get PROC_UNAVAIL")
	}
}

func TestParseCallRejectsGarbage(t *testing.T) {
	good := fixture(t, "cdj-50111-76.hex")
	for n := 0; n < len(good)-12; n++ { // any truncation inside the header fails cleanly
		if _, err := ParseCall(good[:n]); err == nil && n < 40 {
			t.Errorf("truncated call (%d bytes) parsed", n)
		}
	}
	reply := fixture(t, "rb-from50111-28.hex")
	if _, err := ParseCall(reply); err != ErrNotCall {
		t.Errorf("reply parsed as call: %v", err)
	}
}

func FuzzHandlers(f *testing.F) {
	for _, n := range []string{"cdj-50111-76.hex", "cdj-35008-60.hex", "cdj-35008-68.hex"} {
		b, _ := os.ReadFile(filepath.Join("testdata", n))
		lines := strings.Split(string(b), "\n")
		raw, _ := hex.DecodeString(strings.TrimSpace(lines[1]))
		f.Add(raw)
	}
	pm := &Portmap{}
	m := rb7Mount(true)
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := ParseCall(b)
		if err != nil {
			return
		}
		pm.Handle(c)
		m.Handle(c)
	})
}
