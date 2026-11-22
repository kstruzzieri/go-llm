//go:build !darwin

package tools

import (
	"context"
	"fmt"
)

// ActiveDeveloperDirectory has no developer directory to report off macOS:
// it always fails, wrapping ErrSandboxPolicy.
func ActiveDeveloperDirectory(context.Context) (string, error) {
	return "", fmt.Errorf("tools: the active developer directory exists only on macOS: %w", ErrSandboxPolicy)
}
