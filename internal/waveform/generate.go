// SPDX-License-Identifier: GPL-3.0-or-later

package waveform

import (
	"encoding/binary"
	"math"

	"github.com/yniverz/slipmat/internal/anlz"
)

// Calibration. Each byte is clamp(round(k * feature^p)); features and
// constants were fitted against rekordbox 7's analysis of 49 tracks and
// validated on held-out tracks (calibrate_test.go). Holdout correlation
// with rekordbox's own bytes is noted per format.
//
// Feature terms: "peak"/"rms" of a band over the column; "share" =
// band RMS / sum of the three band RMS; "rel" = band RMS / loudest band RMS.
// Bands come from 4th-order Butterworth crossovers (lowHz/highHz).
const (
	detailRate = 150 // entries per second
	detailSize = SampleRate / detailRate

	// Rekordbox never writes values above 127 in PWV4/PWV6/PWV7, and some
	// players misbehave on larger ones (an XDJ-AZ restarts on PWV4 > 127
	// [VN]), so those are clamped at 127.
	maxBand = 127
)

// Result holds the section bodies of a generated analysis.
type Result struct {
	PWAV, PWV2, PWV3, PWV4, PWV5, PWV6, PWV7 []byte
}

// accumulators for one signal at one resolution.
type acc struct {
	sumsq, peak []float64
	cnt         []int
}

func newAcc(n int) *acc {
	return &acc{sumsq: make([]float64, n), peak: make([]float64, n), cnt: make([]int, n)}
}

func (a *acc) add(c int, v float64) {
	if v < 0 {
		v = -v
	}
	a.sumsq[c] += v * v
	a.cnt[c]++
	if v > a.peak[c] {
		a.peak[c] = v
	}
}

func (a *acc) rms(c int) float64 {
	if a.cnt[c] == 0 {
		return 0
	}
	return math.Sqrt(a.sumsq[c] / float64(a.cnt[c]))
}

// filter state for a 4th-order (two cascaded biquads) stage.
type stage struct {
	q                  biquad
	x1, x2, y1, y2     float64
	x1b, x2b, y1b, y2b float64
}

func (s *stage) step(x float64) float64 {
	y := s.q.b0*x + s.q.b1*s.x1 + s.q.b2*s.x2 - s.q.a1*s.y1 - s.q.a2*s.y2
	s.x2, s.x1, s.y2, s.y1 = s.x1, x, s.y1, y
	z := s.q.b0*y + s.q.b1*s.x1b + s.q.b2*s.x2b - s.q.a1*s.y1b - s.q.a2*s.y2b
	s.x2b, s.x1b, s.y2b, s.y1b = s.x1b, y, s.y1b, z
	return z
}

// envelope is a peak follower with instant attack and exponential release.
type envelope struct{ v, decay float64 }

func newEnv(release float64) *envelope {
	return &envelope{decay: math.Exp(-1 / (release * SampleRate))}
}

func (e *envelope) step(x float64) float64 {
	if x < 0 {
		x = -x
	}
	if x > e.v {
		e.v = x
	} else {
		e.v *= e.decay
	}
	return e.v
}

// split3 streams a three-way split at lowHz/highHz.
type split3 struct{ lo, hi, midHP, midLP stage }

func newSplit(lowHz, highHz float64) *split3 {
	return &split3{lo: stage{q: lowpass(lowHz)}, hi: stage{q: highpass(highHz)}, midHP: stage{q: highpass(lowHz)}, midLP: stage{q: lowpass(highHz)}}
}

func (s *split3) step(x float64) (lo, mid, hi float64) {
	return s.lo.step(x), s.midLP.step(s.midHP.step(x)), s.hi.step(x)
}

// signals tracked per resolution.
const (
	sFull = iota
	sLoA  // 200/2000 split
	sMiA
	sHiA
	sLoB // 150/2500
	sMiB
	sHiB
	sLoC // 120/1500
	sMiC
	sHiC
	sLoD // < 300 Hz
	nSignals
)

type resolution struct {
	n    int
	col  func(i int) int
	sigs [nSignals]*acc
}

