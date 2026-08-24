//go:build !unix

package tools

import "io"

type unsupportedStarter struct{}

func newPlatformStarter() backgroundStarter { return unsupportedStarter{} }

func (unsupportedStarter) Start(execSpec, io.Writer, io.Writer) (backgroundProcess, error) {
	return nil, errExecUnsupported
}
