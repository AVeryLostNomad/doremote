//go:build windows

package platform

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Desktop Duplication through raw COM vtables (Windows builds are CGO-free). Vtable indices
// count the inherited methods: IUnknown (0-2), IDXGIObject (3-6), ID3D11DeviceChild (3-6),
// ID3D11Resource (7-9), in the declaration order of the Windows SDK's dxgi.h, dxgi1_2.h,
// dxgi1_5.h and d3d11.h.

var (
	modD3D11 = windows.NewLazySystemDLL("d3d11.dll")
	modDXGI  = windows.NewLazySystemDLL("dxgi.dll")

	procD3D11CreateDevice  = modD3D11.NewProc("D3D11CreateDevice")
	procCreateDXGIFactory1 = modDXGI.NewProc("CreateDXGIFactory1")
)

var (
	iidIDXGIFactory1   = windows.GUID{Data1: 0x770aae78, Data2: 0xf26f, Data3: 0x4dba, Data4: [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}
	iidIDXGIOutput1    = windows.GUID{Data1: 0x00cddea8, Data2: 0x939b, Data3: 0x4b83, Data4: [8]byte{0xa3, 0x40, 0xa6, 0x85, 0x22, 0x66, 0x66, 0xcc}}
	iidIDXGIOutput5    = windows.GUID{Data1: 0x80a07424, Data2: 0xab52, Data3: 0x42eb, Data4: [8]byte{0x83, 0x3c, 0x0c, 0x42, 0xfd, 0x28, 0x2d, 0x98}}
	iidID3D11Texture2D = windows.GUID{Data1: 0x6f15aaf2, Data2: 0xd208, Data3: 0x4e89, Data4: [8]byte{0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c}}
)

const (
	vtQueryInterface = 0
	vtRelease        = 2

	vtFactory1EnumAdapters1 = 12 // IDXGIFactory 7-11, IDXGIFactory1 12-13
	vtAdapterEnumOutputs    = 7
	vtOutputGetDesc         = 7
	vtOutput1DuplicateOut   = 22 // IDXGIOutput 7-18, IDXGIOutput1 19-22
	vtOutput5DuplicateOut1  = 26 // IDXGIOutput2 23, IDXGIOutput3 24, IDXGIOutput4 25

	vtDuplGetDesc             = 7
	vtDuplAcquireNextFrame    = 8
	vtDuplGetFramePointerShap = 11 // after GetFrameDirtyRects 9, GetFrameMoveRects 10
	vtDuplMapDesktopSurface   = 12
	vtDuplUnMapDesktopSurface = 13
	vtDuplReleaseFrame        = 14

	vtDeviceCreateTexture2D = 5 // CreateBuffer 3, CreateTexture1D 4
	vtContextMap            = 14
	vtContextUnmap          = 15
	vtContextCopyResource   = 47
	vtTexture2DGetDesc      = 10
)

const (
	dxgiFormatB8G8R8A8UNorm = 87
	dxgiModeRotationIdent   = 1
	d3dDriverTypeUnknown    = 0
	d3d11SDKVersion         = 7
	d3d11UsageStaging       = 3
	d3d11CPUAccessRead      = 0x20000
	d3d11MapRead            = 1

	ptrShapeMonochrome  = 1
	ptrShapeColor       = 2
	ptrShapeMaskedColor = 4
)

// HRESULTs from winerror.h.
const (
	hrEAccessDenied            = 0x80070005
	hrDXGINotFound             = 0x887A0002
	hrDXGIUnsupported          = 0x887A0004
	hrDXGIModeChangeInProgress = 0x887A0025
	hrDXGIAccessLost           = 0x887A0026
	hrDXGIWaitTimeout          = 0x887A0027
	hrDXGISessionDisconnected  = 0x887A0028
)

// DXGI_OUTPUT_DESC
type outputDesc struct {
	DeviceName         [32]uint16
	DesktopCoordinates struct{ Left, Top, Right, Bottom int32 }
	AttachedToDesktop  int32
	Rotation           uint32
	Monitor            windows.Handle
}

// DXGI_OUTDUPL_DESC (DXGI_MODE_DESC inlined).
type outduplDesc struct {
	Width, Height              uint32
	RefreshNum, RefreshDen     uint32
	Format                     uint32
	ScanlineOrdering, Scaling  uint32
	Rotation                   uint32
	DesktopImageInSystemMemory int32
}

// DXGI_OUTDUPL_FRAME_INFO
type outduplFrameInfo struct {
	LastPresentTime           int64
	LastMouseUpdateTime       int64
	AccumulatedFrames         uint32
	RectsCoalesced            int32
	ProtectedContentMaskedOut int32
	PointerPosition           point // DXGI_OUTDUPL_POINTER_POSITION: top-left of the shape
	PointerVisible            int32
	TotalMetadataBufferSize   uint32
	PointerShapeBufferSize    uint32
}

// DXGI_OUTDUPL_POINTER_SHAPE_INFO
type pointerShapeInfo struct {
	Type, Width, Height, Pitch uint32
	HotSpot                    point
}

// D3D11_TEXTURE2D_DESC
type texture2DDesc struct {
	Width, Height, MipLevels, ArraySize uint32
	Format                              uint32
	SampleCount, SampleQuality          uint32
	Usage                               uint32
	BindFlags, CPUAccessFlags           uint32
	MiscFlags                           uint32
}

// D3D11_MAPPED_SUBRESOURCE
type mappedSubresource struct {
	Data       unsafe.Pointer
	RowPitch   uint32
	DepthPitch uint32
}

// DXGI_MAPPED_RECT
type mappedRect struct {
	Pitch int32
	Bits  unsafe.Pointer
}

var (
	_ [64 + 16 + 4 + 4 + unsafe.Sizeof(uintptr(0))]byte = [unsafe.Sizeof(outputDesc{})]byte{}
	_ [36]byte                                          = [unsafe.Sizeof(outduplDesc{})]byte{}
	_ [48]byte                                          = [unsafe.Sizeof(outduplFrameInfo{})]byte{}
	_ [28]byte                                          = [unsafe.Offsetof(outduplFrameInfo{}.PointerPosition)]byte{}
	_ [24]byte                                          = [unsafe.Sizeof(pointerShapeInfo{})]byte{}
	_ [44]byte                                          = [unsafe.Sizeof(texture2DDesc{})]byte{}
	_ [unsafe.Sizeof(uintptr(0)) + 8]byte               = [unsafe.Sizeof(mappedSubresource{})]byte{}
	_ [2 * unsafe.Sizeof(uintptr(0))]byte               = [unsafe.Sizeof(mappedRect{})]byte{}
)

// comObject is a COM interface pointer; only the OS ever allocates one.
type comObject struct{ vtbl *[128]uintptr }

// call invokes vtable method `method` on o. uintptrescapes keeps Go memory passed as
// uintptr(unsafe.Pointer(&x)) on the heap and alive for the call, as for LazyProc.Call.
//
//go:uintptrescapes
func (o *comObject) call(method int, args ...uintptr) uint32 {
	var a [8]uintptr
	a[0] = uintptr(unsafe.Pointer(o))
	n := copy(a[1:], args)
	r, _, _ := syscall.SyscallN(o.vtbl[method], a[:n+1]...)
	return uint32(r)
}

func (o *comObject) release() {
	if o != nil {
		o.call(vtRelease)
	}
}

func (o *comObject) query(iid *windows.GUID, out **comObject) uint32 {
	return o.call(vtQueryInterface, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(out)))
}

