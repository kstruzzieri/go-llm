//go:build !unix

package tools

import (
	"context"
	"errors"
)

var errExecUnsupported = errors.New("exec unsupported on this platform")

type unsupportedRunner struct{}

func newPlatformRunner() commandRunner { return unsupportedRunner{} }

func (unsupportedRunner) Run(_ context.Context, _ execSpec) (execResult, error) {
	return execResult{}, errExecUnsupported
}
