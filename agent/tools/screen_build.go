//go:build linux || darwin

package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/kstruzzieri/go-llm/agent/interceptor"
)

// notePathMax is the most bytes of a path a note carries.
const notePathMax = 200

// notePath cuts p to notePathMax bytes for a note, which quotes it with %q
// (strconv.Quote), so a path cannot add a line or forge a note.
func notePath(p string) string {
	if len(p) > notePathMax {
		return p[:notePathMax]
	}
	return p
}

// buildOutputScreen walks ws just before a command runs, reads every
// credential-path file, and returns the screen for the command's output. A
// non-nil error means the command must not run: the workspace root was
// replaced or cannot be opened or listed, or ctx is done (returned wrapped).
// Every other incomplete read is on the screen: an entry that changed during
// the read is counted, and anything else, a bound included, withholds both
// streams. The screen is final when returned.
func buildOutputScreen(ctx context.Context, ws *Workspace) (*outputScreen, error) {
	lim := ws.screenLimits.orDefaults()
	wctx, cancel := context.WithTimeout(ctx, lim.budget)
	defer cancel()

	var (
		keys     [][]byte
		total    int
		withhold string
		changed  int
	)
	// stop records the first withhold cause and ends the walk.
	stop := func(cause string) error {
		if withhold == "" {
			withhold = cause
		}
		return fs.SkipAll
	}
	// failed handles an open, read or skip failure at rel; listing picks the
	// cause a withheld listing failure gets.
	failed := func(rel string, err error, listing bool) error {
		switch classifySkip(err) {
		case skipFatal:
			return err
		case skipIgnore:
			return nil
		case skipAnnounceChanged:
			changed++
			return nil
		}
		if listing {
			return stop(fmt.Sprintf("listing failed: %q", notePath(rel)))
		}
		return stop(fmt.Sprintf("unreadable: %q", notePath(rel)))
	}
	// read keys the file f holds and closes it.
	read := func(rel string, f *os.File, gitConfig bool) error {
		content, err := io.ReadAll(io.LimitReader(f, int64(lim.fileBytes)+1))
		_ = f.Close()
		if err != nil {
			return failed(rel, err, false)
		}
		if len(content) > lim.fileBytes {
			return stop(fmt.Sprintf("file over %d bytes: %q", lim.fileBytes, notePath(rel)))
		}
		ks, overLong := credentialFileKeys(content, gitConfig, lim.keyBytes)
		if overLong {
			return stop(fmt.Sprintf("line over %d bytes: %q", lim.keyBytes, notePath(rel)))
		}
		for _, k := range ks {
			if len(keys) == lim.keys {
				return stop(fmt.Sprintf("more than %d keys", lim.keys))
			}
			if total+len(k) > lim.totalBytes {
				return stop(fmt.Sprintf("keys over %d bytes", lim.totalBytes))
			}
			keys = append(keys, k)
			total += len(k)
		}
		return nil
	}

	fn := func(rel string, d fs.DirEntry) error {
		if withhold != "" {
			return fs.SkipAll
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 || !interceptor.IsCredentialPath(rel) {
			return nil
		}
		f, err := ws.openScreened(rel, d)
		if err != nil {
			return failed(rel, err, false)
		}
		return read(rel, f, interceptor.IsGitConfigPath(rel))
	}
	// ignored reads the config of each .git directory the walk skips. A
	// missing, symlinked or non-regular config is nothing: the walk rechecks
	// the root after this returns, no listing says what a non-regular config
	// was before, and a credential-named symlink is never read.
	ignored := func(rel string) error {
		if withhold != "" {
			return fs.SkipAll
		}
		if filepath.Base(rel) != ".git" {
			return nil
		}
		cfg := rel + "/config"
		err := ws.screenSeams.open(cfg)
		var f *os.File
		if err == nil {
			f, err = ws.openRegularFile(cfg)
		}
		switch {
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, errSymlink), errors.Is(err, errNotRegular):
			return nil
		case err != nil:
			return failed(cfg, err, false)
		}
		return read(cfg, f, true)
	}
	skip := func(s walkSkip) {
		var le listingError
		if err := failed(s.rel, s.err, errors.As(s.err, &le)); err != nil && err != fs.SkipAll {
			// The walk returns what it cannot go past; fail closed if it did not.
			_ = stop(fmt.Sprintf("unreadable: %q", notePath(s.rel)))
		}
	}

	err := ws.walkTolerant(wctx, fn, ignored, skip)
	switch {
	case ctx.Err() != nil:
		return nil, fmt.Errorf("tools: credential screen: %w", ctx.Err())
	case err == nil, errors.Is(err, fs.SkipAll):
	case errors.Is(err, context.DeadlineExceeded) && wctx.Err() != nil:
		_ = stop(fmt.Sprintf("time budget of %s exhausted", lim.budget))
	default:
		return nil, err
	}
	s := newOutputScreen(keys)
	s.withhold = withhold
	s.changed = changed
	return s, nil
}

// openScreened is openWalked with the openErr seam asked first; a seam's
// error is handled as openWalked handles a failed open.
func (w *Workspace) openScreened(rel string, d fs.DirEntry) (*os.File, error) {
	if err := w.screenSeams.open(rel); err != nil {
		e, ok := d.(workspaceEntry)
		if !ok {
			return nil, errWalkEntryUnheld
		}
		if reachErr := w.verifyReachable(filepath.Dir(rel), e.dir); reachErr != nil {
			return nil, reachErr
		}
		return nil, err
	}
	return w.openWalked(rel, d)
}