// Generate computes all waveform formats from mono samples at SampleRate.
func Generate(samples []float32) *Result {
	total := len(samples)
	nDetail := (total + detailSize - 1) / detailSize
	even := func(n int) func(int) int {
		return func(i int) int { return int(int64(i) * int64(n) / int64(total)) }
	}
	res := map[string]*resolution{
		"detail": {n: nDetail, col: func(i int) int { return i / detailSize }},
		"400":    {n: 400, col: even(400)},
		"100":    {n: 100, col: even(100)},
		"1200":   {n: 1200, col: even(1200)},
	}
	for _, r := range res {
		for s := range r.sigs {
			r.sigs[s] = newAcc(r.n)
		}
	}
	rs := []*resolution{res["detail"], res["400"], res["100"], res["1200"]}
	a, b, c := newSplit(200, 2000), newSplit(150, 2500), newSplit(120, 1500)
	d := stage{q: lowpass(300)}
	// Envelope followers for the 3-band detail (instant attack, exponential
	// release): rekordbox's PWV7 is smooth per beat rather than following
	// individual bass cycles. Release 80 ms (low, mid) and 40 ms (high)
	// raised holdout correlation from ~0.88 to ~0.93-0.95.
	envLo, envMi, envHi := newEnv(0.08), newEnv(0.08), newEnv(0.04)
	env7 := make([]float64, 3*nDetail)
	var v [nSignals]float64
	if total > 0 {
		for i, smp := range samples {
			x := float64(smp)
			v[sFull] = x
			v[sLoA], v[sMiA], v[sHiA] = a.step(x)
			v[sLoB], v[sMiB], v[sHiB] = b.step(x)
			v[sLoC], v[sMiC], v[sHiC] = c.step(x)
			v[sLoD] = d.step(x)
			if c := i / detailSize; c < nDetail {
				e := env7[3*c:]
				e[0] = math.Max(e[0], envLo.step(v[sLoA]))
				e[1] = math.Max(e[1], envMi.step(v[sMiC]))
				e[2] = math.Max(e[2], envHi.step(v[sHiA]))
			}
			for _, r := range rs {
				col := r.col(i)
				for s := 0; s < nSignals; s++ {
					r.sigs[s].add(col, v[s])
				}
			}
		}
	}
	out := &Result{}
	det, p400, p100, p1200 := res["detail"], res["400"], res["100"], res["1200"]

	q := func(k, x, p, max float64) byte {
		if x <= 0 {
			return 0
		}
		return byte(math.Min(max, math.Round(k*math.Pow(x, p))))
	}
	share := func(r *resolution, col int, lo, mi, hi int) (float64, float64, float64) {
		l, m, h := r.sigs[lo].rms(col), r.sigs[mi].rms(col), r.sigs[hi].rms(col)
		sum := l + m + h
		if sum == 0 {
			return 0, 0, 0
		}
		return l / sum, m / sum, h / sum
	}
	rel := func(r *resolution, col int, lo, mi, hi int) (float64, float64, float64) {
		l, m, h := r.sigs[lo].rms(col), r.sigs[mi].rms(col), r.sigs[hi].rms(col)
		mx := math.Max(l, math.Max(m, h))
		if mx == 0 {
			return 0, 0, 0
		}
		return l / mx, m / mx, h / mx
	}

	// Scrolling waveforms, 150 entries per second.
	out.PWV3 = make([]byte, det.n)
	out.PWV5 = make([]byte, 2*det.n)
	out.PWV7 = make([]byte, 3*det.n)
	for col := 0; col < det.n; col++ {
		peak := det.sigs[sFull].peak[col]
		loShare, _, _ := share(det, col, sLoA, sMiA, sHiA)
		// PWV3: height 5 bits (r=0.998), whiteness 3 bits (r=0.94).
		h3 := q(15.06, peak, 2.08, 31)
		w3 := q(7.564, 1-loShare, 1.14, 7)
		if peak == 0 {
			w3 = 0
		}
		out.PWV3[col] = w3<<5 | h3
		// PWV5: RRRGGGBB BHHHHH00 (height r=0.97, R/G/B r=0.87/0.83/0.87).
		lr, _, _ := rel(det, col, sLoB, sMiB, sHiB)
		_, mr, _ := rel(det, col, sLoB, sMiB, sHiB)
		_, _, hr := rel(det, col, sLoC, sMiC, sHiC)
		h5 := uint16(q(14.95, peak, 2.02, 31))
		r5, g5, b5 := uint16(q(6.713, lr, 0.96, 7)), uint16(q(4.73, mr, 0.86, 7)), uint16(q(7.905, hr, 0.54, 7))
		binary.BigEndian.PutUint16(out.PWV5[2*col:], r5<<13|g5<<10|b5<<7|h5<<2)
		// PWV7: low, mid, high envelopes (r=0.95/0.93/0.93).
		out.PWV7[3*col] = q(81.2, env7[3*col], 0.62, maxBand)
		out.PWV7[3*col+1] = q(79.7, env7[3*col+1], 0.90, maxBand)
		out.PWV7[3*col+2] = q(83.4, env7[3*col+2], 1.46, maxBand)
	}

	// Mono preview, 400 columns: height (r=0.88), whiteness (weak, r=0.41).
	out.PWAV = make([]byte, 400)
	for col := 0; col < 400; col++ {
		rms := p400.sigs[sFull].rms(col)
		loShare, _, _ := share(p400, col, sLoA, sMiA, sHiA)
		w := q(4.863, 1-loShare, 0.86, 7)
		if rms == 0 {
			w = 0
		}
		out.PWAV[col] = w<<5 | q(42.29, rms, 0.98, 31)
	}
	// Tiny preview, 100 columns, 4-bit height (r=0.78).
	out.PWV2 = make([]byte, 100)
	for col := 0; col < 100; col++ {
		out.PWV2[col] = q(21.5, p100.sigs[sFull].rms(col), 0.54, 15)
	}

	// Colour and 3-band previews, 1200 columns.
	out.PWV4 = make([]byte, 6*1200)
	out.PWV6 = make([]byte, 3*1200)
	for col := 0; col < 1200; col++ {
		peak := p1200.sigs[sFull].peak[col]
		e := out.PWV4[6*col:]
		e[0] = q(85.98, peak, 1.02, maxBand) // r=0.97
		d1 := 230.5 - 65.7*peak              // unknown meaning; linear fit r=0.64
		if peak == 0 {
			d1 = 0
		}
		e[1] = byte(math.Max(0, math.Min(255, math.Round(d1))))
		e[2] = q(96.09, p1200.sigs[sLoD].peak[col], 0.78, maxBand)              // r=0.98
		e[3] = q(87.83, p1200.sigs[sLoA].peak[col], 1.02, maxBand)              // red, r=0.98
		e[4] = q(62.89, p1200.sigs[sMiA].peak[col], 1.04, maxBand)              // green, r=0.89
		e[5] = q(47.12, p1200.sigs[sHiA].peak[col], 0.92, maxBand)              // blue, r=0.89
		out.PWV6[3*col] = q(54.36, p1200.sigs[sLoA].rms(col), 0.54, maxBand)    // r=0.84
		out.PWV6[3*col+1] = q(63.17, p1200.sigs[sMiA].rms(col), 0.46, maxBand)  // r=0.70
		out.PWV6[3*col+2] = q(100.98, p1200.sigs[sHiC].rms(col), 0.58, maxBand) // r=0.79
	}
	return out
}

