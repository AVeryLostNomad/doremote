//go:build !darwin && !windows

package platform

import (
	"errors"
	"runtime"
)

var errUnsupported = errors.New("doremote connector supports macOS and Windows only, not " + runtime.GOOS)

func Preflight() error { return errUnsupported }

func Recover() error { return nil }

func Start(Options) (Session, error) { return nil, errUnsupported }
