//go:build windows

package platform

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procGetDC         = modUser32.NewProc("GetDC")
	procReleaseDC     = modUser32.NewProc("ReleaseDC")
	procGetCursorInfo = modUser32.NewProc("GetCursorInfo")
	procGetIconInfo   = modUser32.NewProc("GetIconInfo")
	procDrawIconEx    = modUser32.NewProc("DrawIconEx")

	procCreateCompatibleDC = modGdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection   = modGdi32.NewProc("CreateDIBSection")
	procSelectObject       = modGdi32.NewProc("SelectObject")
	procBitBlt             = modGdi32.NewProc("BitBlt")
	procDeleteDC           = modGdi32.NewProc("DeleteDC")
	procDeleteObject       = modGdi32.NewProc("DeleteObject")
	procGdiFlush           = modGdi32.NewProc("GdiFlush")
)

const (
	srcCopy       = 0x00CC0020
	captureBlt    = 0x40000000
	dibRGBColors  = 0
	biRGB         = 0
	cursorShowing = 0x1
	diNormal      = 0x3
)

// BITMAPINFOHEADER
type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

// BITMAPINFO with a single (unused) palette entry.
type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [1]uint32
}

type point struct{ X, Y int32 }

// CURSORINFO
type cursorInfo struct {
	Size      uint32
	Flags     uint32
	Cursor    windows.Handle
	ScreenPos point
}

// ICONINFO
type iconInfo struct {
	Icon     int32
	XHotspot uint32
	YHotspot uint32
	Mask     windows.Handle
	Color    windows.Handle
}

var (
	_ [40]byte = [unsafe.Sizeof(bitmapInfoHeader{})]byte{}
	_ [44]byte = [unsafe.Sizeof(bitmapInfo{})]byte{}
)

// gdiCapturer blits a desktop rectangle into a top-down 32bpp DIB section.
type gdiCapturer struct {
	screenDC uintptr
	memDC    uintptr
	bitmap   uintptr
	oldObj   uintptr
	bits     unsafe.Pointer
	w, h     int
}

func newGDICapturer(w, h int) (*gdiCapturer, error) {
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("invalid capture size %dx%d", w, h)
	}
	c := &gdiCapturer{w: w, h: h}
	c.screenDC, _, _ = procGetDC.Call(0)
	if c.screenDC == 0 {
		return nil, errors.New("GetDC(desktop) failed")
	}
	c.memDC, _, _ = procCreateCompatibleDC.Call(c.screenDC)
	if c.memDC == 0 {
		c.close()
		return nil, errors.New("CreateCompatibleDC failed")
	}
	bi := bitmapInfo{Header: bitmapInfoHeader{
		Width:       int32(w),
		Height:      -int32(h), // negative: top-down rows
		Planes:      1,
		BitCount:    32,
		Compression: biRGB,
	}}
	bi.Header.Size = uint32(unsafe.Sizeof(bi.Header))
	var bits unsafe.Pointer
	c.bitmap, _, _ = procCreateDIBSection.Call(c.screenDC, uintptr(unsafe.Pointer(&bi)), dibRGBColors, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if c.bitmap == 0 || bits == nil {
		c.close()
		return nil, fmt.Errorf("CreateDIBSection(%dx%d) failed", w, h)
	}
	c.bits = bits
	c.oldObj, _, _ = procSelectObject.Call(c.memDC, c.bitmap)
	return c, nil
}

// capture copies the desktop rectangle (x,y,w,h) plus the mouse cursor into dst
// (len 4*w*h, BGRA top-down).
func (c *gdiCapturer) capture(x, y int, dst []byte) error {
	r, _, err := procBitBlt.Call(c.memDC, 0, 0, uintptr(c.w), uintptr(c.h), c.screenDC, uintptr(x), uintptr(y), srcCopy|captureBlt)
	if r == 0 {
		return fmt.Errorf("BitBlt: %w", err)
	}
	c.drawCursor(x, y)
	procGdiFlush.Call()
	src := unsafe.Slice((*byte)(c.bits), c.w*c.h*4)
	copy(dst, src)
	// GDI leaves the alpha channel undefined; make frames fully opaque.
	for i := 3; i < len(dst); i += 4 {
		dst[i] = 0xFF
	}
	return nil
}

func (c *gdiCapturer) drawCursor(originX, originY int) {
	ci := cursorInfo{}
	ci.Size = uint32(unsafe.Sizeof(ci))
	if r, _, _ := procGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci))); r == 0 {
		return
	}
	if ci.Flags&cursorShowing == 0 || ci.Cursor == 0 {
		return
	}
	var ii iconInfo
	if r, _, _ := procGetIconInfo.Call(uintptr(ci.Cursor), uintptr(unsafe.Pointer(&ii))); r == 0 {
		return
	}
	if ii.Mask != 0 {
		procDeleteObject.Call(uintptr(ii.Mask))
	}
	if ii.Color != 0 {
		procDeleteObject.Call(uintptr(ii.Color))
	}
	cx := int(ci.ScreenPos.X) - int(ii.XHotspot) - originX
	cy := int(ci.ScreenPos.Y) - int(ii.YHotspot) - originY
	if cx >= c.w || cy >= c.h || cx < -256 || cy < -256 {
		return
	}
	procDrawIconEx.Call(c.memDC, uintptr(cx), uintptr(cy), uintptr(ci.Cursor), 0, 0, 0, 0, diNormal)
}

func (c *gdiCapturer) close() {
	if c.memDC != 0 {
		if c.oldObj != 0 {
			procSelectObject.Call(c.memDC, c.oldObj)
			c.oldObj = 0
		}
		procDeleteDC.Call(c.memDC)
		c.memDC = 0
	}
	if c.bitmap != 0 {
		procDeleteObject.Call(c.bitmap)
		c.bitmap = 0
		c.bits = nil
	}
	if c.screenDC != 0 {
		procReleaseDC.Call(0, c.screenDC)
		c.screenDC = 0
	}
}
