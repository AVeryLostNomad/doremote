//go:build windows

package platform

// scanKey is a PC/AT set-1 scancode (with E0 prefix as extended), or a
// virtual-key code for keys whose scancode form SendInput cannot express.
type scanKey struct {
	scan     uint16
	extended bool
	vk       uint16 // nonzero: inject by virtual key instead of scancode
}

const (
	vkPause   = 0x13
	vkNumLock = 0x90
)

func sc(code uint16) scanKey  { return scanKey{scan: code} }
func ext(code uint16) scanKey { return scanKey{scan: code, extended: true} }

// winScancodes maps DOM KeyboardEvent.code values to Windows scancodes.
var winScancodes = map[string]scanKey{
	"Escape":    sc(0x01),
	"Digit1":    sc(0x02),
	"Digit2":    sc(0x03),
	"Digit3":    sc(0x04),
	"Digit4":    sc(0x05),
	"Digit5":    sc(0x06),
	"Digit6":    sc(0x07),
	"Digit7":    sc(0x08),
	"Digit8":    sc(0x09),
	"Digit9":    sc(0x0A),
	"Digit0":    sc(0x0B),
	"Minus":     sc(0x0C),
	"Equal":     sc(0x0D),
	"Backspace": sc(0x0E),
	"Tab":       sc(0x0F),

	"KeyQ":         sc(0x10),
	"KeyW":         sc(0x11),
	"KeyE":         sc(0x12),
	"KeyR":         sc(0x13),
	"KeyT":         sc(0x14),
	"KeyY":         sc(0x15),
	"KeyU":         sc(0x16),
	"KeyI":         sc(0x17),
	"KeyO":         sc(0x18),
	"KeyP":         sc(0x19),
	"BracketLeft":  sc(0x1A),
	"BracketRight": sc(0x1B),
	"Enter":        sc(0x1C),
	"ControlLeft":  sc(0x1D),

	"KeyA":      sc(0x1E),
	"KeyS":      sc(0x1F),
	"KeyD":      sc(0x20),
	"KeyF":      sc(0x21),
	"KeyG":      sc(0x22),
	"KeyH":      sc(0x23),
	"KeyJ":      sc(0x24),
	"KeyK":      sc(0x25),
	"KeyL":      sc(0x26),
	"Semicolon": sc(0x27),
	"Quote":     sc(0x28),
	"Backquote": sc(0x29),
	"ShiftLeft": sc(0x2A),
	"Backslash": sc(0x2B),

	"KeyZ":       sc(0x2C),
	"KeyX":       sc(0x2D),
	"KeyC":       sc(0x2E),
	"KeyV":       sc(0x2F),
	"KeyB":       sc(0x30),
	"KeyN":       sc(0x31),
	"KeyM":       sc(0x32),
	"Comma":      sc(0x33),
	"Period":     sc(0x34),
	"Slash":      sc(0x35),
	"ShiftRight": sc(0x36),

	"NumpadMultiply": sc(0x37),
	"AltLeft":        sc(0x38),
	"Space":          sc(0x39),
	"CapsLock":       sc(0x3A),

	"F1":  sc(0x3B),
	"F2":  sc(0x3C),
	"F3":  sc(0x3D),
	"F4":  sc(0x3E),
	"F5":  sc(0x3F),
	"F6":  sc(0x40),
	"F7":  sc(0x41),
	"F8":  sc(0x42),
	"F9":  sc(0x43),
	"F10": sc(0x44),
	"F11": sc(0x57),
	"F12": sc(0x58),
	"F13": sc(0x64),
	"F14": sc(0x65),
	"F15": sc(0x66),
	"F16": sc(0x67),
	"F17": sc(0x68),
	"F18": sc(0x69),
	"F19": sc(0x6A),
	"F20": sc(0x6B),
	"F21": sc(0x6C),
	"F22": sc(0x6D),
	"F23": sc(0x6E),
	"F24": sc(0x76),

	// Windows reports NumLock as E0 45 and Pause as E1 1D 45; inject both by virtual key.
	"NumLock":    {scan: 0x45, extended: true, vk: vkNumLock},
	"Pause":      {scan: 0x45, vk: vkPause},
	"ScrollLock": sc(0x46),

	"Numpad7":        sc(0x47),
	"Numpad8":        sc(0x48),
	"Numpad9":        sc(0x49),
	"NumpadSubtract": sc(0x4A),
	"Numpad4":        sc(0x4B),
	"Numpad5":        sc(0x4C),
	"Numpad6":        sc(0x4D),
	"NumpadAdd":      sc(0x4E),
	"Numpad1":        sc(0x4F),
	"Numpad2":        sc(0x50),
	"Numpad3":        sc(0x51),
	"Numpad0":        sc(0x52),
	"NumpadDecimal":  sc(0x53),
	"NumpadEqual":    sc(0x59),
	"NumpadComma":    sc(0x7E),
	"NumpadEnter":    ext(0x1C),
	"NumpadDivide":   ext(0x35),

	"IntlBackslash": sc(0x56),
	"IntlRo":        sc(0x73),
	"IntlYen":       sc(0x7D),
	"KanaMode":      sc(0x70),
	"Convert":       sc(0x79),
	"NonConvert":    sc(0x7B),

	"ControlRight": ext(0x1D),
	"AltRight":     ext(0x38),
	"PrintScreen":  ext(0x37),
	"Home":         ext(0x47),
	"ArrowUp":      ext(0x48),
	"PageUp":       ext(0x49),
	"ArrowLeft":    ext(0x4B),
	"ArrowRight":   ext(0x4D),
	"End":          ext(0x4F),
	"ArrowDown":    ext(0x50),
	"PageDown":     ext(0x51),
	"Insert":       ext(0x52),
	"Delete":       ext(0x53),
	"MetaLeft":     ext(0x5B),
	"MetaRight":    ext(0x5C),
	"OSLeft":       ext(0x5B), // legacy Firefox name for MetaLeft
	"OSRight":      ext(0x5C), // legacy Firefox name for MetaRight
	"ContextMenu":  ext(0x5D),

	"AudioVolumeMute":    ext(0x20),
	"AudioVolumeDown":    ext(0x2E),
	"AudioVolumeUp":      ext(0x30),
	"MediaPlayPause":     ext(0x22),
	"MediaStop":          ext(0x24),
	"MediaTrackNext":     ext(0x19),
	"MediaTrackPrevious": ext(0x10),
}
