//go:build !unix

package main

import "context"

// acquireAuthorLock returns a non-nil release function exactly when err is nil.
func acquireAuthorLock(context.Context, string) (func(), error) {
	return nil, errAgentflowAuthoringUnsupported
}
