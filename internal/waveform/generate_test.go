// SPDX-License-Identifier: GPL-3.0-or-later

package waveform

import (
	"bytes"
	"encoding/hex"
	"math"
	"testing"
	"time"
)

func sine(freq, amp float64, seconds float64) []float32 {
	n := int(seconds * SampleRate)
	s := make([]float32, n)
	for i := range s {
		s[i] = float32(amp * math.Sin(2*math.Pi*freq*float64(i)/SampleRate))
	}
	return s
}

func avg(b []byte, stride, off int) float64 {
	var sum, n float64
	for i := off; i < len(b); i += stride {
		sum += float64(b[i])
		n++
	}
	return sum / n
}

func TestSilenceIsZero(t *testing.T) {
	r := Generate(make([]float32, SampleRate*2))
	for name, b := range map[string][]byte{"PWAV": r.PWAV, "PWV2": r.PWV2, "PWV3": r.PWV3, "PWV5": r.PWV5, "PWV6": r.PWV6, "PWV7": r.PWV7} {
		if bytes.Count(b, []byte{0}) != len(b) {
			t.Errorf("%s not all zero for silence", name)
		}
	}
	if len(r.PWV3) != 300 { // 2 s × 150
		t.Errorf("PWV3 entries %d", len(r.PWV3))
	}
}

func TestBandsFollowFrequency(t *testing.T) {
	bass := Generate(sine(60, 0.5, 4))
	treble := Generate(sine(8000, 0.5, 4))
	// PWV7 order is low, mid, high [RB7 calibration].
	if !(avg(bass.PWV7, 3, 0) > 3*avg(bass.PWV7, 3, 2)) {
		t.Errorf("bass tone: low %.1f vs high %.1f", avg(bass.PWV7, 3, 0), avg(bass.PWV7, 3, 2))
	}
	if !(avg(treble.PWV7, 3, 2) > 3*avg(treble.PWV7, 3, 0)) {
		t.Errorf("treble tone: high %.1f vs low %.1f", avg(treble.PWV7, 3, 2), avg(treble.PWV7, 3, 0))
	}
	// PWV5 colour: bass is red, treble is blue.
	red := func(r *Result) float64 { return float64(r.PWV5[200] >> 5) }
	blue := func(r *Result) float64 { return float64((uint16(r.PWV5[200])<<8 | uint16(r.PWV5[201])) >> 7 & 7) }
	if red(bass) <= blue(bass) || blue(treble) <= red(treble) {
		t.Errorf("colours: bass R%.0f B%.0f, treble R%.0f B%.0f", red(bass), blue(bass), red(treble), blue(treble))
	}
	// Whiteness rises with treble.
	if bass.PWV3[300]>>5 >= treble.PWV3[300]>>5 {
		t.Errorf("whiteness: bass %d treble %d", bass.PWV3[300]>>5, treble.PWV3[300]>>5)
	}
}

func TestClampsAndHeaders(t *testing.T) {
	sq := make([]float32, SampleRate*3)
	for i := range sq {
		if (i/50)%2 == 0 {
			sq[i] = 1
		} else {
			sq[i] = -1
		}
	}
	r := Generate(sq)
	for name, b := range map[string][]byte{"PWV4": r.PWV4, "PWV6": r.PWV6, "PWV7": r.PWV7} {
		for i, v := range b {
			if name == "PWV4" && i%6 == 1 {
				continue // d1 is not a level
			}
			if v > 127 {
				t.Fatalf("%s byte %d = %d > 127", name, i, v)
			}
		}
	}
	a := r.Analysis()
	want := map[string]string{
		"PWAV": "5057415600000014000001a40000019000010000",
		"PWV2": "50575632000000140000007800000064000100000",
		"PWV4": "505756340000001800001c3800000006000004b000000000",
		"PWV6": "505756360000001400000e2400000003000004b0",
	}
	for tag, h := range want {
		s, ok := a.Any(tag)
		if !ok {
			t.Fatalf("%s missing", tag)
		}
		if got := hex.EncodeToString(s.Raw[:s.Header]); got != h[:len(got)] {
			t.Errorf("%s header %s, rekordbox %s", tag, got, h)
		}
	}
	p5, _ := a.Section("EXT", "PWV5")
	if hex.EncodeToString(p5.Raw[12:24]) != "00000002000001c200960305" { // 450 entries for 3 s
		t.Errorf("PWV5 header %x", p5.Raw[:24])
	}
}

func TestStoreRoundTrip(t *testing.T) {
	st := NewStore(t.TempDir(), 1, nil)
	j := Job{TrackID: 42, Path: "/x.mp3", Size: 1000, ModTime: time.Unix(1700000000, 5)}
	r := Generate(sine(440, 0.3, 2))
	if err := st.save(j, r); err != nil {
		t.Fatal(err)
	}
	got, err := st.load(j)
	if err != nil || !bytes.Equal(got.PWV7, r.PWV7) || !bytes.Equal(got.PWV4, r.PWV4) {
		t.Fatalf("round trip: %v", err)
	}
	j2 := j
	j2.Size++
	if _, err := st.load(j2); err == nil {
		t.Fatal("stale entry accepted for a changed file")
	}
	if cached, queued := st.Add([]Job{j}); cached != 1 || queued != 0 {
		t.Fatalf("add: cached %d queued %d", cached, queued)
	}
	if st.Get(42) == nil {
		t.Fatal("cached waveform not returned")
	}
	// The file changed: the old waveform must not be served any more.
	st2 := NewStore(st.Dir, 1, nil)
	if cached, queued := st2.Add([]Job{j2}); cached != 0 || queued != 1 {
		t.Fatalf("changed file: cached %d queued %d", cached, queued)
	}
	if st2.Get(42) != nil {
		t.Fatal("stale waveform served for a changed file")
	}
}
