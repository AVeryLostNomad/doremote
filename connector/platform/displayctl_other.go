//go:build !darwin

package platform

import (
	"fmt"
	"os"
)

// displayCtlMain exists for macOS only, where display transactions run out of process.
func displayCtlMain() int {
	fmt.Fprintln(os.Stderr, "display control helper is macOS-only")
	return 2
}
