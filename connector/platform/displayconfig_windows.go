//go:build windows

package platform

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modUser32 = windows.NewLazySystemDLL("user32.dll")
	modGdi32  = windows.NewLazySystemDLL("gdi32.dll")

	procSetProcessDpiAwarenessContext = modUser32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDPIAware            = modUser32.NewProc("SetProcessDPIAware")
	procGetDisplayConfigBufferSizes   = modUser32.NewProc("GetDisplayConfigBufferSizes")
	procQueryDisplayConfig            = modUser32.NewProc("QueryDisplayConfig")
	procSetDisplayConfig              = modUser32.NewProc("SetDisplayConfig")
	procDisplayConfigGetDeviceInfo    = modUser32.NewProc("DisplayConfigGetDeviceInfo")
	procChangeDisplaySettingsExW      = modUser32.NewProc("ChangeDisplaySettingsExW")
	procEnumDisplaySettingsExW        = modUser32.NewProc("EnumDisplaySettingsExW")
)

const (
	qdcOnlyActivePaths = 0x2

	sdcTopologyExtend           = 0x4
	sdcUseDatabaseCurrent       = 0xF
	sdcUseSuppliedDisplayConfig = 0x20
	sdcApply                    = 0x80
	sdcAllowChanges             = 0x400

	displayConfigPathActive       = 0x1
	displayConfigPathModeIdxInval = 0xFFFFFFFF

	displayConfigDeviceInfoGetSourceName = 1
	displayConfigDeviceInfoGetTargetName = 2

	enumCurrentSettings = 0xFFFFFFFF

	dmPelsWidth  = 0x00080000
	dmPelsHeight = 0x00100000

	dispChangeSuccessful = 0
	dispChangeRestart    = 1
)

// dpiAwarenessContextPerMonitorAwareV2 is DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 ((HANDLE)-4).
const dpiAwarenessContextPerMonitorAwareV2 = ^uintptr(3)

// DISPLAYCONFIG_PATH_SOURCE_INFO
type pathSourceInfo struct {
	AdapterID   windows.LUID
	ID          uint32
	ModeInfoIdx uint32
	StatusFlags uint32
}

// DISPLAYCONFIG_PATH_TARGET_INFO
type pathTargetInfo struct {
	AdapterID        windows.LUID
	ID               uint32
	ModeInfoIdx      uint32
	OutputTechnology uint32
	Rotation         uint32
	Scaling          uint32
	RefreshRate      rational
	ScanLineOrdering uint32
	TargetAvailable  int32
	StatusFlags      uint32
}

type rational struct{ Numerator, Denominator uint32 }

// DISPLAYCONFIG_PATH_INFO
type pathInfo struct {
	Source pathSourceInfo
	Target pathTargetInfo
	Flags  uint32
}

// DISPLAYCONFIG_MODE_INFO; the 48-byte union starts at offset 16.
type modeInfo struct {
	InfoType  uint32
	ID        uint32
	AdapterID windows.LUID
	union     [48]byte
}

// DISPLAYCONFIG_SOURCE_MODE (the source arm of modeInfo's union).
type sourceMode struct {
	Width       uint32
	Height      uint32
	PixelFormat uint32
	PositionX   int32
	PositionY   int32
}

func (m *modeInfo) source() *sourceMode { return (*sourceMode)(unsafe.Pointer(&m.union[0])) }

// DISPLAYCONFIG_DEVICE_INFO_HEADER
type deviceInfoHeader struct {
	Type      uint32
	Size      uint32
	AdapterID windows.LUID
	ID        uint32
}

// DISPLAYCONFIG_TARGET_DEVICE_NAME
type targetDeviceName struct {
	Header                    deviceInfoHeader
	Flags                     uint32
	OutputTechnology          uint32
	EdidManufactureID         uint16
	EdidProductCodeID         uint16
	ConnectorInstance         uint32
	MonitorFriendlyDeviceName [64]uint16
	MonitorDevicePath         [128]uint16
}

// DISPLAYCONFIG_SOURCE_DEVICE_NAME
type sourceDeviceName struct {
	Header            deviceInfoHeader
	ViewGdiDeviceName [32]uint16
}

// DEVMODEW, display variant of the first union (dmPosition etc.).
type devMode struct {
	DeviceName         [32]uint16
	SpecVersion        uint16
	DriverVersion      uint16
	Size               uint16
	DriverExtra        uint16
	Fields             uint32
	PositionX          int32
	PositionY          int32
	DisplayOrientation uint32
	DisplayFixedOutput uint32
	Color              int16
	Duplex             int16
	YResolution        int16
	TTOption           int16
	Collate            int16
	FormName           [32]uint16
	LogPixels          uint16
	BitsPerPel         uint32
	PelsWidth          uint32
	PelsHeight         uint32
	DisplayFlags       uint32
	DisplayFrequency   uint32
	ICMMethod          uint32
	ICMIntent          uint32
	MediaType          uint32
	DitherType         uint32
	Reserved1          uint32
	Reserved2          uint32
	PanningWidth       uint32
	PanningHeight      uint32
}

