package platform

import (
	"math"
	"math/rand/v2"
	"runtime"
	"testing"
)

// nv12Reference is the floating-point BT.709 limited-range conversion of one RGB colour.
func nv12Reference(r, g, b float64) (y, cb, cr float64) {
	const kr, kb = 0.2126, 0.0722
	ey := kr*r + (1-kr-kb)*g + kb*b
	return 16 + 219*ey/255, 128 + 224*(b-ey)/(2*(1-kb))/255, 128 + 224*(r-ey)/(2*(1-kr))/255
}

func TestNV12KnownColours(t *testing.T) {
	c := newNV12Converter(1)
	defer c.close()
	for _, tc := range []struct {
		name      string
		r, g, b   byte
		y, cb, cr byte
	}{
		{"white", 255, 255, 255, 235, 128, 128},
		{"black", 0, 0, 0, 16, 128, 128},
		{"red", 255, 0, 0, 63, 102, 240},
		{"green", 0, 255, 0, 173, 42, 26},
		{"blue", 0, 0, 255, 32, 240, 118},
		{"grey", 128, 128, 128, 126, 128, 128},
	} {
		src := make([]byte, 2*2*4)
		for i := 0; i < len(src); i += 4 {
			src[i], src[i+1], src[i+2], src[i+3] = tc.b, tc.g, tc.r, 255
		}
		dy, duv := make([]byte, 4), make([]byte, 2)
		c.convert(dy, 2, duv, 2, src, 8, 2, 2)
		for i, v := range dy {
			if v != tc.y {
				t.Errorf("%s: Y[%d] = %d, want %d", tc.name, i, v, tc.y)
			}
		}
		if duv[0] != tc.cb || duv[1] != tc.cr {
			t.Errorf("%s: CbCr = %d/%d, want %d/%d", tc.name, duv[0], duv[1], tc.cb, tc.cr)
		}
		y, cb, cr := nv12Reference(float64(tc.r), float64(tc.g), float64(tc.b))
		if math.Round(y) != float64(tc.y) || math.Round(cb) != float64(tc.cb) || math.Round(cr) != float64(tc.cr) {
			t.Errorf("%s: table entry disagrees with reference %.2f/%.2f/%.2f", tc.name, y, cb, cr)
		}
	}
}

// TestNV12Bands converts a random image with padded strides on several worker counts (bands
// that don't divide the height evenly) and checks every sample against the float reference,
// that all worker counts agree exactly, and that stride padding is never written.
func TestNV12Bands(t *testing.T) {
	const w, h = 38, 46
	const srcStride, yStride, uvStride = w*4 + 12, w + 6, w + 10
	rng := rand.New(rand.NewPCG(1, 2))
	src := make([]byte, srcStride*h)
	for i := range src {
		src[i] = byte(rng.UintN(256))
	}
	var first []byte
	for _, workers := range []int{1, 2, 3, 5, 8, 64} {
		dy := make([]byte, yStride*h)
		duv := make([]byte, uvStride*h/2)
		for i := range dy {
			dy[i] = 0xAA
		}
		for i := range duv {
			duv[i] = 0xAA
		}
		c := newNV12Converter(workers)
		c.convert(dy, yStride, duv, uvStride, src, srcStride, w, h)
		c.close()

		for y := range h {
			for x := range yStride {
				got := dy[y*yStride+x]
				if x >= w {
					if got != 0xAA {
						t.Fatalf("workers=%d: Y padding (%d,%d) written", workers, x, y)
					}
					continue
				}
				p := src[y*srcStride+x*4:]
				ry, _, _ := nv12Reference(float64(p[2]), float64(p[1]), float64(p[0]))
				if math.Abs(float64(got)-ry) > 0.51 {
					t.Fatalf("workers=%d: Y(%d,%d) = %d, want %.2f", workers, x, y, got, ry)
				}
			}
		}
		for y := range h / 2 {
			for x := range uvStride / 2 {
				i := y*uvStride + x*2
				if x*2 >= w {
					if duv[i] != 0xAA || duv[i+1] != 0xAA {
						t.Fatalf("workers=%d: CbCr padding (%d,%d) written", workers, x, y)
					}
					continue
				}
				var r, g, b float64
				for _, o := range [][2]int{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
					p := src[(y*2+o[1])*srcStride+(x*2+o[0])*4:]
					b, g, r = b+float64(p[0])/4, g+float64(p[1])/4, r+float64(p[2])/4
				}
				_, rcb, rcr := nv12Reference(r, g, b)
				if math.Abs(float64(duv[i])-rcb) > 0.51 || math.Abs(float64(duv[i+1])-rcr) > 0.51 {
					t.Fatalf("workers=%d: CbCr(%d,%d) = %d/%d, want %.2f/%.2f", workers, x, y, duv[i], duv[i+1], rcb, rcr)
				}
			}
		}
		all := append(dy, duv...)
		if first == nil {
			first = all
		} else if string(first) != string(all) {
			t.Fatalf("workers=%d: output differs from single-worker output", workers)
		}
	}
}

func BenchmarkNV12_1080p(b *testing.B) {
	const w, h = 1920, 1080
	src := make([]byte, w*4*h)
	dy, duv := make([]byte, w*h), make([]byte, w*h/2)
	c := newNV12Converter(runtime.GOMAXPROCS(0))
	defer c.close()
	b.SetBytes(int64(len(src)))
	for b.Loop() {
		c.convert(dy, w, duv, w, src, w*4, w, h)
	}
}
