package platform

import (
	"math"
	"sync"
)

// BT.709 limited-range BGRA→YCbCr coefficients in 16-bit fixed point. The chroma green
// terms are derived so each chroma row sums to exactly zero: greys map to 128 without bias.
var nv12Coef = func() (c struct{ yr, yg, yb, ur, ug, ub, vr, vg, vb int32 }) {
	const kr, kb = 0.2126, 0.0722
	const kg = 1 - kr - kb
	const ys, cs = 219.0 / 255 * 65536, 224.0 / 255 * 65536
	fix := func(f float64) int32 { return int32(math.Round(f)) }
	c.yr, c.yg, c.yb = fix(kr*ys), fix(kg*ys), fix(kb*ys)
	c.ub = fix(0.5 * cs)
	c.ur = fix(-kr / (2 * (1 - kb)) * cs)
	c.ug = -c.ub - c.ur
	c.vr = fix(0.5 * cs)
	c.vb = fix(-kb / (2 * (1 - kr)) * cs)
	c.vg = -c.vr - c.vb
	return c
}()

// nv12Converter converts BGRA to NV12 (BT.709, limited range) with a fixed pool of workers
// that each take a band of rows, so a frame costs no goroutine starts or allocations.
type nv12Converter struct {
	workers int
	bands   chan nv12Band
	wg      sync.WaitGroup
}

type nv12Band struct {
	dstY, dstUV          []byte
	yStride, uvStride    int
	src                  []byte
	srcStride, w, y0, y1 int
}

func newNV12Converter(workers int) *nv12Converter {
	c := &nv12Converter{workers: max(workers, 1), bands: make(chan nv12Band)}
	// The calling goroutine converts one band itself.
	for range c.workers - 1 {
		go func() {
			for b := range c.bands {
				b.convert()
				c.wg.Done()
			}
		}()
	}
	return c
}

// convert writes the w×h BGRA image src (srcStride bytes per row) as NV12: the Y plane into
// dstY (yStride bytes per row) and the interleaved CbCr plane into dstUV (uvStride bytes per
// row). w and h must be even. Each chroma sample averages its 2×2 block (centred siting).
func (c *nv12Converter) convert(dstY []byte, yStride int, dstUV []byte, uvStride int, src []byte, srcStride, w, h int) {
	pairs := h / 2
	n := min(c.workers, pairs)
	if n <= 1 {
		nv12Band{dstY, dstUV, yStride, uvStride, src, srcStride, w, 0, h}.convert()
		return
	}
	c.wg.Add(n - 1)
	for i := range n - 1 {
		c.bands <- nv12Band{dstY, dstUV, yStride, uvStride, src, srcStride, w, pairs * i / n * 2, pairs * (i + 1) / n * 2}
	}
	nv12Band{dstY, dstUV, yStride, uvStride, src, srcStride, w, pairs * (n - 1) / n * 2, h}.convert()
	c.wg.Wait()
}

func (c *nv12Converter) close() { close(c.bands) }

func (b nv12Band) convert() {
	k := nv12Coef
	const yOff = 16<<16 + 1<<15
	const cOff = 128<<18 + 1<<17
	w := b.w
	for y := b.y0; y < b.y1; y += 2 {
		s0 := b.src[y*b.srcStride:][:w*4]
		s1 := b.src[(y+1)*b.srcStride:][:w*4]
		d0 := b.dstY[y*b.yStride:][:w]
		d1 := b.dstY[(y+1)*b.yStride:][:w]
		uv := b.dstUV[y/2*b.uvStride:][:w]
		for x := 0; x+1 < w; x += 2 {
			p := s0[x*4 : x*4+8 : x*4+8]
			q := s1[x*4 : x*4+8 : x*4+8]
			b0, g0, r0 := int32(p[0]), int32(p[1]), int32(p[2])
			b1, g1, r1 := int32(p[4]), int32(p[5]), int32(p[6])
			b2, g2, r2 := int32(q[0]), int32(q[1]), int32(q[2])
			b3, g3, r3 := int32(q[4]), int32(q[5]), int32(q[6])
			d0[x] = uint8((k.yr*r0 + k.yg*g0 + k.yb*b0 + yOff) >> 16)
			d0[x+1] = uint8((k.yr*r1 + k.yg*g1 + k.yb*b1 + yOff) >> 16)
			d1[x] = uint8((k.yr*r2 + k.yg*g2 + k.yb*b2 + yOff) >> 16)
			d1[x+1] = uint8((k.yr*r3 + k.yg*g3 + k.yb*b3 + yOff) >> 16)
			r, g, bl := r0+r1+r2+r3, g0+g1+g2+g3, b0+b1+b2+b3
			c := uv[x : x+2 : x+2]
			c[0] = uint8((k.ur*r + k.ug*g + k.ub*bl + cOff) >> 18)
			c[1] = uint8((k.vr*r + k.vg*g + k.vb*bl + cOff) >> 18)
		}
	}
}
