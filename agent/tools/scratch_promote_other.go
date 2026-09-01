//go:build !linux && !darwin

package tools

import (
	"errors"
	"io/fs"
)

// scratchPromotionSupported is false: this platform has no behaviorally
// tested atomic no-replace install, so a promotion-enabled factory fails
// construction (S5). Capture and query still work.
const scratchPromotionSupported = false

// fsModePermMask is the permission-bit mask used by post-commit checks.
const fsModePermMask = fs.FileMode(0o777)

func installPromotedCreate(root string, change scratchChange) (bool, error) {
	return false, errors.New("tools: scratch promotion is unsupported on this platform")
}
