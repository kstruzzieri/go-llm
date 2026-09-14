package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
)

var (
	errRecipeHintRefused  = errors.New("recipe model hint refused")
	errModelRestoreFailed = errors.New("recipe model restoration failed")
)

// applyRecipeModelHint returns cleanup only after publishing the complete tuple.
// Pure lookup failure is advisory; every failure after resolution refuses the goal.
func applyRecipeModelHint(ctx context.Context, out io.Writer, sess *replSession, invocation recipeInvocationHint) (func(context.Context) error, error) {
	refused := func(err error) (func(context.Context) error, error) {
		return nil, fmt.Errorf("%w: %w", errRecipeHintRefused, err)
	}
	if sess.runtime == nil {
		return refused(errors.New("runtime unavailable"))
	}
	sel := sess.selection
	role, arm, key := invocation.hint.Role, "role", invocation.hint.Role
	var route providerbootstrap.PlannedRoute
	var err error
	if invocation.hint.UseCase != "" {
		arm, key = "use_case", invocation.hint.UseCase
	}
	if sel.effective != nil {
		cfg := sel.effective.Config()
		if arm == "use_case" {
			var ok bool
			role, ok = cfg.Defaults[key]
			if !ok {
				err = errors.New("use case has no configured default")
			}
		}
		if err == nil {
			route, err = providerbootstrap.PlanRoleRoute(cfg, role, modelSetUseCase)
		}
	} else {
		err = errors.New("no effective configuration")
	}
	if err != nil {
		_, _ = fmt.Fprintf(out, "recipes: %s: model hint %s %q unavailable; using current model\n", gitContextText(invocation.command), arm, key)
		return nil, nil
	}
	if !sel.useRecommend && slices.Equal(sel.chain, route.Chain) && sel.useCase == route.UseCase {
		return nil, nil
	}
	sw, admitted, err := prepareModelRoute(ctx, sess, route, role)
	if err != nil {
		if admitted {
			_, _ = fmt.Fprintln(out, destinationGrantRetainedNotice)
		}
		return refused(err)
	}
	saved := modelSwitch{selection: sel, tools: slices.Clone(sess.tools), orch: sess.orch, newOrch: sess.newOrchestrator, options: sess.runtime.ModelOptions(), budget: sess.runtime.Budget()}
	if err := sw.publishRecipeModel(ctx, sess); err != nil {
		if admitted {
			_, _ = fmt.Fprintln(out, destinationGrantRetainedNotice)
		}
		return refused(err)
	}
	if sw.thinkNotice != "" {
		_, _ = fmt.Fprintln(out, sw.thinkNotice)
	}
	for _, warning := range sw.warnings {
		_, _ = fmt.Fprintln(out, warning)
	}
	return func(ctx context.Context) error { return saved.publishRecipeModel(ctx, sess) }, nil
}

func (sw modelSwitch) publishRecipeModel(ctx context.Context, sess *replSession) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := sess.runtime.ReplaceConfiguration(ctx, golemruntime.Configuration{
		System: sess.baseSystem, Tools: sw.tools[sess.readToolCount:], ModelOptions: sw.options, Orchestrator: sw.orch, Budget: sw.budget,
	}); err != nil {
		return err
	}
	sess.selection, sess.tools, sess.orch, sess.newOrchestrator = sw.selection, sw.tools, sw.orch, sw.newOrch
	return nil
}
