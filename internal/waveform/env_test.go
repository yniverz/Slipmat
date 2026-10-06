package waveform

import (
	"context"
	"fmt"
	"math"
	"testing"
)

// envelope: instant attack, exponential release with time constant tau;
// sampled as the maximum within each detail window.
func envCols(x []float32, n int, tau float64) []float64 {
	out := make([]float64, n)
	dec := math.Exp(-1 / (tau * SampleRate))
	var env float64
	for i, v := range x {
		a := math.Abs(float64(v))
		if a > env {
			env = a
		} else {
			env *= dec
		}
		c := i / detailSize
		if c < n && env > out[c] {
			out[c] = env
		}
	}
	return out
}

func TestEnvelopeFit(t *testing.T) {
	set := calibrationSet(t, 10)
	type key struct {
		band string
		tau  float64
	}
	acc := map[key][]pair{}
	for _, ct := range set {
		samples, _ := Decode(context.Background(), ct.t.Path)
		p7s, _ := ct.a.Section("2EX", "PWV7")
		p7 := p7s.Body()
		n := len(p7) / 3
		a := Split(samples, 200, 2000)
		c := Split(samples, 120, 1500)
		for _, tau := range []float64{0, 0.01, 0.02, 0.04, 0.08, 0.15} {
			var lo, mi, hi []float64
			if tau == 0 {
				lo, mi, hi = ColumnsFixed(a.Low, detailSize, n).Peak, ColumnsFixed(c.Mid, detailSize, n).Peak, ColumnsFixed(a.High, detailSize, n).Peak
			} else {
				lo, mi, hi = envCols(a.Low, n, tau), envCols(c.Mid, n, tau), envCols(a.High, n, tau)
			}
			for i := 0; i < n; i += 3 {
				acc[key{"low", tau}] = append(acc[key{"low", tau}], pair{lo[i], float64(p7[3*i])})
				acc[key{"mid", tau}] = append(acc[key{"mid", tau}], pair{mi[i], float64(p7[3*i+1])})
				acc[key{"high", tau}] = append(acc[key{"high", tau}], pair{hi[i], float64(p7[3*i+2])})
			}
		}
	}
	for _, b := range []string{"low", "mid", "high"} {
		line := b + ":"
		for _, tau := range []float64{0, 0.01, 0.02, 0.04, 0.08, 0.15} {
			ps := acc[key{b, tau}]
			xs, ys := make([]float64, len(ps)), make([]float64, len(ps))
			for i, p := range ps {
				xs[i], ys[i] = p.x, p.y
			}
			k, p := fitPower(xs, ys, 127)
			var pp []pair
			for i := range xs {
				pp = append(pp, pair{math.Min(127, math.Round(k*math.Pow(xs[i], p))), ys[i]})
			}
			line += fmt.Sprintf("  tau=%.2f r=%.3f (k=%.1f p=%.2f)", tau, pearson(pp), k, p)
		}
		t.Log(line)
	}
}
