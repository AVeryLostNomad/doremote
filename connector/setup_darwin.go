package main

const setupTitle = "⚠ Grant Screen Recording & Accessibility…"

// fixSetup opens the privacy settings where doremote needs Screen Recording and Accessibility.
func fixSetup() error {
	return openURL("x-apple.systempreferences:com.apple.preference.security?Privacy_ScreenCapture")
}
