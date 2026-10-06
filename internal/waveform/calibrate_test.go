// SPDX-License-Identifier: GPL-3.0-or-later

package waveform

// Calibration against rekordbox 7. These tests only run with
// SLIPMAT_CALIBRATE_DIR pointing at a music folder whose tracks have
// rekordbox analysis (in rekordbox's own folder or a copied USBANLZ):
//
//	SLIPMAT_CALIBRATE_DIR=~/Music/Slipmat go test ./internal/waveform -run 'Fit|Generator' -v
//
// TestFit fits the per-byte curves used in generate.go (on half the tracks,
// validated on the other half); TestGeneratorAgainstRekordbox scores the
// real generator end to end.

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"testing"

	"github.com/yniverz/slipmat/internal/anlz"
	"github.com/yniverz/slipmat/internal/library"
)

type pair struct{ x, y float64 }

func pearson(ps []pair) float64 {
	var sx, sy, sxx, syy, sxy float64
	n := float64(len(ps))
	for _, p := range ps {
		sx += p.x
		sy += p.y
		sxx += p.x * p.x
		syy += p.y * p.y
		sxy += p.x * p.y
	}
	d := math.Sqrt((n*sxx - sx*sx) * (n*syy - sy*sy))
	if d == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / d
}

// powerFit fits y = k * x^p over points with x, y > 0.
func powerFit(ps []pair) (k, p float64) {
	var l []pair
	for _, q := range ps {
		if q.x > 1e-6 && q.y > 0.5 {
			l = append(l, pair{math.Log(q.x), math.Log(q.y)})
		}
	}
	if len(l) < 10 {
		return 0, 0
	}
	var sx, sy, sxx, sxy float64
	n := float64(len(l))
	for _, q := range l {
		sx += q.x
		sy += q.y
		sxx += q.x * q.x
		sxy += q.x * q.y
	}
	p = (n*sxy - sx*sy) / (n*sxx - sx*sx)
	k = math.Exp((sy - p*sx) / n)
	return k, p
}

type calTrack struct {
	t *library.Track
	a *anlz.Analysis
}

func calibrationSet(t *testing.T, max int) []calTrack {
	dir := os.Getenv("SLIPMAT_CALIBRATE_DIR")
	if dir == "" {
		t.Skip("SLIPMAT_CALIBRATE_DIR not set")
	}
	lib, err := library.Scan(context.Background(), dir, library.ScanOptions{Prober: library.FFprobe{}})
	if err != nil {
		t.Fatal(err)
	}
	var refs []anlz.TrackRef
	for _, tr := range lib.Tracks() {
		refs = append(refs, anlz.TrackRef{ID: tr.ID, RelPath: tr.RelPath})
	}
	ix, _ := anlz.BuildIndex(anlz.DefaultDirs(dir), refs, nil)
	var out []calTrack
	for _, id := range ix.IDs() {
		tr, _ := lib.Track(id)
		out = append(out, calTrack{tr, ix.Load(id)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].t.RelPath < out[j].t.RelPath })
	if len(out) > max {
		out = out[:max]
	}
	return out
}

type featSet map[string][]float64

func features(samples []float32, n int, fixed bool) featSet {
	fs := featSet{}
	col := func(x []float32) Stats {
		if fixed {
			return ColumnsFixed(x, SampleRate/150.0, n)
		}
		return Columns(x, n)
	}
	full := col(samples)
	fs["full.rms"], fs["full.peak"] = full.RMS, full.Peak
	for _, sp := range [][2]float64{{120, 1500}, {150, 2500}, {200, 2000}, {200, 4000}, {300, 3000}} {
		b := Split(samples, sp[0], sp[1])
		tag := fmt.Sprintf("%v-%v", sp[0], sp[1])
		lo, mi, hi := col(b.Low), col(b.Mid), col(b.High)
		for name, st := range map[string]Stats{"lo": lo, "mi": mi, "hi": hi} {
			fs[name+".rms."+tag] = st.RMS
			fs[name+".peak."+tag] = st.Peak
			rel := make([]float64, n)
			share := make([]float64, n)
			for i := 0; i < n; i++ {
				mx := math.Max(lo.RMS[i], math.Max(mi.RMS[i], hi.RMS[i]))
				sum := lo.RMS[i] + mi.RMS[i] + hi.RMS[i]
				if mx > 0 {
					rel[i] = st.RMS[i] / mx
					share[i] = st.RMS[i] / sum
				}
			}
			fs[name+".rel."+tag] = rel
			fs[name+".share."+tag] = share
		}
	}
	return fs
}

