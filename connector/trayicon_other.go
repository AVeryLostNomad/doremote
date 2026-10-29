//go:build !darwin

package main

import (
	_ "embed"
	"encoding/binary"

	"fyne.io/systray"
)

//go:embed assets/tray-windows.png
var trayPNG []byte

// The Windows notification area takes .ico data; an ICO may wrap a PNG image directly.
func setTrayIcon() { systray.SetIcon(pngToICO(trayPNG)) }

func pngToICO(png []byte) []byte {
	ico := make([]byte, 22, 22+len(png))
	binary.LittleEndian.PutUint16(ico[2:], 1) // type: icon
	binary.LittleEndian.PutUint16(ico[4:], 1) // image count
	w := binary.BigEndian.Uint32(png[16:20])  // PNG IHDR width/height
	h := binary.BigEndian.Uint32(png[20:24])
	ico[6], ico[7] = byte(w), byte(h)           // 0 means 256
	binary.LittleEndian.PutUint16(ico[10:], 1)  // color planes
	binary.LittleEndian.PutUint16(ico[12:], 32) // bits per pixel
	binary.LittleEndian.PutUint32(ico[14:], uint32(len(png)))
	binary.LittleEndian.PutUint32(ico[18:], 22)
	return append(ico, png...)
}
