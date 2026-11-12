//go:build linux || darwin

package tools

import (
	"context"
	"io/fs"

	"github.com/kstruzzieri/go-llm/agent/interceptor"
)

// benchWalk is the one call the screen benchmarks make into walkTolerant, so
// that screen_walk_bench_test.go is the same file at every commit it is run
// against; only this shim follows walkTolerant's signature.
func benchWalk(ctx context.Context, ws *Workspace, fn, ignored func(string, fs.DirEntry) error, skip func(walkSkip)) error {
	return ws.walkTolerant(ctx, fn, ignored, skip, interceptor.IsCredentialPath)
}