func failed(hr uint32) bool { return int32(hr) < 0 }

type hresultError struct {
	op string
	hr uint32
}

func (e *hresultError) Error() string { return fmt.Sprintf("%s: HRESULT 0x%08X", e.op, e.hr) }

func hrErr(op string, hr uint32) error { return &hresultError{op, hr} }

// ddaTransient reports errors that mean "no image right now" rather than "this machine can't
// duplicate": a secure desktop (UAC, lock screen), a disconnected session or a mode change.
func ddaTransient(err error) bool {
	var he *hresultError
	if !errors.As(err, &he) {
		return false
	}
	switch he.hr {
	case hrEAccessDenied, hrDXGIAccessLost, hrDXGISessionDisconnected, hrDXGIModeChangeInProgress:
		return true
	}
	return false
}

// ddaCapturer duplicates one output with DXGI Desktop Duplication. img holds the latest desktop
// image with the pointer drawn in (BGRA top-down, stride 4*w).
type ddaCapturer struct {
	device, context, dupl, staging *comObject
	w, h                           int
	sysMem                         bool // desktop image readable via MapDesktopSurface
	img                            []byte
	haveImage                      bool

	// DDA doesn't composite a hardware pointer into the image; it reports it separately
	// (PointerVisible=0 when the OS drew a software pointer into the image itself), so the
	// pointer is drawn here from GetFramePointerShape. Drawing it through GDI instead
	// (GetCursorInfo+DrawIconEx) would double a software pointer and need a DIB per frame.
	ptrShape   []byte
	ptrInfo    pointerShapeInfo // Type 0: no usable shape
	ptrX, ptrY int
	ptrVisible bool
	// Pixels of img under the drawn pointer, restored before drawing it elsewhere.
	under      []byte
	underRect  [4]int // x0, y0, x1, y1
	underValid bool

	// Per-frame out-parameters, kept here so passing them to COM doesn't allocate.
	fi       outduplFrameInfo
	res, tex *comObject
	mapped   mappedSubresource
	sysRect  mappedRect
}

