// SPDX-License-Identifier: GPL-3.0-or-later

package waveform

import (
	"image"
	"image/color"

	"github.com/yniverz/slipmat/internal/anlz"
)

// Render draws an analysis the way players roughly show it, for visual
// comparison (rendering rules follow beat-link's waveform components):
//
//	row 1: PWV4 colour overview (1200 columns, back + front layers)
//	row 2: PWV6 3-band overview
//	row 3: PWV5 colour detail, `seconds` seconds from `from`
//	row 4: PWV7 3-band detail, same window
//	row 5: PWV3 blue/white detail, same window
//
// width is the image width; detail rows show one entry per pixel column
// (scaled to fit).
func Render(a *anlz.Analysis, width int, from, seconds float64) *image.RGBA {
	const rowH = 70
	img := image.NewRGBA(image.Rect(0, 0, width, 5*rowH))
	bg := color.RGBA{12, 12, 16, 255}
	for i := range img.Pix {
		img.Pix[i] = []byte{bg.R, bg.G, bg.B, bg.A}[i%4]
	}
	bar := func(row, x, h int, c color.RGBA, mirror bool) {
		base := row*rowH + rowH - 2
		if mirror {
			base = row*rowH + rowH/2
		}
		for y := 0; y < h && y < rowH-2; y++ {
			img.SetRGBA(x, base-y, c)
			if mirror {
				img.SetRGBA(x, base+y, c)
			}
		}
	}
	scale := func(v, maxV float64, px int) int { return int(v / maxV * float64(px)) }

	if s, ok := a.Section("EXT", "PWV4"); ok {
		b := s.Body()
		for x := 0; x < width; x++ {
			i := x * (len(b) / 6) / width * 6
			r, g, bl := float64(b[i+3]), float64(b[i+4]), float64(b[i+5])
			back := max(r, g, bl)
			if back == 0 {
				continue
			}
			bar(0, x, scale(back, 127, rowH-4), color.RGBA{uint8(r * 191 / back), uint8(g * 191 / back), uint8(bl * 191 / back), 255}, false)
			bar(0, x, scale(bl, 127, rowH-4), color.RGBA{uint8(r * 255 / back), uint8(g * 255 / back), uint8(bl * 255 / back), 255}, false)
		}
	}
	threeBand := func(row int, b []byte, lo, mi, hi float64, maxV float64) {
		n := len(b) / 3
		for x := 0; x < width; x++ {
			i := x * n / width * 3
			l, m, h := float64(b[i])*lo, float64(b[i+1])*mi, float64(b[i+2])*hi
			bar(row, x, scale(l+m+h, maxV, rowH/2-2), color.RGBA{240, 240, 240, 255}, true)
			bar(row, x, scale(l+m, maxV, rowH/2-2), color.RGBA{230, 140, 30, 255}, true)
			bar(row, x, scale(l, maxV, rowH/2-2), color.RGBA{30, 70, 200, 255}, true)
		}
	}
	if s, ok := a.Section("2EX", "PWV6"); ok {
		threeBand(1, s.Body(), 0.49, 0.32, 0.25, 64)
	}
	window := func(n int) (int, int) {
		a := int(from * detailRate)
		b := a + int(seconds*detailRate)
		return min(a, n), min(b, n)
	}
	if s, ok := a.Section("EXT", "PWV5"); ok {
		b := s.Body()
		start, end := window(len(b) / 2)
		for x := 0; x < width && end > start; x++ {
			i := start + x*(end-start)/width
			v := uint16(b[2*i])<<8 | uint16(b[2*i+1])
			h := int(v >> 2 & 0x1f)
			c := color.RGBA{uint8(v >> 13 * 36), uint8(v >> 10 & 7 * 36), uint8(v >> 7 & 7 * 36), 255}
			bar(2, x, scale(float64(h), 31, rowH/2-2), c, true)
		}
	}
	if s, ok := a.Section("2EX", "PWV7"); ok {
		b := s.Body()
		start, end := window(len(b) / 3)
		if end > start {
			threeBand(3, b[3*start:3*end], 0.4, 0.3, 0.06, 64)
		}
	}
	if s, ok := a.Section("EXT", "PWV3"); ok {
		b := s.Body()
		start, end := window(len(b))
		for x := 0; x < width && end > start; x++ {
			v := b[start+x*(end-start)/width]
			c := color.RGBA{40, 90, 220, 255}
			if v>>5 >= 5 {
				c = color.RGBA{150, 200, 255, 255}
			}
			bar(4, x, scale(float64(v&0x1f), 31, rowH/2-2), c, true)
		}
	}
	return img
}
