//go:build !unix

package tools

import (
	"context"
)

type unsupportedRunner struct{}

func newPlatformRunner() commandRunner { return unsupportedRunner{} }

func (unsupportedRunner) Run(_ context.Context, _ execSpec) (execResult, error) {
	return execResult{}, errExecUnsupported
}
