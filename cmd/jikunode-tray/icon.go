package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
)

// iconState selects the tray icon colour.
type iconState int

const (
	iconOff        iconState = iota // node stopped or not bound: grey
	iconConnecting                  // connecting, or paused: amber
	iconConnected                   // connected and running: green
	iconError                       // repeated failure: red
)

var iconColors = map[iconState]color.NRGBA{
	iconOff:        {0x8a, 0x8f, 0x98, 0xff},
	iconConnecting: {0xe0, 0x9b, 0x1a, 0xff},
	iconConnected:  {0x1f, 0xa9, 0x5a, 0xff},
	iconError:      {0xd6, 0x3b, 0x3b, 0xff},
}

// iconSizes are the frames put in each .ico so Windows can pick a crisp one
// for any display scale.
var iconSizes = []int{16, 24, 32, 48}

// renderIcon returns a .ico file (PNG-compressed frames, supported since
// Windows Vista) showing a coloured disc with a white node mark. Drawing it in
// code keeps binary assets out of the repository.
func renderIcon(state iconState) []byte {
	frames := make([][]byte, len(iconSizes))
	for i, size := range iconSizes {
		var buf bytes.Buffer
		_ = png.Encode(&buf, drawFrame(size, iconColors[state]))
		frames[i] = buf.Bytes()
	}
	var out bytes.Buffer
	// ICONDIR: reserved, type 1 (icon), image count.
	_ = binary.Write(&out, binary.LittleEndian, [3]uint16{0, 1, uint16(len(frames))})
	offset := 6 + 16*len(frames)
	for i, size := range iconSizes {
		dim := byte(size)
		if size >= 256 {
			dim = 0
		}
		// ICONDIRENTRY: width, height, colours, reserved, planes, bpp, size, offset.
		out.Write([]byte{dim, dim, 0, 0})
		_ = binary.Write(&out, binary.LittleEndian, [2]uint16{1, 32})
		_ = binary.Write(&out, binary.LittleEndian, [2]uint32{uint32(len(frames[i])), uint32(offset)})
		offset += len(frames[i])
	}
	for _, f := range frames {
		out.Write(f)
	}
	return out.Bytes()
}

// drawFrame draws one anti-aliased frame: a filled disc in fill with a white
// ring and centre dot (a node with its link).
func drawFrame(size int, fill color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 4 // supersampling per axis
	c := float64(size) / 2
	outer := c - 0.5
	ringOut, ringIn := outer*0.62, outer*0.46
	dot := outer * 0.2
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var discHits, whiteHits int
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					px := float64(x) + (float64(sx)+0.5)/ss
					py := float64(y) + (float64(sy)+0.5)/ss
					d := math.Hypot(px-c, py-c)
					if d > outer {
						continue
					}
					discHits++
					if (d <= ringOut && d >= ringIn) || d <= dot {
						whiteHits++
					}
				}
			}
			if discHits == 0 {
				continue
			}
			total := float64(ss * ss)
			w := float64(whiteHits) / float64(discHits)
			mix := func(a uint8) uint8 { return uint8(float64(a)*(1-w) + 255*w + 0.5) }
			img.SetNRGBA(x, y, color.NRGBA{mix(fill.R), mix(fill.G), mix(fill.B), uint8(255*float64(discHits)/total + 0.5)})
		}
	}
	return img
}
