//go:build !darwin && !windows

package main

import (
	"context"
	"errors"
)

// No release asset exists for other platforms; updatesSupported reports false.
func updateAssetKey() string { return "" }

func stageUpdate(context.Context, string, []byte) (stagedUpdate, error) {
	return nil, errors.New("updates are only published for macOS and Windows")
}

func cleanupUpdate() {}
