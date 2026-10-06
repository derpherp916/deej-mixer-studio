package main

import (
	"image"
	"image/color"
	"math"
)

// AppVersion is overridden at build time with -ldflags "-X main.AppVersion=x.y.z".
var AppVersion = "2.0.0"

// drawIcon renders the app icon (a knob with a lit level ring) at the given size.
func drawIcon(size int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	c := float64(size) / 2
	ringOn := color.RGBA{0x30, 0xA0, 0xFF, 0xFF}
	ringOff := color.RGBA{0x3A, 0x40, 0x4D, 0xFF}
	body := color.RGBA{0x24, 0x28, 0x31, 0xFF}
	edge := color.RGBA{0x55, 0x5D, 0x6B, 0xFF}
	white := color.RGBA{0xF2, 0xF4, 0xF8, 0xFF}
	s := float64(size)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			// 4x supersampling for smooth edges
			var r, g, b, a float64
			for sy := 0; sy < 4; sy++ {
				for sx := 0; sx < 4; sx++ {
					px := float64(x) + (float64(sx)+0.5)/4 - c
					py := float64(y) + (float64(sy)+0.5)/4 - c
					d := math.Hypot(px, py) / s
					ang := math.Atan2(px, -py) * 180 / math.Pi // 0 = up, clockwise
					var col color.RGBA
					hit := true
					switch {
					case d <= 0.30:
						col = body
						// pointer
						if math.Abs(px) < s*0.045 && py < 0 && py > -s*0.26 {
							col = white
						}
					case d <= 0.335:
						col = edge
					case d >= 0.39 && d <= 0.49 && math.Abs(ang) <= 135:
						if ang <= 45 {
							col = ringOn
						} else {
							col = ringOff
						}
					default:
						hit = false
					}
					if hit {
						r += float64(col.R)
						g += float64(col.G)
						b += float64(col.B)
						a += 255
					}
				}
			}
			if a > 0 {
				img.SetRGBA(x, y, color.RGBA{uint8(r / (a / 255)), uint8(g / (a / 255)), uint8(b / (a / 255)), uint8(a / 16)})
			}
		}
	}
	return img
}
