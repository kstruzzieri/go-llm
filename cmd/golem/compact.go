package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	golemruntime "github.com/kstruzzieri/go-llm/golem"
)

func handleCompact(ctx context.Context, out io.Writer, sess *replSession, fields []string) {
	if len(fields) != 1 {
		_, _ = fmt.Fprintln(out, "usage: /compact")
		return
	}
	if sess.session == nil {
		_, _ = fmt.Fprintln(out, "session disabled (--no-session)")
		return
	}
	if sess.runtime == nil {
		_, _ = fmt.Fprintln(out, "compact: runtime unavailable")
		return
	}
	if sess.noCompress {
		_, _ = fmt.Fprintln(out, "compact: compression unavailable")
		return
	}
	opCtx, cancel := interruptContext(ctx, sess.interrupts)
	defer cancel()
	var report golemruntime.CompactionReport
	var err error
	if sess.destAdmission != nil {
		err = sess.destAdmission.ensure(opCtx)
	}
	if err == nil {
		report, err = sess.runtime.CompactThread(opCtx, sess.session.id)
	}
	if err != nil {
		switch {
		case errors.Is(err, golemruntime.ErrCompressionUnavailable):
			_, _ = fmt.Fprintln(out, "compact: compression unavailable")
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			_, _ = fmt.Fprintln(out, "compact: canceled; session unchanged")
		default:
			_, _ = fmt.Fprintf(out, "compact failed: %s\n", runFailureMessage("", err))
		}
		return
	}
	state := "unchanged"
	if report.Changed {
		state = "changed"
	}
	_, _ = fmt.Fprintf(out, "compact: history token estimate %d -> %d (%s)\n", report.TokensBefore, report.TokensAfter, state)
	if report.Changed {
		if _, err := sess.session.switchTo(ctx, sess.session.id); err != nil {
			_, _ = fmt.Fprintf(out, "warning: session state not refreshed: %s\n", runFailureMessage("", err))
		}
	}
}
