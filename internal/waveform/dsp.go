// SPDX-License-Identifier: GPL-3.0-or-later

// Package waveform computes rekordbox-style waveform data (PWAV, PWV2,
// PWV3, PWV4, PWV5, PWV6, PWV7) from audio, for tracks that have no
// rekordbox analysis. Band levels and scales are calibrated against
// rekordbox 7's own analysis of real tracks (see calibrate_test.go).
package waveform

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os/exec"
)

// SampleRate is the rate audio is decoded at.
const SampleRate = 44100

// Decode decodes an audio file to mono float32 samples at SampleRate
// using ffmpeg.
func Decode(ctx context.Context, path string) ([]float32, error) {
	// -flags2 +skip_manual: don't trim the MP3 encoder delay ("gapless"),
	// as rekordbox doesn't; otherwise waveforms of LAME-tagged MP3s would be
	// ~27 ms early against what the player plays. [RB7 calibration]
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-nostdin", "-flags2", "+skip_manual", "-i", path,
		"-f", "f32le", "-acodec", "pcm_f32le", "-ac", "1", "-ar", fmt.Sprint(SampleRate), "-")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("ffmpeg: %s", ee.Stderr)
		}
		return nil, fmt.Errorf("ffmpeg: %w", err)
	}
	s := make([]float32, len(out)/4)
	for i := range s {
		s[i] = math.Float32frombits(binary.LittleEndian.Uint32(out[4*i:]))
	}
	return s, nil
}

// biquad is a second-order IIR filter (RBJ audio-EQ cookbook).
type biquad struct{ b0, b1, b2, a1, a2 float64 }

func lowpass(f float64) biquad {
	w := 2 * math.Pi * f / SampleRate
	alpha := math.Sin(w) / math.Sqrt2 // Q = 1/sqrt(2): Butterworth
	cw := math.Cos(w)
	a0 := 1 + alpha
	return biquad{(1 - cw) / 2 / a0, (1 - cw) / a0, (1 - cw) / 2 / a0, -2 * cw / a0, (1 - alpha) / a0}
}

func highpass(f float64) biquad {
	w := 2 * math.Pi * f / SampleRate
	alpha := math.Sin(w) / math.Sqrt2
	cw := math.Cos(w)
	a0 := 1 + alpha
	return biquad{(1 + cw) / 2 / a0, -(1 + cw) / a0, (1 + cw) / 2 / a0, -2 * cw / a0, (1 - alpha) / a0}
}

// apply filters x, returning a new slice.
func (q biquad) apply(x []float32) []float32 {
	y := make([]float32, len(x))
	var x1, x2, y1, y2 float64
	for i, v := range x {
		xv := float64(v)
		yv := q.b0*xv + q.b1*x1 + q.b2*x2 - q.a1*y1 - q.a2*y2
		x2, x1, y2, y1 = x1, xv, y1, yv
		y[i] = float32(yv)
	}
	return y
}

// cascade applies the filter twice (4th order, 24 dB/octave).
func (q biquad) cascade(x []float32) []float32 { return q.apply(q.apply(x)) }

// Bands is a three-way split of a signal.
type Bands struct {
	Low, Mid, High, Full []float32
}

// Split separates samples into low (< lowHz), high (> highHz) and the
// mid band between them.
func Split(samples []float32, lowHz, highHz float64) Bands {
	low := lowpass(lowHz).cascade(samples)
	high := highpass(highHz).cascade(samples)
	mid := highpass(lowHz).cascade(lowpass(highHz).cascade(samples))
	return Bands{Low: low, Mid: mid, High: high, Full: samples}
}

// Column statistics of a signal over n equal windows (the last window
// takes the remainder).
type Stats struct {
	RMS, Peak []float64
}

// Columns computes RMS and peak for n windows spanning x.
func Columns(x []float32, n int) Stats {
	st := Stats{RMS: make([]float64, n), Peak: make([]float64, n)}
	if n == 0 || len(x) == 0 {
		return st
	}
	for c := 0; c < n; c++ {
		a := int(int64(c) * int64(len(x)) / int64(n))
		b := int(int64(c+1) * int64(len(x)) / int64(n))
		if b <= a {
			b = min(a+1, len(x))
		}
		var sum, peak float64
		for _, v := range x[a:b] {
			f := math.Abs(float64(v))
			sum += f * f
			if f > peak {
				peak = f
			}
		}
		st.RMS[c] = math.Sqrt(sum / float64(b-a))
		st.Peak[c] = peak
	}
	return st
}

// ColumnsFixed computes stats for windows of a fixed size (detail
// waveforms: SampleRate/150 samples per entry), returning n windows.
func ColumnsFixed(x []float32, size float64, n int) Stats {
	st := Stats{RMS: make([]float64, n), Peak: make([]float64, n)}
	for c := 0; c < n; c++ {
		a := int(float64(c) * size)
		b := int(float64(c+1) * size)
		if a >= len(x) {
			break
		}
		b = min(b, len(x))
		var sum, peak float64
		for _, v := range x[a:b] {
			f := math.Abs(float64(v))
			sum += f * f
			if f > peak {
				peak = f
			}
		}
		if b > a {
			st.RMS[c] = math.Sqrt(sum / float64(b-a))
		}
		st.Peak[c] = peak
	}
	return st
}