// Compile-time layout checks against the Win32 definitions.
var (
	_ [72]byte  = [unsafe.Sizeof(pathInfo{})]byte{}
	_ [64]byte  = [unsafe.Sizeof(modeInfo{})]byte{}
	_ [16]byte  = [unsafe.Offsetof(modeInfo{}.union)]byte{}
	_ [20]byte  = [unsafe.Sizeof(sourceMode{})]byte{}
	_ [20]byte  = [unsafe.Sizeof(deviceInfoHeader{})]byte{}
	_ [420]byte = [unsafe.Sizeof(targetDeviceName{})]byte{}
	_ [84]byte  = [unsafe.Sizeof(sourceDeviceName{})]byte{}
	_ [220]byte = [unsafe.Sizeof(devMode{})]byte{}
	_ [76]byte  = [unsafe.Offsetof(devMode{}.PositionX)]byte{}
	_ [172]byte = [unsafe.Offsetof(devMode{}.PelsWidth)]byte{}
)

// displayConfig is a snapshot of QueryDisplayConfig output.
type displayConfig struct {
	paths []pathInfo
	modes []modeInfo
}

type targetKey struct {
	adapter windows.LUID
	id      uint32
}

func (p *pathInfo) targetKey() targetKey { return targetKey{p.Target.AdapterID, p.Target.ID} }

func win32Err(name string, r uintptr) error {
	return fmt.Errorf("%s: %w", name, syscall.Errno(uint32(r)))
}

func queryDisplayConfig(flags uint32) (*displayConfig, error) {
	for range 8 {
		var nPaths, nModes uint32
		r, _, _ := procGetDisplayConfigBufferSizes.Call(uintptr(flags), uintptr(unsafe.Pointer(&nPaths)), uintptr(unsafe.Pointer(&nModes)))
		if r != 0 {
			return nil, win32Err("GetDisplayConfigBufferSizes", r)
		}
		paths := make([]pathInfo, nPaths+1)
		modes := make([]modeInfo, nModes+1)
		nPaths, nModes = uint32(len(paths)), uint32(len(modes))
		r, _, _ = procQueryDisplayConfig.Call(uintptr(flags),
			uintptr(unsafe.Pointer(&nPaths)), uintptr(unsafe.Pointer(&paths[0])),
			uintptr(unsafe.Pointer(&nModes)), uintptr(unsafe.Pointer(&modes[0])),
			0)
		if r == uintptr(windows.ERROR_INSUFFICIENT_BUFFER) {
			continue // topology changed between the two calls
		}
		if r != 0 {
			return nil, win32Err("QueryDisplayConfig", r)
		}
		return &displayConfig{paths: paths[:nPaths], modes: modes[:nModes]}, nil
	}
	return nil, errors.New("QueryDisplayConfig: display topology kept changing")
}

func setDisplayConfig(paths []pathInfo, modes []modeInfo, flags uint32) error {
	r, _, _ := procSetDisplayConfig.Call(
		uintptr(len(paths)), uintptr(unsafe.Pointer(unsafe.SliceData(paths))),
		uintptr(len(modes)), uintptr(unsafe.Pointer(unsafe.SliceData(modes))),
		uintptr(flags))
	if r != 0 {
		return win32Err("SetDisplayConfig", r)
	}
	return nil
}

// restoreDisplayConfig re-applies a saved active configuration, falling back
// to the persisted database configuration.
func restoreDisplayConfig(saved *displayConfig) error {
	if saved != nil && len(saved.paths) > 0 {
		err := setDisplayConfig(saved.paths, saved.modes, sdcApply|sdcUseSuppliedDisplayConfig|sdcAllowChanges)
		if err == nil {
			return nil
		}
	}
	return setDisplayConfig(nil, nil, sdcApply|sdcUseDatabaseCurrent)
}

func getTargetName(p *pathInfo) (*targetDeviceName, error) {
	var tn targetDeviceName
	tn.Header = deviceInfoHeader{
		Type:      displayConfigDeviceInfoGetTargetName,
		Size:      uint32(unsafe.Sizeof(tn)),
		AdapterID: p.Target.AdapterID,
		ID:        p.Target.ID,
	}
	r, _, _ := procDisplayConfigGetDeviceInfo.Call(uintptr(unsafe.Pointer(&tn)))
	if r != 0 {
		return nil, win32Err("DisplayConfigGetDeviceInfo(target name)", r)
	}
	return &tn, nil
}

func getSourceGDIName(p *pathInfo) (string, error) {
	var sn sourceDeviceName
	sn.Header = deviceInfoHeader{
		Type:      displayConfigDeviceInfoGetSourceName,
		Size:      uint32(unsafe.Sizeof(sn)),
		AdapterID: p.Source.AdapterID,
		ID:        p.Source.ID,
	}
	r, _, _ := procDisplayConfigGetDeviceInfo.Call(uintptr(unsafe.Pointer(&sn)))
	if r != 0 {
		return "", win32Err("DisplayConfigGetDeviceInfo(source name)", r)
	}
	return windows.UTF16ToString(sn.ViewGdiDeviceName[:]), nil
}

