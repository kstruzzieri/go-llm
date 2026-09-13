package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/consult"
)

// loadConsultants resolves the consultants file for this process. An unusable
// default -- no file, or no resolvable user config dir -- simply leaves
// /consult unavailable; every other failure (an unreadable explicit path,
// malformed JSON, an invalid declaration) is a misconfiguration the caller
// reports rather than silently degrading to no consultants.
func loadConsultants(explicit string) (map[string]consult.Consultant, error) {
	m, err := consult.Load(explicit)
	if errors.Is(err, consult.ErrDisabled) {
		return nil, nil
	}
	// A well-formed file declaring no consultants is the same state as no file:
	// there is nothing /consult could reach. Returning the empty map instead
	// would advertise the command, announce nothing at startup, and answer every
	// name with "unknown consultant". (The refusal still says "no
	// consultants.json", which is imprecise for this one case; the string is
	// pinned by docs/consult.md.)
	if len(m) == 0 {
		return nil, err
	}
	return m, err
}

// consultFailureLine is the one operator line for an InspectAdvisory failure.
// Only a policy refusal is reported as one: validation, the fail-closed
// annotation cap and a cancelled inspection also come back from that call, and
// calling those "blocked by policy" would misattribute a host bug or a Ctrl-C.
// The refusal outranks a cancellation that travels with it, because the policy
// verdict is the actionable fact and the advice is refused either way.
func consultFailureLine(err error) string {
	var be *agent.BlockedError
	switch {
	case errors.Is(err, agent.ErrAdvisoryBlocked):
		if errors.As(err, &be) && len(be.Findings) > 0 {
			return "consult failed: blocked by interceptor policy (" + be.Findings[0].Rule + ")"
		}
		return "consult failed: blocked by interceptor policy"
	case errors.Is(err, context.Canceled):
		return "consult canceled"
	}
	return "consult failed: internal"
}

// handleConsult runs one consultation and stages the admitted answer for the
// next goal (#382). The consultant name and prompt come from the line; the
// command, argv and environment come only from local config and the adapter.
// Every failure is reported as a fixed code, never as vendor bytes.
func handleConsult(ctx context.Context, out io.Writer, sess *replSession, line string) {
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "/consult"))
	// Dropping local advice needs no consultant launch or interceptor policy.
	if rest == "drop" {
		if sess.advisory == nil {
			_, _ = fmt.Fprintln(out, "no staged advice")
		} else {
			_, _ = fmt.Fprintf(out, "dropped staged advice from %s\n", sess.advisory.Source)
			sess.advisory = nil
		}
		return
	}
	// The refusals come first, ahead of the bare-command status line: a status
	// line reads as "this command works, here is its state", which is exactly
	// wrong when /consult is disabled or ungated.
	switch {
	case sess.consultants == nil:
		_, _ = fmt.Fprintln(out, "consult disabled: no consultants.json (see -consultants-config)")
		return
	case !sess.interceptorsOn:
		_, _ = fmt.Fprintln(out, "consult requires -interceptors")
		return
	case rest == "" && sess.advisory == nil:
		_, _ = fmt.Fprintln(out, "usage: /consult <name> <prompt> | /consult drop; staged: none")
		return
	case rest == "":
		_, _ = fmt.Fprintf(out, "usage: /consult <name> <prompt> | /consult drop; staged: %s (sha256:%s)\n",
			sess.advisory.Source, sess.advisory.Digest[:12])
		return
	}
	name, prompt, _ := strings.Cut(rest, " ")
	if prompt = strings.TrimSpace(prompt); prompt == "" {
		_, _ = fmt.Fprintln(out, "usage: /consult <name> <prompt> | /consult drop")
		return
	}
	c, ok := sess.consultants[name]
	if !ok {
		// name is user-typed; %q escapes it so it cannot rewrite the line.
		_, _ = fmt.Fprintf(out, "unknown consultant %q\n", name)
		return
	}
	runCtx, cancel := interruptContext(ctx, sess.interrupts)
	defer cancel()
	_, _ = fmt.Fprintf(out, "consulting %s...\n", name)
	// The trailing newline matches the E2.2 launch stdin (harness-snapshot-e2-2:
	// e2_2_probe.py writes the prompt terminated by exactly one "\n" and
	// harness.go feeds those bytes verbatim), which is what rule 1's byte-exact
	// user echo was validated against.
	r, err := consult.Run(runCtx, c, prompt+"\n")
	if err != nil {
		var ce *consult.Error
		if errors.As(err, &ce) {
			// Code and Reason are both drawn from closed host-authored
			// vocabularies -- never consultant text, a path, or a wrapped
			// error -- so printing the reason narrows the failure for the
			// operator without leaking anything.
			_, _ = fmt.Fprintf(out, "consult failed: %s (%s)\n", ce.Code, ce.Reason)
		} else {
			_, _ = fmt.Fprintln(out, "consult failed: internal")
		}
		return
	}
	adv, err := sess.orch.InspectAdvisory(runCtx, agent.Advisory{
		Source: r.Consultant, Tool: r.Adapter + " " + r.Version, Model: r.Model,
		Digest: r.ContentSHA256, Content: r.Answer, Origin: agent.OriginModel,
	})
	if err != nil {
		_, _ = fmt.Fprintln(out, consultFailureLine(err))
		return
	}
	if sess.advisory != nil {
		_, _ = fmt.Fprintf(out, "replaced staged advice from %s\n", sess.advisory.Source)
	}
	sess.advisory = &adv
	// adv.Tool, not r.Adapter+r.Version: the terminal shows the operator the
	// same attribution line the model is given, from the same value.
	_, _ = fmt.Fprintf(out, "%s (%s, model %s, exit %d, %s):\n%s\n",
		r.Consultant, adv.Tool, r.Model, r.ExitCode,
		r.Duration.Round(100*time.Millisecond), adv.Content)
	// The interceptor trailer is host-authored and sits outside the frozen
	// answer, so it is shown on its own line rather than folded into it.
	if adv.Annotation != "" {
		_, _ = fmt.Fprintln(out, strings.TrimRight(adv.Annotation, "\n"))
	}
	_, _ = fmt.Fprintln(out, "staged for the next goal")
}