// model: y = clamp(k * x^p), fitted by least squares on y over a grid of p
// (k solved in closed form for each p).
func fitPower(xs, ys []float64, ymax float64) (k, p float64) {
	bestErr := math.Inf(1)
	for pp := 0.2; pp <= 4.01; pp += 0.02 {
		var sxy, sxx float64
		for i := range xs {
			f := math.Pow(xs[i], pp)
			sxy += f * ys[i]
			sxx += f * f
		}
		if sxx == 0 {
			continue
		}
		kk := sxy / sxx
		var e float64
		for i := range xs {
			v := math.Min(ymax, math.Round(kk*math.Pow(xs[i], pp)))
			e += (v - ys[i]) * (v - ys[i])
		}
		if e < bestErr {
			bestErr, k, p = e, kk, pp
		}
	}
	return
}

func TestFit(t *testing.T) {
	set := calibrationSet(t, 60)
	type spec struct {
		target, feature string
		ymax            float64
	}
	specs := []spec{
		{"PWV3.height", "full.peak", 31}, {"PWV3.white", "hi.white", 7},
		{"PWV5.height", "full.peak", 31}, {"PWV5.R", "lo.rel.150-2500", 7}, {"PWV5.G", "mi.rel.150-2500", 7}, {"PWV5.B", "hi.rel.120-1500", 7},
		{"PWV7.low", "lo.peak.200-2000", 255}, {"PWV7.mid", "mi.peak.120-1500", 255}, {"PWV7.high", "hi.peak.200-2000", 255},
		{"PWAV.height", "full.rms", 31}, {"PWAV.white", "hi.white", 7}, {"PWV2.height", "full.rms", 15},
		{"PWV4.d0", "full.peak", 255}, {"PWV4.d2", "lo.peak.300-3000", 255}, {"PWV4.d3", "lo.peak.200-2000", 255}, {"PWV4.d4", "mi.peak.200-2000", 255}, {"PWV4.d5", "hi.peak.200-2000", 255},
		{"PWV6.low", "lo.rms.200-2000", 255}, {"PWV6.mid", "mi.rms.200-2000", 255}, {"PWV6.high", "hi.rms.120-1500", 255},
	}
	type data struct{ xs, ys []float64 }
	train, test := map[string]*data{}, map[string]*data{}
	for ti, ct := range set {
		dst := train
		if ti%2 == 1 {
			dst = test
		}
		samples, _ := Decode(context.Background(), ct.t.Path)
		pwv3, _ := ct.a.Section("EXT", "PWV3")
		n := len(pwv3.Body())
		withWhite := func(fs featSet) featSet {
			w := make([]float64, len(fs["full.rms"]))
			for i := range w {
				w[i] = 1 - fs["lo.share.200-2000"][i]
			}
			fs["hi.white"] = w
			return fs
		}
		fd := withWhite(features(samples, n, true))
		f4 := withWhite(features(samples, 400, false))
		f1 := withWhite(features(samples, 100, false))
		f12 := withWhite(features(samples, 1200, false))
		get := func(tag, kind string) []byte { s, _ := ct.a.Section(kind, tag); return s.Body() }
		p3, p5, p7 := get("PWV3", "EXT"), get("PWV5", "EXT"), get("PWV7", "2EX")
		pw, p2, p4, p6 := get("PWAV", "DAT"), get("PWV2", "DAT"), get("PWV4", "EXT"), get("PWV6", "2EX")
		val := func(target string, i int) (float64, featSet, bool) {
			switch target {
			case "PWV3.height":
				return float64(p3[i] & 0x1f), fd, true
			case "PWV3.white":
				return float64(p3[i] >> 5), fd, true
			case "PWV5.height", "PWV5.R", "PWV5.G", "PWV5.B":
				v := uint16(p5[2*i])<<8 | uint16(p5[2*i+1])
				sh := map[string]uint{"PWV5.height": 2, "PWV5.R": 13, "PWV5.G": 10, "PWV5.B": 7}[target]
				mask := uint16(7)
				if target == "PWV5.height" {
					mask = 0x1f
				}
				return float64(v >> sh & mask), fd, true
			case "PWV7.low", "PWV7.mid", "PWV7.high":
				return float64(p7[3*i+map[string]int{"PWV7.low": 0, "PWV7.mid": 1, "PWV7.high": 2}[target]]), fd, true
			}
			return 0, nil, false
		}
		for _, sp := range specs {
			d := dst[sp.target]
			if d == nil {
				d = &data{}
				dst[sp.target] = d
			}
			switch {
			case sp.target[:4] == "PWV3" || sp.target[:4] == "PWV5" || sp.target[:4] == "PWV7":
				for i := 0; i < n; i += 5 {
					y, fs, _ := val(sp.target, i)
					d.xs = append(d.xs, fs[sp.feature][i])
					d.ys = append(d.ys, y)
				}
			case sp.target == "PWAV.height" || sp.target == "PWAV.white":
				for i, b := range pw {
					y := float64(b & 0x1f)
					if sp.target == "PWAV.white" {
						y = float64(b >> 5)
					}
					d.xs = append(d.xs, f4[sp.feature][i])
					d.ys = append(d.ys, y)
				}
			case sp.target == "PWV2.height":
				for i, b := range p2 {
					d.xs = append(d.xs, f1[sp.feature][i])
					d.ys = append(d.ys, float64(b&0xf))
				}
			case sp.target[:4] == "PWV4":
				di := int(sp.target[6] - '0')
				for i := 0; i < 1200; i++ {
					d.xs = append(d.xs, f12[sp.feature][i])
					d.ys = append(d.ys, float64(p4[6*i+di]))
				}
			case sp.target[:4] == "PWV6":
				bi := map[string]int{"PWV6.low": 0, "PWV6.mid": 1, "PWV6.high": 2}[sp.target]
				for i := 0; i < 1200; i++ {
					d.xs = append(d.xs, f12[sp.feature][i])
					d.ys = append(d.ys, float64(p6[3*i+bi]))
				}
			}
		}
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].target < specs[j].target })
	for _, sp := range specs {
		tr, te := train[sp.target], test[sp.target]
		k, p := fitPower(tr.xs, tr.ys, sp.ymax)
		var ps []pair
		var mae, my float64
		for i := range te.xs {
			v := math.Min(sp.ymax, math.Round(k*math.Pow(te.xs[i], p)))
			ps = append(ps, pair{v, te.ys[i]})
			mae += math.Abs(v - te.ys[i])
			my += te.ys[i]
		}
		nn := float64(len(te.xs))
		t.Log(fmt.Sprintf("%-12s %-18s k=%9.3f p=%.2f | holdout r=%.3f MAE=%.2f (mean %.1f, max %.0f)", sp.target, sp.feature, k, p, pearson(ps), mae/nn, my/nn, sp.ymax))
	}
}