// newDDACapturer duplicates the output whose GDI device name is gdiName, on a D3D11 device
// created on the adapter that owns it (DuplicateOutput requires that adapter).
func newDDACapturer(gdiName string) (*ddaCapturer, error) {
	if err := procCreateDXGIFactory1.Find(); err != nil {
		return nil, err
	}
	if err := procD3D11CreateDevice.Find(); err != nil {
		return nil, err
	}
	var factory *comObject
	r, _, _ := procCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidIDXGIFactory1)), uintptr(unsafe.Pointer(&factory)))
	if failed(uint32(r)) {
		return nil, hrErr("CreateDXGIFactory1", uint32(r))
	}
	defer factory.release()

	adapter, output, rotation, err := findOutput(factory, gdiName)
	if err != nil {
		return nil, err
	}
	defer adapter.release()
	defer output.release()
	if rotation > dxgiModeRotationIdent {
		// The duplicated surface stays unrotated; GDI captures rotated desktops correctly.
		return nil, fmt.Errorf("output %s is rotated (DXGI_MODE_ROTATION %d)", gdiName, rotation)
	}

	c := &ddaCapturer{}
	r, _, _ = procD3D11CreateDevice.Call(uintptr(unsafe.Pointer(adapter)), d3dDriverTypeUnknown, 0, 0, 0, 0,
		d3d11SDKVersion, uintptr(unsafe.Pointer(&c.device)), 0, uintptr(unsafe.Pointer(&c.context)))
	if failed(uint32(r)) {
		return nil, hrErr("D3D11CreateDevice", uint32(r))
	}

	// DuplicateOutput1 is the variant meant for per-monitor-DPI-aware processes; asking for
	// BGRA only makes DXGI convert HDR/fullscreen formats for us.
	var out5, out1 *comObject
	if hr := output.query(&iidIDXGIOutput5, &out5); !failed(hr) {
		format := uint32(dxgiFormatB8G8R8A8UNorm)
		hr = out5.call(vtOutput5DuplicateOut1, uintptr(unsafe.Pointer(c.device)), 0, 1,
			uintptr(unsafe.Pointer(&format)), uintptr(unsafe.Pointer(&c.dupl)))
		out5.release()
		if failed(hr) {
			c.close()
			return nil, hrErr("DuplicateOutput1", hr)
		}
	} else {
		if hr := output.query(&iidIDXGIOutput1, &out1); failed(hr) {
			c.close()
			return nil, hrErr("QueryInterface(IDXGIOutput1)", hr)
		}
		hr := out1.call(vtOutput1DuplicateOut, uintptr(unsafe.Pointer(c.device)), uintptr(unsafe.Pointer(&c.dupl)))
		out1.release()
		if failed(hr) {
			c.close()
			return nil, hrErr("DuplicateOutput", hr)
		}
	}

	var d outduplDesc
	c.dupl.call(vtDuplGetDesc, uintptr(unsafe.Pointer(&d)))
	if d.Rotation > dxgiModeRotationIdent {
		c.close()
		return nil, fmt.Errorf("output %s is rotated (DXGI_MODE_ROTATION %d)", gdiName, d.Rotation)
	}
	if d.Width == 0 || d.Height == 0 {
		c.close()
		return nil, fmt.Errorf("output %s reports empty duplication size", gdiName)
	}
	c.w, c.h = int(d.Width), int(d.Height)
	c.sysMem = d.DesktopImageInSystemMemory != 0
	c.img = make([]byte, 4*c.w*c.h)
	return c, nil
}

