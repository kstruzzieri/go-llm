package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/configio"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

// modelsCheck is `golem models -probe-all` (unforced) or `-reprobe` (forced):
// an explicit check of the agent chain's provider/model entries for the agent
// capabilities, through configio.PlanCheck and RunCheck. It never touches the
// model registry or its automatic allowance, and deletes nothing.
type modelsCheck struct {
	cfg      *config.Config                  // the bootstrap's effective config
	store    fingerprint.EvidenceStore       // the shared store's evidence log
	listings map[string][]provider.ModelInfo // bootstrap listings, for runtime digests
	policy   provider.DestinationPolicy      // the destinations admission granted
	force    bool                            // -reprobe: fresh answers are checked too
	// memoryOnly is true when the shared store fell back to memory, so a
	// saved result lasts only for this command.
	memoryOnly bool
}

// run checks the chain's qualified entries, grouped by provider, providers in
// sorted order; a bare selector names no provider and is not checked. For each
// provider it prints the plan, then one line per result, on errOut. A provider
// that cannot be checked is reported and the rest still run. Only the caller's
// cancellation stops it, returning ctx's error.
func (c *modelsCheck) run(ctx context.Context, chain []string, errOut io.Writer) error {
	byProvider := make(map[string][]string)
	for _, sel := range chain {
		if key, ok := parseSelector(sel); ok {
			byProvider[key.Provider] = append(byProvider[key.Provider], key.Model)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(byProvider)) {
		if err := c.checkProvider(ctx, name, byProvider[name], errOut); err != nil {
			return err
		}
	}
	return nil
}

// checkProvider plans, shows and runs one provider's check. Results arrive in
// completion order, so they are matched to the plan by model; a planned model
// with no result was skipped because its answers became fresh.
func (c *modelsCheck) checkProvider(ctx context.Context, name string, models []string, errOut io.Writer) error {
	plan, err := configio.PlanCheck(ctx, name, models, agent.ModelCallCapabilities(true), configio.CheckOptions{
		Config: c.cfg, Store: c.store, Force: c.force,
		RuntimeDigests: runtimeDigests(c.cfg, name, models, c.listings[name]),
	})
	if err != nil {
		return reportCheckError(ctx, errOut, name, err)
	}
	printCheckPlan(errOut, plan)
	if len(plan.Items) == 0 {
		return nil
	}
	reported := make(map[string]bool, len(plan.Items))
	err = configio.RunCheck(ctx, plan, configio.RunOptions{
		Config: c.cfg, Store: c.store, Policy: c.policy,
		OnResult: func(r configio.CheckResult) {
			reported[r.Model] = true
			c.printResult(errOut, r)
		},
	})
	if err != nil {
		return reportCheckError(ctx, errOut, name, err)
	}
	for _, it := range plan.Items {
		if !reported[it.Model] {
			_, _ = fmt.Fprintf(errOut, "check result %q: not checked: answer became fresh\n", it.Model)
		}
	}
	return nil
}

// runtimeDigests maps each of models to the content digest listing (the
// provider's bootstrap listing) reports for it: the member named exactly as
// the model, else the member under its other Ollama spelling, as routing's
// runtime query finds it. A member without a digest gives none.
func runtimeDigests(cfg *config.Config, providerName string, models []string, listing []provider.ModelInfo) map[string]string {
	pc, ok := cfg.Providers[providerName]
	if !ok || len(listing) == 0 {
		return nil
	}
	b, err := provider.EvidenceBindingFor(pc.BaseURL, pc.APIFormat)
	if err != nil {
		return nil // PlanCheck refuses the provider itself
	}
	digests := make(map[string]string)
	for _, m := range models {
		for _, name := range append([]string{m}, provider.EvidenceModelNames(b.Adapter, m)...) {
			i := slices.IndexFunc(listing, func(info provider.ModelInfo) bool { return info.Name == name })
			if i < 0 {
				continue
			}
			if d := listing[i].Digest; d != "" {
				digests[m] = d
			}
			break
		}
	}
	return digests
}

// reportCheckError prints why provider name was not checked and returns nil,
// or returns ctx's error once the caller cancelled.
func reportCheckError(ctx context.Context, errOut io.Writer, name string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	code, _ := configio.CodeOf(err)
	switch {
	case code == configio.CodeBatchTooLarge:
		_, _ = fmt.Fprintf(errOut, "check: provider %q not checked (%s): %v; keep fewer of its models in the agent chain\n", name, code, err)
	case errors.Is(err, provider.ErrEvidenceUnreadable):
		_, _ = fmt.Fprintf(errOut, "check: provider %q not checked: %s\n", name, evidenceUnreadableReason)
	case code != "":
		_, _ = fmt.Fprintf(errOut, "check: provider %q not checked (%s): %v\n", name, code, err)
	default:
		_, _ = fmt.Fprintf(errOut, "check: provider %q not checked: %v\n", name, err)
	}
	return nil
}

// printCheckPlan shows what a check will send: its provider and endpoint
// host, then per model the method, the capabilities it asks, the requests it
// usually and at most sends, the output tokens each requests of the server,
// the time and response bytes each may take, and what it may load: nothing for
// a metadata read, the model once at a loopback endpoint, and whatever a
// remote server decides.
func printCheckPlan(w io.Writer, plan configio.CheckPlan) {
	head := fmt.Sprintf("check plan: provider %q at %q", plan.Provider, plan.EndpointHost) // the host comes from config: quoted, so no control rune reaches the terminal
	if plan.Force {
		head += " (forced)"
	}
	if len(plan.Items) == 0 {
		_, _ = fmt.Fprintf(w, "%s: nothing to check\n", head)
		return
	}
	_, _ = fmt.Fprintf(w, "%s: %s to check\n", head, plural(len(plan.Items), "model", "models"))
	for _, it := range plan.Items {
		output := "generates nothing"
		if it.MaxOutputTokensPerRequest > 0 {
			output = fmt.Sprintf("requests up to %d output tokens per request", it.MaxOutputTokensPerRequest)
		}
		load := "the server may load the model" // a remote server's loads cannot be predicted
		switch {
		case it.MaxOutputTokensPerRequest == 0:
			load = "loads no model" // a metadata read generates nothing
		case it.ModelLoads > 0:
			load = "may load the model once"
		}
		_, _ = fmt.Fprintf(w, "  %q: %s, %s; usually %s, at most %d; %s; up to %s and %d response bytes per request; %s\n",
			it.Model, strings.Join(it.Methods, "+"), strings.Join(it.Capabilities, "|"),
			plural(it.UsualRequests, "request", "requests"), it.MaxRequests, output, it.RequestTimeout, it.MaxResponseBytes, load)
	}
}

// printResult shows one model's result: its code or its answers, the requests
// it sent, and whether it was saved: only in memory when the store fell back to
// memory, and "store write failed" for a completed check a store did not keep,
// so that is not mistaken for a failed check.
func (c *modelsCheck) printResult(w io.Writer, r configio.CheckResult) {
	outcome := string(r.Code)
	if r.Code == "" {
		parts := make([]string, 0, len(r.Answers))
		for _, a := range r.Answers {
			part := a.Capability + "=" + a.State
			if a.Detail != "" {
				part += " (" + a.Detail + ")"
			}
			parts = append(parts, part)
		}
		outcome = strings.Join(parts, " ")
	}
	saved := "not saved"
	switch {
	case r.Persisted && c.memoryOnly:
		saved = "saved (memory only)"
	case r.Persisted:
		saved = "saved"
	case r.Code == "" && len(r.Answers) > 0 && c.store != nil:
		saved = "not saved (store write failed)"
	}
	_, _ = fmt.Fprintf(w, "check result %q: %s; %s sent; %s\n", r.Model, outcome, plural(r.Requests, "request", "requests"), saved)
}
