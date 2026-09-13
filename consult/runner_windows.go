//go:build windows

package consult

import (
	"context"
	"errors"
)

// run is unavailable on Windows: the bounded runner depends on Setpgid and
// negative-PID process-group signalling, which have no equivalent here.
func run(context.Context, runSpec) (runOutcome, error) {
	return runOutcome{}, errors.ErrUnsupported
}
