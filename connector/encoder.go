package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"

	"github.com/joey/doremote/connector/platform"
)

const (
	jpegQuality = 70
	tileSize    = 64
	// Above this share of dirty tiles a single full-frame rect is cheaper than many small ones.
	fullFrameRatio = 0.5
)

type rect struct{ x, y, w, h int }

// frameEncoder turns captured frames into update messages that carry only the regions that
// changed since the previous frame, each as a JPEG. Wire format (little-endian):
//
//	u16 rectCount, then per rect: u16 x, u16 y, u16 w, u16 h, u32 jpegLen, jpegLen bytes
type frameEncoder struct {
	prev []byte // BGRA of the last encoded frame
	w, h int
	rgba *image.RGBA
	out  bytes.Buffer
	jpg  bytes.Buffer
}

// encode returns nil when nothing changed. The returned slice is valid until the next call.
func (e *frameEncoder) encode(f *platform.Frame) ([]byte, error) {
	if e.prev == nil || e.w != f.Width || e.h != f.Height {
		e.w, e.h = f.Width, f.Height
		e.prev = make([]byte, f.Width*f.Height*4)
		e.rgba = image.NewRGBA(image.Rect(0, 0, f.Width, f.Height))
		e.copyRows(f, rect{0, 0, f.Width, f.Height})
		return e.write(f, []rect{{0, 0, f.Width, f.Height}})
	}

	rects, dirtyTiles, totalTiles := e.dirtyRects(f)
	if len(rects) == 0 {
		return nil, nil
	}
	if float64(dirtyTiles) > fullFrameRatio*float64(totalTiles) {
		rects = []rect{{0, 0, f.Width, f.Height}}
	}
	for _, r := range rects {
		e.copyRows(f, r)
	}
	return e.write(f, rects)
}

// dirtyRects compares tiles against the previous frame and merges horizontally adjacent dirty
// tiles of a tile row into one rect.
func (e *frameEncoder) dirtyRects(f *platform.Frame) (rects []rect, dirty, total int) {
	for ty := 0; ty < f.Height; ty += tileSize {
		th := min(tileSize, f.Height-ty)
		runStart := -1
		for tx := 0; tx < f.Width; tx += tileSize {
			tw := min(tileSize, f.Width-tx)
			total++
			changed := e.tileChanged(f, tx, ty, tw, th)
			if changed {
				dirty++
				if runStart < 0 {
					runStart = tx
				}
			}
			if !changed && runStart >= 0 {
				rects = append(rects, rect{runStart, ty, tx - runStart, th})
				runStart = -1
			}
		}
		if runStart >= 0 {
			rects = append(rects, rect{runStart, ty, f.Width - runStart, th})
		}
	}
	return rects, dirty, total
}

func (e *frameEncoder) tileChanged(f *platform.Frame, x, y, w, h int) bool {
	for row := y; row < y+h; row++ {
		cur := f.Pix[row*f.Stride+x*4 : row*f.Stride+(x+w)*4]
		old := e.prev[(row*e.w+x)*4 : (row*e.w+x+w)*4]
		if !bytes.Equal(cur, old) {
			return true
		}
	}
	return false
}

// copyRows records r in prev and converts it from BGRA into the RGBA image used for encoding.
func (e *frameEncoder) copyRows(f *platform.Frame, r rect) {
	for row := r.y; row < r.y+r.h; row++ {
		src := f.Pix[row*f.Stride+r.x*4 : row*f.Stride+(r.x+r.w)*4]
		copy(e.prev[(row*e.w+r.x)*4:], src)
		dst := e.rgba.Pix[row*e.rgba.Stride+r.x*4 : row*e.rgba.Stride+(r.x+r.w)*4]
		for i := 0; i < len(src); i += 4 {
			dst[i] = src[i+2]
			dst[i+1] = src[i+1]
			dst[i+2] = src[i]
			dst[i+3] = 255
		}
	}
}

func (e *frameEncoder) write(f *platform.Frame, rects []rect) ([]byte, error) {
	e.out.Reset()
	var hdr [12]byte
	binary.LittleEndian.PutUint16(hdr[:2], uint16(len(rects)))
	e.out.Write(hdr[:2])
	for _, r := range rects {
		e.jpg.Reset()
		sub := e.rgba.SubImage(image.Rect(r.x, r.y, r.x+r.w, r.y+r.h))
		if err := jpeg.Encode(&e.jpg, sub, &jpeg.Options{Quality: jpegQuality}); err != nil {
			return nil, err
		}
		binary.LittleEndian.PutUint16(hdr[0:], uint16(r.x))
		binary.LittleEndian.PutUint16(hdr[2:], uint16(r.y))
		binary.LittleEndian.PutUint16(hdr[4:], uint16(r.w))
		binary.LittleEndian.PutUint16(hdr[6:], uint16(r.h))
		binary.LittleEndian.PutUint32(hdr[8:], uint32(e.jpg.Len()))
		e.out.Write(hdr[:])
		e.out.Write(e.jpg.Bytes())
	}
	return e.out.Bytes(), nil
}
