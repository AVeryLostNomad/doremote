//go:build windows

package platform

import (
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Parsec Virtual Display Driver (https://github.com/nomi-san/parsec-vdd, core/parsec-vdd.h).

// vddAdapterGUID is the device interface GUID of the Parsec Virtual Display Adapter.
// {00b41627-04c4-429e-a26e-0265cf50c8fa}
var vddAdapterGUID = windows.GUID{
	Data1: 0x00b41627,
	Data2: 0x04c4,
	Data3: 0x429e,
	Data4: [8]byte{0xa2, 0x6e, 0x02, 0x65, 0xcf, 0x50, 0xc8, 0xfa},
}

const (
	vddDisplayID   = "PSCCDD0"   // monitor device ID (HKLM\SYSTEM\CurrentControlSet\Enum\DISPLAY)
	vddDisplayName = "ParsecVDA" // monitor friendly name

	vddIoctlAdd     = 0x0022e004
	vddIoctlRemove  = 0x0022a008
	vddIoctlUpdate  = 0x0022a00c
	vddIoctlVersion = 0x0022e010

	vddIoctlTimeoutMs = 5000
	vddKeepalive      = 50 * time.Millisecond // driver unplugs displays not pinged within ~100ms

	vddInstallHint = "install the Parsec Virtual Display Driver from https://github.com/nomi-san/parsec-vdd/releases " +
		"(driver installer: https://builds.parsec.app/vdd/parsec-vdd-0.45.0.0.exe), then restart the connector"
)

var errVDDNotInstalled = errors.New("Parsec Virtual Display Driver not found; " + vddInstallHint)

// vddDevice is an open handle to the VDD adapter. All ioctls are serialized
// through one heap-allocated overlapped state so buffers stay valid while the
// kernel owns them.
type vddDevice struct {
	mu     sync.Mutex
	handle windows.Handle
	event  windows.Handle
	ov     windows.Overlapped
	in     [32]byte
	out    uint32
	n      uint32

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

func openVDD() (*vddDevice, error) {
	paths, err := windows.CM_Get_Device_Interface_List("", &vddAdapterGUID, windows.CM_GET_DEVICE_INTERFACE_LIST_PRESENT)
	if err != nil {
		return nil, fmt.Errorf("enumerate Parsec VDD adapter interfaces: %w; %s", err, vddInstallHint)
	}
	var lastErr error
	for _, p := range paths {
		if p == "" {
			continue
		}
		name, err := windows.UTF16PtrFromString(p)
		if err != nil {
			lastErr = err
			continue
		}
		h, err := windows.CreateFile(name,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_NO_BUFFERING|windows.FILE_FLAG_OVERLAPPED|windows.FILE_FLAG_WRITE_THROUGH,
			0)
		if err != nil {
			lastErr = err
			continue
		}
		ev, err := windows.CreateEvent(nil, 1, 0, nil)
		if err != nil {
			windows.CloseHandle(h)
			return nil, fmt.Errorf("create event for Parsec VDD: %w", err)
		}
		return &vddDevice{handle: h, event: ev}, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("open Parsec Virtual Display Driver: %w; %s", lastErr, vddInstallHint)
	}
	return nil, errVDDNotInstalled
}

// ioctl issues one overlapped DeviceIoControl with a 32-byte input buffer and
// a DWORD output, waiting up to vddIoctlTimeoutMs for completion.
func (d *vddDevice) ioctl(code uint32, data []byte) (uint32, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.handle == 0 {
		return 0, errors.New("Parsec VDD handle closed")
	}
	d.in = [32]byte{}
	copy(d.in[:], data)
	d.out = 0
	d.n = 0
	d.ov = windows.Overlapped{HEvent: d.event}
	if err := windows.ResetEvent(d.event); err != nil {
		return 0, fmt.Errorf("reset VDD event: %w", err)
	}
	err := windows.DeviceIoControl(d.handle, code, &d.in[0], uint32(len(d.in)), (*byte)(unsafe.Pointer(&d.out)), 4, nil, &d.ov)
	if err != nil && err != windows.ERROR_IO_PENDING {
		return 0, fmt.Errorf("VDD ioctl %#x: %w", code, err)
	}
	ev, werr := windows.WaitForSingleObject(d.event, vddIoctlTimeoutMs)
	if werr != nil || ev != windows.WAIT_OBJECT_0 {
		// Make sure the kernel is done with our buffers before returning.
		_ = windows.CancelIoEx(d.handle, &d.ov)
		_ = windows.GetOverlappedResult(d.handle, &d.ov, &d.n, true)
		if werr != nil {
			return 0, fmt.Errorf("VDD ioctl %#x wait: %w", code, werr)
		}
		return 0, fmt.Errorf("VDD ioctl %#x timed out", code)
	}
	if err := windows.GetOverlappedResult(d.handle, &d.ov, &d.n, false); err != nil {
		return 0, fmt.Errorf("VDD ioctl %#x: %w", code, err)
	}
	return d.out, nil
}

func (d *vddDevice) version() (uint32, error) { return d.ioctl(vddIoctlVersion, nil) }

func (d *vddDevice) update() error {
	_, err := d.ioctl(vddIoctlUpdate, nil)
	return err
}

// addDisplay plugs a new virtual display and returns its driver index.
func (d *vddDevice) addDisplay() (int, error) {
	idx, err := d.ioctl(vddIoctlAdd, nil)
	if err != nil {
		return 0, fmt.Errorf("add virtual display: %w", err)
	}
	if int32(idx) < 0 {
		return 0, errors.New("add virtual display: driver refused (display limit reached?)")
	}
	if err := d.update(); err != nil {
		return 0, fmt.Errorf("ping virtual display driver: %w", err)
	}
	return int(idx), nil
}

// removeDisplay unplugs the display at index (sent as 16-bit big-endian).
func (d *vddDevice) removeDisplay(index int) error {
	be := []byte{byte(index >> 8), byte(index)}
	if _, err := d.ioctl(vddIoctlRemove, be); err != nil {
		return fmt.Errorf("remove virtual display %d: %w", index, err)
	}
	return d.update()
}

// startKeepalive pings the driver in the background so added displays stay plugged.
func (d *vddDevice) startKeepalive() {
	d.stop = make(chan struct{})
	d.done = make(chan struct{})
	go func() {
		defer close(d.done)
		t := time.NewTicker(vddKeepalive)
		defer t.Stop()
		for {
			select {
			case <-d.stop:
				return
			case <-t.C:
				_ = d.update()
			}
		}
	}()
}

func (d *vddDevice) stopKeepalive() {
	if d.stop == nil {
		return
	}
	d.stopOnce.Do(func() {
		close(d.stop)
		<-d.done
	})
}

func (d *vddDevice) close() {
	d.stopKeepalive()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.handle != 0 {
		windows.CloseHandle(d.handle)
		d.handle = 0
	}
	if d.event != 0 {
		windows.CloseHandle(d.event)
		d.event = 0
	}
}

// writeVDDCustomResolution stores width×height@60 in custom resolution slot 0
// (HKLM\SOFTWARE\Parsec\vdd\0) so the driver advertises the mode on plug.
// Best effort: requires admin rights; failures are ignored.
func writeVDDCustomResolution(width, height int) {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, `SOFTWARE\Parsec\vdd\0`, registry.SET_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return
	}
	defer k.Close()
	_ = k.SetDWordValue("width", uint32(width))
	_ = k.SetDWordValue("height", uint32(height))
	_ = k.SetDWordValue("hz", 60)
}
