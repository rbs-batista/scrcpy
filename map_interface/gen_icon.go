//go:build ignore

package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"os"
)

func drawPhone(size int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	s := float64(size)

	inRR := func(px, py, x1, y1, x2, y2, r float64) bool {
		if px < x1 || px > x2 || py < y1 || py > y2 {
			return false
		}
		var cx, cy float64
		corner := true
		switch {
		case px < x1+r && py < y1+r:
			cx, cy = x1+r, y1+r
		case px > x2-r && py < y1+r:
			cx, cy = x2-r, y1+r
		case px < x1+r && py > y2-r:
			cx, cy = x1+r, y2-r
		case px > x2-r && py > y2-r:
			cx, cy = x2-r, y2-r
		default:
			corner = false
		}
		if corner {
			dx, dy := px-cx, py-cy
			return dx*dx+dy*dy <= r*r
		}
		return true
	}

	lerp := func(a, b, t float64) uint8 {
		v := a + (b-a)*t
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return uint8(v)
	}

	// Phone body — blue gradient top (#64B5F6) to bottom (#1565C0)
	bx1, by1 := s*0.18, s*0.04
	bx2, by2 := s*0.82, s*0.96
	br := s * 0.14

	// Screen — dark navy
	sx1, sy1 := s*0.25, s*0.18
	sx2, sy2 := s*0.75, s*0.77
	sr := s * 0.04

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5

			if inRR(fx, fy, bx1, by1, bx2, by2, br) {
				t := (fy - by1) / (by2 - by1)
				img.SetRGBA(x, y, color.RGBA{
					R: lerp(100, 21, t),
					G: lerp(181, 101, t),
					B: lerp(246, 192, t),
					A: 255,
				})
			}

			if inRR(fx, fy, sx1, sy1, sx2, sy2, sr) {
				img.SetRGBA(x, y, color.RGBA{10, 10, 20, 255})
			}
		}
	}

	// Signal bars inside screen (3 bars, bottom-right of screen)
	barBaseY := sy2 - s*0.04
	barX := sx2 - s*0.06
	barW := s * 0.035
	barGap := s * 0.022
	barHeights := []float64{s * 0.06, s * 0.10, s * 0.14}
	for i, h := range barHeights {
		bx := barX - float64(len(barHeights)-1-i)*(barW+barGap)
		alpha := uint8(200)
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				fx, fy := float64(x)+0.5, float64(y)+0.5
				if fx >= bx && fx < bx+barW && fy >= barBaseY-h && fy < barBaseY {
					img.SetRGBA(x, y, color.RGBA{255, 255, 255, alpha})
				}
			}
		}
	}

	// Speaker pill — top center
	spkCX, spkCY := s*0.5, s*0.112
	spkHW, spkHH := s*0.13, s*0.025
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			if inRR(fx, fy, spkCX-spkHW, spkCY-spkHH, spkCX+spkHW, spkCY+spkHH, spkHH) {
				img.SetRGBA(x, y, color.RGBA{255, 255, 255, 160})
			}
		}
	}

	// Home button — white circle at bottom
	hcx, hcy, hr := s*0.5, s*0.875, s*0.065
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			dx, dy := fx-hcx, fy-hcy
			if dx*dx+dy*dy <= hr*hr {
				img.SetRGBA(x, y, color.RGBA{255, 255, 255, 255})
			}
		}
	}

	return img
}

func pngOf(img image.Image) []byte {
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func main() {
	sizes := []int{16, 24, 32, 48, 64, 128, 256}
	pngs := make([][]byte, len(sizes))
	for i, s := range sizes {
		pngs[i] = pngOf(drawPhone(s))
	}

	count := len(sizes)
	offset := uint32(6 + 16*count)

	f, err := os.Create("icon.ico")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	// ICONDIR header
	f.Write([]byte{0, 0, 1, 0, byte(count), 0})

	// Directory entries
	for i, sz := range sizes {
		w := sz
		if w >= 256 {
			w = 0
		}
		e := make([]byte, 16)
		e[0] = byte(w)
		e[1] = byte(w)
		binary.LittleEndian.PutUint16(e[4:], 1)
		binary.LittleEndian.PutUint16(e[6:], 32)
		binary.LittleEndian.PutUint32(e[8:], uint32(len(pngs[i])))
		binary.LittleEndian.PutUint32(e[12:], offset)
		f.Write(e)
		offset += uint32(len(pngs[i]))
	}

	for _, data := range pngs {
		f.Write(data)
	}
}