// findOutput returns the adapter and output whose DXGI_OUTPUT_DESC.DeviceName is gdiName.
func findOutput(factory *comObject, gdiName string) (adapter, output *comObject, rotation uint32, err error) {
	for i := uintptr(0); ; i++ {
		var a *comObject
		hr := factory.call(vtFactory1EnumAdapters1, i, uintptr(unsafe.Pointer(&a)))
		if hr == hrDXGINotFound {
			return nil, nil, 0, fmt.Errorf("no DXGI output named %s", gdiName)
		}
		if failed(hr) {
			return nil, nil, 0, hrErr("EnumAdapters1", hr)
		}
		for j := uintptr(0); ; j++ {
			var o *comObject
			if failed(a.call(vtAdapterEnumOutputs, j, uintptr(unsafe.Pointer(&o)))) {
				break
			}
			var d outputDesc
			if !failed(o.call(vtOutputGetDesc, uintptr(unsafe.Pointer(&d)))) &&
				strings.EqualFold(windows.UTF16ToString(d.DeviceName[:]), gdiName) {
				return a, o, d.Rotation, nil
			}
			o.release()
		}
		a.release()
	}
}

// next waits up to timeout for a desktop or pointer update and reports whether img changed.
// Errors invalidate the capturer; ddaTransient tells whether recreating it is worthwhile now.
func (c *ddaCapturer) next(timeout time.Duration) (bool, error) {
	ms := uintptr((timeout + time.Millisecond - 1) / time.Millisecond)
	fi := &c.fi
	c.res = nil
	hr := c.dupl.call(vtDuplAcquireNextFrame, ms, uintptr(unsafe.Pointer(fi)), uintptr(unsafe.Pointer(&c.res)))
	if hr == hrDXGIWaitTimeout {
		return false, nil
	}
	if failed(hr) {
		return false, hrErr("AcquireNextFrame", hr)
	}
	// Hold the frame only while copying it: DWM accumulates further updates meanwhile.
	res := c.res
	defer c.dupl.call(vtDuplReleaseFrame)
	defer res.release()

	desktop := fi.LastPresentTime != 0
	if desktop {
		if err := c.copyDesktop(res); err != nil {
			return false, err
		}
		c.haveImage = true
		c.underValid = false // the fresh image has no pointer in it
	}

	pointer := false
	if fi.LastMouseUpdateTime != 0 {
		visible := fi.PointerVisible != 0
		x, y := int(fi.PointerPosition.X), int(fi.PointerPosition.Y)
		pointer = visible != c.ptrVisible || (visible && (x != c.ptrX || y != c.ptrY))
		c.ptrVisible, c.ptrX, c.ptrY = visible, x, y
	}
	if fi.PointerShapeBufferSize != 0 {
		if err := c.fetchPointerShape(fi.PointerShapeBufferSize); err != nil {
			return false, err
		}
		pointer = pointer || c.ptrVisible
	}

	if !c.haveImage || (!desktop && !pointer) {
		return false, nil
	}
	if !desktop {
		c.restoreUnder()
	}
	c.drawPointer()
	return true, nil
}

