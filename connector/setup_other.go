//go:build !darwin && !windows

package main

import "errors"

const setupTitle = "⚠ Unsupported operating system"

func fixSetup() error { return errors.New("doremote supports macOS and Windows") }