func isVDDTarget(p *pathInfo) bool {
	tn, err := getTargetName(p)
	if err != nil {
		return false
	}
	friendly := windows.UTF16ToString(tn.MonitorFriendlyDeviceName[:])
	devPath := strings.ToUpper(windows.UTF16ToString(tn.MonitorDevicePath[:]))
	return strings.EqualFold(friendly, vddDisplayName) || strings.Contains(devPath, vddDisplayID)
}

// findNewVDDPath returns the index of an active path that is a Parsec VDD
// monitor and was not active in the baseline configuration.
func findNewVDDPath(cfg *displayConfig, baseline map[targetKey]bool) int {
	for i := range cfg.paths {
		p := &cfg.paths[i]
		if p.Flags&displayConfigPathActive == 0 || baseline[p.targetKey()] {
			continue
		}
		if isVDDTarget(p) {
			return i
		}
	}
	return -1
}

// isolatedConfig derives a configuration from cfg in which only path vdd stays
// active, positioned at the desktop origin with the requested source size.
// Unreferenced modes are dropped and indices renumbered.
func isolatedConfig(cfg *displayConfig, vdd int, width, height int, keepTargetMode bool) ([]pathInfo, []modeInfo) {
	paths := make([]pathInfo, len(cfg.paths))
	copy(paths, cfg.paths)
	var modes []modeInfo
	remap := func(idx uint32) uint32 {
		if idx == displayConfigPathModeIdxInval || int(idx) >= len(cfg.modes) {
			return displayConfigPathModeIdxInval
		}
		modes = append(modes, cfg.modes[idx])
		return uint32(len(modes) - 1)
	}
	for i := range paths {
		p := &paths[i]
		if i != vdd {
			p.Flags &^= displayConfigPathActive
			p.Source.ModeInfoIdx = displayConfigPathModeIdxInval
			p.Target.ModeInfoIdx = displayConfigPathModeIdxInval
			continue
		}
		p.Source.ModeInfoIdx = remap(p.Source.ModeInfoIdx)
		if p.Source.ModeInfoIdx != displayConfigPathModeIdxInval {
			sm := modes[p.Source.ModeInfoIdx].source()
			sm.PositionX, sm.PositionY = 0, 0
			sm.Width, sm.Height = uint32(width), uint32(height)
		}
		if keepTargetMode {
			p.Target.ModeInfoIdx = remap(p.Target.ModeInfoIdx)
		} else {
			p.Target.ModeInfoIdx = displayConfigPathModeIdxInval
		}
	}
	return paths, modes
}

func newDevMode() devMode {
	var dm devMode
	dm.Size = uint16(unsafe.Sizeof(dm))
	return dm
}

func enumDisplaySettings(gdiName *uint16, mode uint32) (devMode, bool) {
	dm := newDevMode()
	r, _, _ := procEnumDisplaySettingsExW.Call(uintptr(unsafe.Pointer(gdiName)), uintptr(mode), uintptr(unsafe.Pointer(&dm)), 0)
	return dm, r != 0
}

// displayRect returns the current desktop rectangle of a GDI display.
func displayRect(gdiName string) (x, y, w, h int, err error) {
	name, err := windows.UTF16PtrFromString(gdiName)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	dm, ok := enumDisplaySettings(name, enumCurrentSettings)
	if !ok {
		return 0, 0, 0, 0, fmt.Errorf("EnumDisplaySettingsExW(%s): no current settings", gdiName)
	}
	return int(dm.PositionX), int(dm.PositionY), int(dm.PelsWidth), int(dm.PelsHeight), nil
}

// setDisplayResolution switches gdiName to width×height, falling back to the
// largest supported mode that fits inside the request when the exact size is
// not offered by the driver.
func setDisplayResolution(gdiName string, width, height int) error {
	name, err := windows.UTF16PtrFromString(gdiName)
	if err != nil {
		return err
	}
	if changeDisplayMode(name, uint32(width), uint32(height)) == nil {
		return nil
	}
	var bestW, bestH uint32
	for i := uint32(0); ; i++ {
		dm, ok := enumDisplaySettings(name, i)
		if !ok {
			break
		}
		if dm.PelsWidth > uint32(width) || dm.PelsHeight > uint32(height) {
			continue
		}
		if uint64(dm.PelsWidth)*uint64(dm.PelsHeight) > uint64(bestW)*uint64(bestH) {
			bestW, bestH = dm.PelsWidth, dm.PelsHeight
		}
	}
	if bestW == 0 {
		return fmt.Errorf("display %s supports no mode within %dx%d", gdiName, width, height)
	}
	return changeDisplayMode(name, bestW, bestH)
}

func changeDisplayMode(name *uint16, width, height uint32) error {
	dm := newDevMode()
	dm.Fields = dmPelsWidth | dmPelsHeight
	dm.PelsWidth, dm.PelsHeight = width, height
	r, _, _ := procChangeDisplaySettingsExW.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&dm)), 0, 0, 0)
	switch int32(r) {
	case dispChangeSuccessful, dispChangeRestart:
		return nil
	default:
		return fmt.Errorf("ChangeDisplaySettingsExW(%dx%d) failed with code %d", width, height, int32(r))
	}
}