// section builds an ANLZ section with rekordbox's header layout.
func section(tag string, fields []uint32, trailer []byte, body []byte) anlz.Section {
	hl := 12 + 4*len(fields) + len(trailer)
	b := make([]byte, 0, hl+len(body))
	b = append(b, tag...)
	b = binary.BigEndian.AppendUint32(b, uint32(hl))
	b = binary.BigEndian.AppendUint32(b, uint32(hl+len(body)))
	for _, f := range fields {
		b = binary.BigEndian.AppendUint32(b, f)
	}
	b = append(b, trailer...)
	return anlz.Section{Tag: tag, Header: hl, Raw: append(b, body...)}
}

// Analysis wraps the result as ANLZ sections with the exact header fields
// rekordbox 7 writes (constant across all 85 files we examined, apart from
// entry counts), so the dbserver serves them like rekordbox analysis.
func (r *Result) Analysis() *anlz.Analysis {
	n := uint32(len(r.PWV3))
	return &anlz.Analysis{
		Source: anlz.SourceSlipmat,
		DAT: &anlz.File{Sections: []anlz.Section{
			section("PWAV", []uint32{uint32(len(r.PWAV)), 0x00010000}, nil, r.PWAV),
			section("PWV2", []uint32{uint32(len(r.PWV2)), 0x00010000}, nil, r.PWV2),
		}},
		EXT: &anlz.File{Sections: []anlz.Section{
			section("PWV3", []uint32{1, n, 0x00960000}, nil, r.PWV3),
			section("PWV5", []uint32{2, n, 0x00960305}, nil, r.PWV5),
			section("PWV4", []uint32{6, 1200, 0}, nil, r.PWV4),
		}},
		EX2: &anlz.File{Sections: []anlz.Section{
			section("PWV7", []uint32{3, n, 0x00960000}, nil, r.PWV7),
			section("PWV6", []uint32{3, 1200}, nil, r.PWV6),
		}},
	}
}