func TestFitD1(t *testing.T) {
	set := calibrationSet(t, 60)
	var sx, sy, sxx, sxy, n float64
	type pt struct{ x, y float64 }
	var test []pt
	for ti, ct := range set {
		samples, _ := Decode(context.Background(), ct.t.Path)
		fs := features(samples, 1200, false)
		p4s, _ := ct.a.Section("EXT", "PWV4")
		p4 := p4s.Body()
		for i := 0; i < 1200; i++ {
			x, y := fs["full.peak"][i], float64(p4[6*i+1])
			if ti%2 == 1 {
				test = append(test, pt{x, y})
				continue
			}
			sx += x
			sy += y
			sxx += x * x
			sxy += x * y
			n++
		}
	}
	b := (n*sxy - sx*sy) / (n*sxx - sx*sx)
	a := (sy - b*sx) / n
	var ps []pair
	var mae float64
	for _, p := range test {
		v := math.Max(128, math.Min(255, math.Round(a+b*p.x)))
		ps = append(ps, pair{v, p.y})
		mae += math.Abs(v - p.y)
	}
	t.Log(fmt.Sprintf("d1 = %.1f + %.1f*peak | holdout r=%.3f MAE=%.1f", a, b, pearson(ps), mae/float64(len(test))))
}

// TestGeneratorAgainstRekordbox runs Generate on every calibration track
// and reports, per format and byte, the correlation with rekordbox's bytes
// and the mean absolute error.
func TestGeneratorAgainstRekordbox(t *testing.T) {
	set := calibrationSet(t, 1000)
	type stat struct {
		ps  []pair
		abs float64
	}
	stats := map[string]*stat{}
	add := func(k string, ours, rb byte) {
		s := stats[k]
		if s == nil {
			s = &stat{}
			stats[k] = s
		}
		s.ps = append(s.ps, pair{float64(ours), float64(rb)})
		s.abs += math.Abs(float64(ours) - float64(rb))
	}
	for _, ct := range set {
		samples, err := Decode(context.Background(), ct.t.Path)
		if err != nil {
			t.Fatal(err)
		}
		g := Generate(samples).Analysis()
		cmp := func(kind, tag string, width int, names ...string) {
			rb, ok1 := ct.a.Section(kind, tag)
			us, ok2 := g.Section(kind, tag)
			if !ok1 || !ok2 {
				t.Fatalf("%s missing", tag)
			}
			x, y := us.Body(), rb.Body()
			if len(x) != len(y) {
				t.Errorf("%s %s: %d bytes, rekordbox %d", ct.t.Title, tag, len(x), len(y))
			}
			for i := 0; i+width <= min(len(x), len(y)); i += width {
				for j, n := range names {
					if n != "" {
						add(tag+"."+n, x[i+j], y[i+j])
					}
				}
			}
		}
		cmp("EXT", "PWV3", 1, "byte")
		cmp("EXT", "PWV4", 6, "d0", "d1", "d2", "d3", "d4", "d5")
		cmp("2EX", "PWV6", 3, "low", "mid", "high")
		cmp("2EX", "PWV7", 3, "low", "mid", "high")
		cmp("DAT", "PWAV", 1, "byte")
		cmp("DAT", "PWV2", 1, "byte")
		// PWV5 fields are bit-packed; compare decoded fields.
		rb, _ := ct.a.Section("EXT", "PWV5")
		us, _ := g.Section("EXT", "PWV5")
		x, y := us.Body(), rb.Body()
		for i := 0; i+1 < min(len(x), len(y)); i += 2 {
			a, b := uint16(x[i])<<8|uint16(x[i+1]), uint16(y[i])<<8|uint16(y[i+1])
			add("PWV5.height", byte(a>>2&0x1f), byte(b>>2&0x1f))
			add("PWV5.R", byte(a>>13), byte(b>>13))
			add("PWV5.G", byte(a>>10&7), byte(b>>10&7))
			add("PWV5.B", byte(a>>7&7), byte(b>>7&7))
		}
		// PWV3/PWAV fields.
		r3, _ := ct.a.Section("EXT", "PWV3")
		u3, _ := g.Section("EXT", "PWV3")
		for i := range min(len(r3.Body()), len(u3.Body())) {
			add("PWV3.height", u3.Body()[i]&0x1f, r3.Body()[i]&0x1f)
			add("PWV3.white", u3.Body()[i]>>5, r3.Body()[i]>>5)
		}
	}
	keys := make([]string, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("%d tracks", len(set))
	for _, k := range keys {
		s := stats[k]
		t.Logf("%-12s r=%.3f  mean abs error %.2f", k, pearson(s.ps), s.abs/float64(len(s.ps)))
	}
}
