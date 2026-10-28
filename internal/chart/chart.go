// Package chart draws the period comparison chart that goes into a report
// section. It draws only figures the server prepared; nothing is estimated.
package chart

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Row is one indicator: its label, current and prior values, and the texts
// printed beside the bars.
type Row struct {
	Label                       string
	Current, Prior              float64
	CurrentText, PriorText, Tag string
}

// Scale is the pixel density; the image is inserted at Width/Scale.
const (
	Scale = 2
	Width = 900 * Scale
)

var (
	ink     = color.RGBA{0x1e, 0x1e, 0x1e, 0xff}
	muted   = color.RGBA{0x6b, 0x6b, 0x6b, 0xff}
	current = color.RGBA{0x00, 0x33, 0x8d, 0xff}
	prior   = color.RGBA{0xb4, 0xbc, 0xc8, 0xff}
	accent  = color.RGBA{0xc1, 0x2b, 0x2b, 0xff}
)

// Bars draws one pair of horizontal bars per row. Units differ between rows,
// so each row is scaled to its own larger value and the subtitle says so.
func Bars(title, subtitle, currentLabel, priorLabel string, rows []Row) ([]byte, int, int, error) {
	regular, err := face(goregular.TTF, 13)
	if err != nil {
		return nil, 0, 0, err
	}
	bold, err := face(gobold.TTF, 15)
	if err != nil {
		return nil, 0, 0, err
	}
	small, err := face(goregular.TTF, 11)
	if err != nil {
		return nil, 0, 0, err
	}
	s := func(v int) int { return v * Scale }
	rowH, top := s(54), s(86)
	height := top + len(rows)*rowH + s(16)
	img := image.NewRGBA(image.Rect(0, 0, Width, height))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	text(img, bold, ink, s(16), s(26), title)
	text(img, small, muted, s(16), s(46), subtitle)
	box(img, s(16), s(58), s(28), s(68), current)
	text(img, small, ink, s(34), s(68), currentLabel)
	box(img, s(110), s(58), s(122), s(68), prior)
	text(img, small, ink, s(128), s(68), priorLabel)
	labelW, barX, barMax := s(250), s(270), s(430)
	for i, r := range rows {
		y := top + i*rowH
		text(img, regular, ink, s(16), y+s(20), fit(regular, r.Label, labelW))
		if r.Tag != "" {
			text(img, small, accent, s(16), y+s(38), r.Tag)
		}
		max := r.Current
		if r.Prior > max {
			max = r.Prior
		}
		bar := func(v float64, top int, c color.Color, label string) {
			w := 0
			if max > 0 && v > 0 {
				w = int(float64(barMax) * v / max)
			}
			box(img, barX, top, barX+w, top+s(16), c)
			text(img, small, ink, barX+w+s(8), top+s(13), label)
		}
		bar(r.Current, y+s(4), current, r.CurrentText)
		bar(r.Prior, y+s(24), prior, r.PriorText)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, 0, 0, err
	}
	return buf.Bytes(), Width / Scale, height / Scale, nil
}

func face(ttf []byte, size float64) (font.Face, error) {
	f, err := opentype.Parse(ttf)
	if err != nil {
		return nil, err
	}
	return opentype.NewFace(f, &opentype.FaceOptions{Size: size * Scale, DPI: 72, Hinting: font.HintingFull})
}

func text(img draw.Image, f font.Face, c color.Color, x, y int, s string) {
	d := font.Drawer{Dst: img, Src: image.NewUniform(c), Face: f, Dot: fixed.P(x, y)}
	d.DrawString(s)
}

func box(img draw.Image, x0, y0, x1, y1 int, c color.Color) {
	draw.Draw(img, image.Rect(x0, y0, x1, y1), image.NewUniform(c), image.Point{}, draw.Src)
}

// fit shortens s with an ellipsis until it fits in width pixels.
func fit(f font.Face, s string, width int) string {
	r := []rune(s)
	for len(r) > 1 && font.MeasureString(f, string(r)).Ceil() > width {
		r = append(r[:len(r)-2], '…')
	}
	return string(r)
}
