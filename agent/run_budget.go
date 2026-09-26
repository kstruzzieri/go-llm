package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/kstruzzieri/go-llm/provider"
)

var errRunBudgetExhausted = errors.New("agent: run token budget exhausted")

type runBudgetKey struct{}

// runBudget belongs to one Run. Capacities and parent links are immutable;
// the tree's shared mutex guards all accounting and lifetime state.
type runBudget struct {
	parent                   *runBudget
	mu                       *sync.Mutex
	cancel                   context.CancelFunc
	input, generation, steps int
	limit                    int
	finite                   bool
	charged, reserved        int
	closed, overrun          bool
	descendants              provider.Usage
}

type tokenReservation struct {
	run            *runBudget
	prompt, tokens int
	settled        bool // guarded by run.mu
}

func newRunBudget(ctx context.Context, req Request) (context.Context, Request, *runBudget) {
	parent, _ := ctx.Value(runBudgetKey{}).(*runBudget)
	b := &runBudget{parent: parent, limit: req.Budget.TotalTokens, finite: req.Budget.TotalTokens > 0}
	if req.Budget.InputCeiling <= 0 {
		req.Budget.InputCeiling = DefaultInputCeiling
	}
	if req.MaxSteps <= 0 {
		req.MaxSteps = defaultMaxSteps
	}
	if parent != nil {
		b.mu = parent.mu
		b.finite = b.finite || parent.finite
		req.Budget.InputCeiling = min(req.Budget.InputCeiling, parent.input)
		req.MaxSteps = min(req.MaxSteps, parent.steps)
	} else {
		b.mu = &sync.Mutex{}
	}
	generation := req.Budget.OutputReserve
	if generation <= 0 {
		generation = req.Options.NumPredict
	}
	if generation <= 0 && b.finite {
		generation = provider.DefaultExpectedOutput("chat")
	}
	if parent != nil && parent.generation > 0 && (generation <= 0 || generation > parent.generation) {
		generation = parent.generation
	}
	b.input, b.steps, b.generation = req.Budget.InputCeiling, req.MaxSteps, max(0, generation)
	if generation > 0 {
		req.Options.NumPredict = generation
		if req.Budget.OutputReserve > 0 {
			req.Budget.OutputReserve = generation
		}
	}
	ctx, b.cancel = context.WithCancel(ctx)
	return context.WithValue(ctx, runBudgetKey{}, b), req, b
}

func (b *runBudget) reserve(ctx context.Context, promptTokens int) (*tokenReservation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	need, ok := checkedTokenAdd(promptTokens, b.generation)
	if promptTokens < 0 || !ok {
		return nil, errRunBudgetExhausted
	}
	for node := b; node != nil; node = node.parent {
		if node.closed || node.overrun || (node.limit > 0 && node.charged >= node.limit) {
			return nil, errRunBudgetExhausted
		}
		if node.limit <= 0 {
			continue
		}
		used, usedOK := checkedTokenAdd(node.charged, node.reserved)
		total, totalOK := checkedTokenAdd(used, need)
		if !usedOK || !totalOK || total > node.limit {
			return nil, errRunBudgetExhausted
		}
	}
	for node := b; node != nil; node = node.parent {
		if node.limit > 0 {
			node.reserved += need // checked above, before any mutation
		}
	}
	return &tokenReservation{run: b, prompt: promptTokens, tokens: need}, nil
}

// settle separates logical admission credits from raw provider telemetry.
// No error sentinel proves that a provider did not execute, so failed calls
// retain their reservation even when no chunk or RouteOutcome was returned.
func (r *tokenReservation) settle(mr ModelResult, callErr error) {
	b := r.run
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.settled {
		return
	}
	r.settled = true
	u := mr.Response.Usage
	sum, sumOK := checkedTokenAdd(max(0, u.PromptTokens), max(0, u.CompletionTokens))
	valid := u.PromptTokens >= 0 && u.CompletionTokens >= 0 && u.TotalTokens >= 0 &&
		u.ReasoningTokens >= 0 && sumOK &&
		(u.TotalTokens == 0 || u.TotalTokens == sum || (u.PromptTokens == 0 && u.CompletionTokens == 0))
	logical := saturatedTokenAdd(max(r.prompt, u.PromptTokens), max(0, u.CompletionTokens))
	charge := max(logical, u.TotalTokens)
	multiAttempt := mr.RouteOutcome != nil && (len(mr.RouteOutcome.Attempts) > 1 || mr.RouteOutcome.FallbacksUsed > 0)
	if outcome := mr.Response.RouteOutcome; outcome != nil {
		multiAttempt = multiAttempt || len(outcome.Attempts) > 1 || outcome.FallbacksUsed > 0
	}
	if callErr != nil || !valid || u.PromptTokens == 0 || multiAttempt {
		charge = max(charge, r.tokens)
	}
	if b.finite && charge > r.tokens {
		b.overrun = true
	}
	for node := b; node != nil; node = node.parent {
		if node.limit > 0 {
			node.reserved -= r.tokens
			node.charged = saturatedTokenAdd(node.charged, charge)
		}
		if node != b && valid {
			node.descendants.PromptTokens = saturatedTokenAdd(node.descendants.PromptTokens, u.PromptTokens)
			node.descendants.CompletionTokens = saturatedTokenAdd(node.descendants.CompletionTokens, u.CompletionTokens)
			node.descendants.TotalTokens = saturatedTokenAdd(node.descendants.TotalTokens, u.TotalTokens)
		}
	}
}

func (b *runBudget) stopped() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for node := b; node != nil; node = node.parent {
		if node.closed || node.overrun || (node.limit > 0 && node.charged >= node.limit) {
			return true
		}
	}
	return false
}

// checkRunBudget gates tool preparation and invocation after callbacks may
// have spent the remaining allowance through a nested Run.
func checkRunBudget(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b, ok := ctx.Value(runBudgetKey{}).(*runBudget); ok && b.stopped() {
		return errRunBudgetExhausted
	}
	return nil
}

func (b *runBudget) close() *provider.Usage {
	b.mu.Lock()
	b.closed = true
	snapshot := b.descendants
	b.mu.Unlock()
	b.cancel()
	if snapshot == (provider.Usage{}) {
		return nil
	}
	return &snapshot
}
