//go:build !unix

package main

import (
	"context"
	"errors"
)

func acquireAuthorLock(context.Context, string) (func(), error) {
	return nil, errors.New("golem: Agentflow plan authoring is unsupported on this platform")
}