// copyDesktop reads the acquired desktop image into img.
func (c *ddaCapturer) copyDesktop(res *comObject) error {
	if c.sysMem {
		m := &c.sysRect
		hr := c.dupl.call(vtDuplMapDesktopSurface, uintptr(unsafe.Pointer(m)))
		if !failed(hr) {
			c.copyRows(m.Bits, int(m.Pitch))
			c.dupl.call(vtDuplUnMapDesktopSurface)
			return nil
		}
		if hr != hrDXGIUnsupported {
			return hrErr("MapDesktopSurface", hr)
		}
		c.sysMem = false
	}

	c.tex = nil
	if hr := res.query(&iidID3D11Texture2D, &c.tex); failed(hr) {
		return hrErr("QueryInterface(ID3D11Texture2D)", hr)
	}
	tex := c.tex
	defer tex.release()
	if c.staging == nil {
		var d texture2DDesc
		tex.call(vtTexture2DGetDesc, uintptr(unsafe.Pointer(&d)))
		if d.Format != dxgiFormatB8G8R8A8UNorm || int(d.Width) != c.w || int(d.Height) != c.h {
			return fmt.Errorf("unexpected desktop texture %dx%d format %d", d.Width, d.Height, d.Format)
		}
		sd := texture2DDesc{
			Width: d.Width, Height: d.Height, MipLevels: 1, ArraySize: 1,
			Format: d.Format, SampleCount: 1,
			Usage: d3d11UsageStaging, CPUAccessFlags: d3d11CPUAccessRead,
		}
		if hr := c.device.call(vtDeviceCreateTexture2D, uintptr(unsafe.Pointer(&sd)), 0, uintptr(unsafe.Pointer(&c.staging))); failed(hr) {
			c.staging = nil
			return hrErr("CreateTexture2D(staging)", hr)
		}
	}
	c.context.call(vtContextCopyResource, uintptr(unsafe.Pointer(c.staging)), uintptr(unsafe.Pointer(tex)))
	m := &c.mapped
	if hr := c.context.call(vtContextMap, uintptr(unsafe.Pointer(c.staging)), 0, d3d11MapRead, 0, uintptr(unsafe.Pointer(m))); failed(hr) {
		return hrErr("Map(staging)", hr)
	}
	c.copyRows(m.Data, int(m.RowPitch))
	c.context.call(vtContextUnmap, uintptr(unsafe.Pointer(c.staging)), 0)
	return nil
}

func (c *ddaCapturer) copyRows(src unsafe.Pointer, pitch int) {
	row := 4 * c.w
	if pitch == row {
		copy(c.img, unsafe.Slice((*byte)(src), row*c.h))
		return
	}
	s := unsafe.Slice((*byte)(src), pitch*(c.h-1)+row)
	for y := range c.h {
		copy(c.img[y*row:(y+1)*row], s[y*pitch:])
	}
}

func (c *ddaCapturer) fetchPointerShape(size uint32) error {
	if uint32(len(c.ptrShape)) < size {
		c.ptrShape = make([]byte, size)
	}
	var got uint32
	var info pointerShapeInfo
	hr := c.dupl.call(vtDuplGetFramePointerShap, uintptr(size), uintptr(unsafe.Pointer(&c.ptrShape[0])),
		uintptr(unsafe.Pointer(&got)), uintptr(unsafe.Pointer(&info)))
	if hr == hrDXGIAccessLost {
		return hrErr("GetFramePointerShape", hr)
	}
	c.ptrInfo = pointerShapeInfo{}
	if failed(hr) || info.Width == 0 || info.Height == 0 {
		return nil
	}
	rowBytes := 4 * int(info.Width)
	if info.Type == ptrShapeMonochrome {
		rowBytes = (int(info.Width) + 7) / 8
	}
	if int(info.Pitch) < rowBytes || int(info.Pitch)*int(info.Height) > int(min(got, size)) {
		return nil
	}
	switch info.Type {
	case ptrShapeMonochrome, ptrShapeColor, ptrShapeMaskedColor:
		c.ptrInfo = info
	}
	return nil
}

