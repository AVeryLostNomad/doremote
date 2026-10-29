package main

import (
	_ "embed"

	"fyne.io/systray"
)

//go:embed assets/tray-template.png
var trayTemplate []byte

// The menu bar tints template images for light/dark mode.
func setTrayIcon() { systray.SetTemplateIcon(trayTemplate, trayTemplate) }
