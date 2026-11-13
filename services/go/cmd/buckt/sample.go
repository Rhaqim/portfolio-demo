package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"sync"
)

var (
	sampleOnce  sync.Once
	sampleBytes []byte
)

// samplePNG renders a 1600x1000 interference pattern once, so every new
// sandbox starts with an image big enough to make the derivatives visible.
func samplePNG() []byte {
	sampleOnce.Do(func() {
		const w, h = 1600, 1000
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := range h {
			for x := range w {
				fx, fy := float64(x)/w, float64(y)/h
				v := math.Sin(fx*18) + math.Sin(fy*11) + math.Sin((fx+fy)*14)
				t := (v + 3) / 6
				img.Set(x, y, color.RGBA{
					R: uint8(40 + 180*t),
					G: uint8(30 + 60*(1-t)),
					B: uint8(60 + 120*fy),
					A: 255,
				})
			}
		}
		var buf bytes.Buffer
		_ = png.Encode(&buf, img)
		sampleBytes = buf.Bytes()
	})
	return sampleBytes
}