func (c *ddaCapturer) restoreUnder() {
	if !c.underValid {
		return
	}
	x0, y0, x1, y1 := c.underRect[0], c.underRect[1], c.underRect[2], c.underRect[3]
	n := 4 * (x1 - x0)
	for y := y0; y < y1; y++ {
		copy(c.img[4*(y*c.w+x0):], c.under[(y-y0)*n:(y-y0+1)*n])
	}
	c.underValid = false
}

// drawPointer blends the pointer shape into img at its position, saving what it covers.
// Blending follows Microsoft's desktop duplication sample.
func (c *ddaCapturer) drawPointer() {
	info := &c.ptrInfo
	if !c.ptrVisible || info.Type == 0 {
		return
	}
	pw, ph := int(info.Width), int(info.Height)
	if info.Type == ptrShapeMonochrome {
		ph /= 2 // AND mask rows, then XOR mask rows
	}
	x0, y0 := max(c.ptrX, 0), max(c.ptrY, 0)
	x1, y1 := min(c.ptrX+pw, c.w), min(c.ptrY+ph, c.h)
	if x0 >= x1 || y0 >= y1 {
		return
	}
	n := 4 * (x1 - x0)
	if cap(c.under) < n*(y1-y0) {
		c.under = make([]byte, n*(y1-y0))
	}
	c.under = c.under[:n*(y1-y0)]
	for y := y0; y < y1; y++ {
		copy(c.under[(y-y0)*n:(y-y0+1)*n], c.img[4*(y*c.w+x0):])
	}
	c.underRect = [4]int{x0, y0, x1, y1}
	c.underValid = true

	pitch, shape := int(info.Pitch), c.ptrShape
	for y := y0; y < y1; y++ {
		sy := y - c.ptrY
		for x := x0; x < x1; x++ {
			sx := x - c.ptrX
			d := c.img[4*(y*c.w+x) : 4*(y*c.w+x)+3]
			switch info.Type {
			case ptrShapeColor: // straight-alpha BGRA
				s := shape[sy*pitch+4*sx : sy*pitch+4*sx+4]
				a := uint32(s[3])
				if a == 0 {
					continue
				}
				for i := range 3 {
					d[i] = byte((uint32(s[i])*a + uint32(d[i])*(255-a) + 127) / 255)
				}
			case ptrShapeMaskedColor: // alpha 0: replace, alpha 0xFF: XOR with the screen
				s := shape[sy*pitch+4*sx : sy*pitch+4*sx+4]
				if s[3] == 0 {
					d[0], d[1], d[2] = s[0], s[1], s[2]
				} else {
					d[0], d[1], d[2] = d[0]^s[0], d[1]^s[1], d[2]^s[2]
				}
			case ptrShapeMonochrome: // screen = (screen AND and) XOR xor, 1 bpp MSB first
				bit := byte(0x80) >> (sx & 7)
				if shape[sy*pitch+sx/8]&bit == 0 {
					d[0], d[1], d[2] = 0, 0, 0
				}
				if shape[(sy+ph)*pitch+sx/8]&bit != 0 {
					d[0], d[1], d[2] = ^d[0], ^d[1], ^d[2]
				}
			}
		}
	}
}

func (c *ddaCapturer) close() {
	c.staging.release()
	c.dupl.release()
	c.context.release()
	c.device.release()
	c.staging, c.dupl, c.context, c.device = nil, nil, nil, nil
}
