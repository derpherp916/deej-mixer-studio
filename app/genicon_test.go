//go:build genicon

package main

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"os"
	"testing"
)

// go test -tags genicon -run GenIcon  -> writes res/app.ico (PNG-compressed entries)
func TestGenIcon(t *testing.T) {
	sizes := []int{16, 20, 24, 32, 40, 48, 64, 256}
	var imgs [][]byte
	for _, s := range sizes {
		var b bytes.Buffer
		png.Encode(&b, drawIcon(s))
		imgs = append(imgs, b.Bytes())
	}
	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, []uint16{0, 1, uint16(len(sizes))})
	offset := 6 + 16*len(sizes)
	for i, s := range sizes {
		w := byte(s)
		if s == 256 {
			w = 0
		}
		out.Write([]byte{w, w, 0, 0})
		binary.Write(&out, binary.LittleEndian, []uint16{1, 32})
		binary.Write(&out, binary.LittleEndian, []uint32{uint32(len(imgs[i])), uint32(offset)})
		offset += len(imgs[i])
	}
	for _, b := range imgs {
		out.Write(b)
	}
	os.MkdirAll("res", 0o755)
	if err := os.WriteFile("res/app.ico", out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Create("res/preview.png")
	png.Encode(f, drawIcon(256))
	f.Close()
}
