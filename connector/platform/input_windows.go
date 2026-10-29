//go:build windows

package platform

import (
	"fmt"
	"math"
	"unsafe"
)

var (
	procSendInput        = modUser32.NewProc("SendInput")
	procGetSystemMetrics = modUser32.NewProc("GetSystemMetrics")
)

const (
	inputMouse    = 0
	inputKeyboard = 1

	mouseeventfMove        = 0x0001
	mouseeventfLeftDown    = 0x0002
	mouseeventfLeftUp      = 0x0004
	mouseeventfRightDown   = 0x0008
	mouseeventfRightUp     = 0x0010
	mouseeventfMiddleDown  = 0x0020
	mouseeventfMiddleUp    = 0x0040
	mouseeventfWheel       = 0x0800
	mouseeventfHWheel      = 0x1000
	mouseeventfVirtualDesk = 0x4000
	mouseeventfAbsolute    = 0x8000

	keyeventfExtendedKey = 0x0001
	keyeventfKeyUp       = 0x0002
	keyeventfScanCode    = 0x0008

	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79

	wheelDelta = 120
)

// MOUSEINPUT
type mouseInput struct {
	Dx        int32
	Dy        int32
	MouseData uint32
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

// KEYBDINPUT
type keybdInput struct {
	Vk        uint16
	Scan      uint16
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

// INPUT: the union is sized/aligned by its largest arm, MOUSEINPUT.
type input struct {
	Type uint32
	mi   mouseInput
}

func (in *input) ki() *keybdInput { return (*keybdInput)(unsafe.Pointer(&in.mi)) }

// INPUT is 40 bytes on 64-bit and 28 bytes on 32-bit Windows.
var (
	_ [3*unsafe.Sizeof(uintptr(0)) + 16]byte = [unsafe.Sizeof(input{})]byte{}
	_ [unsafe.Sizeof(mouseInput{}) - unsafe.Sizeof(keybdInput{})]byte
)

func sendInputs(ins []input) error {
	if len(ins) == 0 {
		return nil
	}
	n, _, err := procSendInput.Call(uintptr(len(ins)), uintptr(unsafe.Pointer(&ins[0])), unsafe.Sizeof(ins[0]))
	if int(n) != len(ins) {
		return fmt.Errorf("SendInput injected %d of %d events (blocked by UIPI or secure desktop?): %w", n, len(ins), err)
	}
	return nil
}

func systemMetric(index int) int {
	r, _, _ := procGetSystemMetrics.Call(uintptr(index))
	return int(int32(r))
}

// absoluteMouse builds an absolute move to desktop pixel (px,py), normalized
// over the virtual screen.
func absoluteMouse(px, py int) input {
	vx, vy := systemMetric(smXVirtualScreen), systemMetric(smYVirtualScreen)
	vw, vh := systemMetric(smCXVirtualScreen), systemMetric(smCYVirtualScreen)
	norm := func(p, origin, size int) int32 {
		if size <= 0 {
			return 0
		}
		v := (int64(p-origin)*65536 + int64(size)/2) / int64(size)
		return int32(min(max(v, 0), 65535))
	}
	return input{Type: inputMouse, mi: mouseInput{
		Dx:    norm(px, vx, vw),
		Dy:    norm(py, vy, vh),
		Flags: mouseeventfMove | mouseeventfAbsolute | mouseeventfVirtualDesk,
	}}
}

func mouseButtonFlags(button int, down bool) (uint32, bool) {
	switch button {
	case 0:
		if down {
			return mouseeventfLeftDown, true
		}
		return mouseeventfLeftUp, true
	case 1:
		if down {
			return mouseeventfMiddleDown, true
		}
		return mouseeventfMiddleUp, true
	case 2:
		if down {
			return mouseeventfRightDown, true
		}
		return mouseeventfRightUp, true
	}
	return 0, false
}

func mouseButtonInput(button int, down bool) (input, bool) {
	flags, ok := mouseButtonFlags(button, down)
	return input{Type: inputMouse, mi: mouseInput{Flags: flags}}, ok
}

// wheelAccumulator converts DOM pixel deltas into WHEEL_DELTA units (100 px
// per notch), carrying fractional remainders across events.
type wheelAccumulator struct{ x, y float64 }

func (a *wheelAccumulator) inputs(dx, dy float64) []input {
	var out []input
	if !math.IsNaN(dy) && !math.IsInf(dy, 0) {
		a.y += -dy * wheelDelta / 100
		if n := math.Round(a.y); n != 0 {
			a.y -= n
			out = append(out, input{Type: inputMouse, mi: mouseInput{MouseData: uint32(int32(n)), Flags: mouseeventfWheel}})
		}
	}
	if !math.IsNaN(dx) && !math.IsInf(dx, 0) {
		a.x += dx * wheelDelta / 100
		if n := math.Round(a.x); n != 0 {
			a.x -= n
			out = append(out, input{Type: inputMouse, mi: mouseInput{MouseData: uint32(int32(n)), Flags: mouseeventfHWheel}})
		}
	}
	return out
}

func keyInput(k scanKey, down bool) input {
	in := input{Type: inputKeyboard}
	ki := in.ki()
	if k.vk != 0 {
		ki.Vk = k.vk
		ki.Scan = k.scan
	} else {
		ki.Scan = k.scan
		ki.Flags = keyeventfScanCode
	}
	if k.extended {
		ki.Flags |= keyeventfExtendedKey
	}
	if !down {
		ki.Flags |= keyeventfKeyUp
	}
	return in
}
