package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/recipe"
)

func installModelRecipe(sess *replSession, hint *recipe.ModelHint) {
	r := reviewRecipe()
	r.ModelHint = hint
	sess.recipes = map[string]recipe.Recipe{"review": r}
}

func TestRecipeModelOneTurn(t *testing.T) {
	fx := newModelSwitchFixture(t, "")
	fx.withSession(t, nil, func(t *testing.T, sess *replSession) {
		installModelRecipe(sess, &recipe.ModelHint{Role: "swap"})
		before := snapshotModelTuple(sess).withoutTurnState()
		src := newCountingSource(sess, "/review \"Project A\"\nnext goal\n")
		var out strings.Builder
		if err := runREPL(t.Context(), src, &out, nil, sess); err != nil {
			t.Fatal(err)
		}
		if got := src.goals(); !reflect.DeepEqual(got, []string{"Review Project A. Focus on correctness.\n\nReport defects.", "next goal"}) {
			t.Fatalf("recall = %q", got)
		}
		if fx.alt.chats.Load() != 1 || fx.primary.chats.Load() != 1 {
			t.Fatalf("backend sequence: alt=%d primary=%d, want one each; output %s", fx.alt.chats.Load(), fx.primary.chats.Load(), out.String())
		}
		if !strings.Contains(fx.alt.chatBodies()[0], "Review Project A.") || !strings.Contains(fx.primary.chatBodies()[0], "next goal") {
			t.Fatal("wrong backend order")
		}
		if after := snapshotModelTuple(sess).withoutTurnState(); !reflect.DeepEqual(before, after) {
			t.Fatalf("persistent tuple changed: before %+v after %+v", before, after)
		}
		if sess.recipeHint != nil {
			t.Fatal("staged hint was not consumed")
		}
		res, err := sess.newOrchestrator().Run(t.Context(), agent.Request{Goal: "factory", MaxSteps: 2}, nil)
		if err != nil || res.Answer != "primary answer" {
			t.Fatalf("restored factory: %q %v", res.Answer, err)
		}
	})
}

func TestRecipeModelAdvisoryLookup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hint   recipe.ModelHint
		absent bool
		broken bool
		want   string
	}{
		{name: "missing role", hint: recipe.ModelHint{Role: "Reviewer"}, want: `role "Reviewer"`},
		{name: "bare model is not role", hint: recipe.ModelHint{Role: "alt/alt-model"}, want: `role "alt/alt-model"`},
		{name: "exact role", hint: recipe.ModelHint{Role: " swap "}, want: `role " swap "`},
		{name: "missing exact use case", hint: recipe.ModelHint{UseCase: "code-review"}, want: `use_case "code-review"`},
		{name: "absent config", hint: recipe.ModelHint{Role: "swap"}, absent: true, want: `role "swap"`},
		{name: "broken chain", hint: recipe.ModelHint{Role: "swap"}, broken: true, want: `role "swap"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newModelSwitchFixture(t, "")
			fx.withSession(t, nil, func(t *testing.T, sess *replSession) {
				if tc.absent {
					sess.selection.effective = nil
				}
				if tc.broken {
					cfg := sess.selection.effective.Config()
					m := cfg.Models["swap"]
					m.Fallbacks = []string{"absent"}
					cfg.Models["swap"] = m
				}
				installModelRecipe(sess, &tc.hint)
				src := newCountingSource(sess, "/review project\n")
				var out strings.Builder
				if err := runREPL(t.Context(), src, &out, nil, sess); err != nil {
					t.Fatal(err)
				}
				want := "recipes: /review: model hint " + tc.want + " unavailable; using current model\n"
				if strings.Count(out.String(), want) != 1 {
					t.Fatalf("missing exact advisory notice %q: %s", want, out.String())
				}
				if fx.primary.chats.Load() != 1 || fx.alt.chats.Load() != 0 || len(src.goals()) != 1 {
					t.Fatalf("fallback goal/history: primary %d alt %d recall %q", fx.primary.chats.Load(), fx.alt.chats.Load(), src.goals())
				}
			})
		})
	}
}

func TestRecipeModelConsentRefusal(t *testing.T) {
	for _, answer := range []string{"no", "yes"} {
		t.Run(answer, func(t *testing.T) {
			fx := newModelSwitchFixture(t, remoteModelURL)
			fx.withSession(t, nil, func(t *testing.T, sess *replSession) {
				installModelRecipe(sess, &recipe.ModelHint{Role: "cloudrole"})
				before := snapshotModelTuple(sess).withoutTurnState()
				src := newCountingSource(sess, "/review project\n"+answer+"\nnext goal\n")
				var out strings.Builder
				if err := runREPL(t.Context(), src, &out, nil, sess); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(src.goals(), []string{"next goal"}) || src.answers.Load() != 1 {
					t.Fatalf("recall %q answers %d", src.goals(), src.answers.Load())
				}
				if strings.Contains(out.String(), "unavailable; using current model") || strings.Contains(out.String(), destinationGrantRetainedNotice) != (answer == "yes") {
					t.Fatalf("consent incorrectly fell back or wrong retention notice: %s", out.String())
				}
				if fx.primary.chats.Load() != 1 || fx.alt.chats.Load() != 0 {
					t.Fatal("refused hint executed a fallback goal")
				}
				if after := snapshotModelTuple(sess).withoutTurnState(); !reflect.DeepEqual(before, after) {
					t.Fatalf("tuple changed: %+v", after)
				}
			})
		})
	}
}

func TestRecipeModelStrictShortcut(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(*replSession)
		shortcut bool
	}{
		{"same", func(*replSession) {}, true},
		{"different use case", func(s *replSession) { s.selection.useCase = "chat" }, false},
		{"recommendation", func(s *replSession) { s.selection.useRecommend = true }, false},
		{"ordered chain differs", func(s *replSession) {
			cfg := s.selection.effective.Config()
			m := cfg.Models["agent"]
			m.Fallbacks = []string{"agentb"}
			cfg.Models["agent"] = m
			s.selection.chain = []string{"primary/agent-model-b", "primary/agent-model"}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newModelSwitchFixture(t, "")
			fx.withSession(t, nil, func(t *testing.T, sess *replSession) {
				tc.mutate(sess)
				before := snapshotModelTuple(sess)
				requests := fx.primary.requests() + fx.alt.requests()
				restore, err := applyRecipeModelHint(t.Context(), io.Discard, sess, recipeInvocationHint{command: "/review", hint: recipe.ModelHint{Role: "agent"}})
				if err != nil {
					t.Fatal(err)
				}
				if (restore == nil) != tc.shortcut {
					t.Fatalf("shortcut %v, want %v", restore == nil, tc.shortcut)
				}
				if tc.shortcut {
					assertModelTupleUnchanged(t, before, sess)
					if fx.primary.requests()+fx.alt.requests() != requests {
						t.Fatal("shortcut performed preparation traffic")
					}
				} else {
					if err := restore(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
			})
		})
	}
}

func TestRecipeModelThinkingBudgetTraceAndPressure(t *testing.T) {
	fx := newModelSwitchFixture(t, "")
	fx.withSession(t, []string{"-think", "high", "-output-reserve", "1024", "-interceptors"}, func(t *testing.T, sess *replSession) {
		obs, err := newObserv(os.Getenv, fx.root, true, false, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		sess.obs = obs
		before := snapshotModelTuple(sess).withoutTurnState()
		var out strings.Builder
		res, err := runOnceWithRecipeHint(t.Context(), &out, nil, sess, interceptorScoredGoal, nil, &recipeInvocationHint{command: "/review", hint: recipe.ModelHint{Role: "swap"}})
		if err != nil || res.Answer != "alt answer" || res.Risk == nil || res.Risk.Score != 10 {
			t.Fatalf("hint turn %+v %v", res, err)
		}
		if !strings.Contains(out.String(), "think: model alt/alt-model does not support thinking; -think ignored\n") {
			t.Fatalf("thinking notice: %s", out.String())
		}
		if after := snapshotModelTuple(sess).withoutTurnState(); !reflect.DeepEqual(before, after) {
			t.Fatalf("restore options/budget: %+v want %+v", after, before)
		}
		if sess.pressure == nil || sess.lastModel != "alt/alt-model" {
			t.Fatal("hint pressure/model evidence discarded")
		}
		sample, seen := sess.pressure.snapshot()
		if !seen || sample.Pressure.InputBudget != 15360 {
			t.Fatalf("hinted pressure budget %+v", sample)
		}
		modelStatus := slash(t, sess, "/model")
		if !strings.Contains(modelStatus, "model: agent\nchain: primary/agent-model (strict; use case: agent)\n") || !strings.Contains(modelStatus, "last routed: alt/alt-model\n") {
			t.Fatalf("persistent selection / last route: %s", modelStatus)
		}
		status := slash(t, sess, "/context")
		if !strings.Contains(status, "configured input ceiling: 32768 tokens; explicit output reserve: 1024 tokens") {
			t.Fatalf("context %s", status)
		}
		files, _ := filepath.Glob(filepath.Join(obs.traceDir, "*.json"))
		if len(files) != 1 {
			t.Fatalf("trace files %v", files)
		}
		raw, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		type traceMetadata struct {
			Request struct {
				Budget agent.Budget `json:"budget"`
				Hash   string       `json:"tool_schema_hash"`
			} `json:"request"`
		}
		var rec traceMetadata
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Request.Budget.InputCeiling != 16384 || rec.Request.Budget.OutputReserve != 1024 || rec.Request.Hash != "fnv64:bc94f893b4083980" {
			t.Fatalf("hint metadata %+v", rec)
		}
		body := fx.alt.chatBodies()[0]
		if strings.Contains(body, `"reasoning_effort":"high"`) {
			t.Fatalf("unsupported thinking in hinted request: %s", body)
		}
		if _, err := runOnce(t.Context(), io.Discard, nil, sess, "next goal", nil); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(fx.primary.chatBodies()[0], `"reasoning_effort":"high"`) {
			t.Fatalf("thinking not restored on actual next request: %s", fx.primary.chatBodies()[0])
		}
		nextFiles, err := filepath.Glob(filepath.Join(obs.traceDir, "*.json"))
		if err != nil || len(nextFiles) != 2 {
			t.Fatalf("following trace files %v: %v", nextFiles, err)
		}
		for _, file := range nextFiles {
			if file == files[0] {
				continue
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var rec traceMetadata
			if err := json.Unmarshal(raw, &rec); err != nil {
				t.Fatal(err)
			}
			if rec.Request.Budget.InputCeiling != 32768 || rec.Request.Budget.OutputReserve != 1024 || rec.Request.Hash != "fnv64:bc94f893b4083980" {
				t.Fatalf("following ordinary trace metadata %+v", rec)
			}
		}
	})
}

func TestRecipeModelSelectionSequences(t *testing.T) {
	for _, mode := range []string{"prior set", "recommendation", "use case", "no hint", "slash-like", "two hints", "persistent"} {
		t.Run(mode, func(t *testing.T) {
			fx := newModelSwitchFixture(t, "")
			if mode == "recommendation" {
				fx.dropAgentDefault(t)
			}
			fx.withSessionOpts(t, mode != "persistent", nil, func(t *testing.T, sess *replSession) {
				hint := &recipe.ModelHint{Role: "swap"}
				if mode == "prior set" {
					slash(t, sess, "/model set agentb")
				}
				if mode == "use case" {
					sess.selection.effective.Config().Defaults["review"] = "swap"
					hint = &recipe.ModelHint{UseCase: "review"}
				}
				if mode == "no hint" {
					hint = nil
				}
				installModelRecipe(sess, hint)
				if mode == "slash-like" {
					r := sess.recipes["review"]
					r.Goal = "/model set weak"
					r.Context = ""
					sess.recipes["review"] = r
				}
				before := snapshotModelTuple(sess).withoutTurnState()
				script := "/review project\nnext goal\n"
				if mode == "two hints" {
					r := reviewRecipe()
					r.Name = "second"
					r.ModelHint = &recipe.ModelHint{Role: "agentb"}
					sess.recipes["second"] = r
					script = "/review project\n/second other\nnext goal\n"
				}
				src := newCountingSource(sess, script)
				if err := runREPL(t.Context(), src, io.Discard, nil, sess); err != nil {
					t.Fatal(err)
				}
				after := snapshotModelTuple(sess).withoutTurnState()
				if mode == "persistent" {
					if !strings.Contains(after.history, "Review project.") || strings.Contains(after.history, "/review") {
						t.Fatalf("persisted history %s", after.history)
					}
					before.history = after.history
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("selection did not restore: %+v want %+v", after, before)
				}
				wantAlt := int64(1)
				if mode == "no hint" {
					wantAlt = 0
				}
				if fx.alt.chats.Load() != wantAlt {
					t.Fatalf("alt requests %d want %d", fx.alt.chats.Load(), wantAlt)
				}
				if mode == "two hints" {
					b := fx.primary.chatBodies()
					if len(b) != 2 || !strings.Contains(b[0], `"model":"agent-model-b"`) || !strings.Contains(b[1], `"model":"agent-model"`) {
						t.Fatalf("ordered requests %q", b)
					}
				}
			})
		})
	}
}

func TestRecipeModelPreparationFailureAndCancellation(t *testing.T) {
	for _, mode := range []string{"capability", "cancellation", "runtime unavailable", "closed runtime"} {
		t.Run(mode, func(t *testing.T) {
			fx := newModelSwitchFixture(t, "")
			fx.withSession(t, nil, func(t *testing.T, sess *replSession) {
				sess.pressure = &pressureCapture{runID: "previous", seen: true, latest: contextFixture()}
				sess.lastModel = "primary/agent-model"
				before := snapshotModelTuple(sess)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				role := "weak"
				if mode == "cancellation" {
					role = "swap"
					cancel()
				}
				rt := sess.runtime
				if mode == "closed runtime" {
					role = "swap"
					if err := rt.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "runtime unavailable" {
					sess.runtime = nil
					role = "missing"
				}
				restore, err := applyRecipeModelHint(ctx, io.Discard, sess, recipeInvocationHint{command: "/review", hint: recipe.ModelHint{Role: role}})
				sess.runtime = rt
				if restore != nil || !errors.Is(err, errRecipeHintRefused) {
					t.Fatalf("preparation refusal %v restore %v", err, restore != nil)
				}
				if mode == "cancellation" && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation cause lost: %v", err)
				}
				if mode == "closed runtime" && !errors.Is(err, golemruntime.ErrClosed) {
					t.Fatalf("closed publication cause lost: %v", err)
				}
				assertModelTupleUnchanged(t, before, sess)
			})
		})
	}
}

type recipeAfterRunWriter struct {
	strings.Builder
	after func()
	match string
}

func (w *recipeAfterRunWriter) Write(p []byte) (int, error) {
	n, err := w.Builder.Write(p)
	if w.after != nil && strings.Contains(string(p), w.match) {
		f := w.after
		w.after = nil
		f()
	}
	return n, err
}

func TestRecipeModelRestorationFailureStopsREPL(t *testing.T) {
	for _, mode := range []string{"success", "provider", "secret", "canary"} {
		t.Run(mode, func(t *testing.T) {
			sensitive := mode == "secret" || mode == "canary"
			fx := newModelSwitchFixture(t, "")
			if mode == "provider" {
				fx.alt.chatResponse = func(w http.ResponseWriter, _ *http.Request, _ string) bool {
					http.Error(w, "provider refused", http.StatusBadRequest)
					return true
				}
			}
			if mode == "canary" {
				fx.alt.echoCanary.Store(true)
			}
			fx.withSession(t, []string{"-interceptors"}, func(t *testing.T, sess *replSession) {
				installModelRecipe(sess, &recipe.ModelHint{Role: "swap"})
				goal := "project"
				match := "done"
				if mode != "success" {
					match = "error:"
				}
				if mode == "secret" {
					goal = "sk-proj-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
					match = "error:"
				}
				out := &recipeAfterRunWriter{match: match, after: func() {
					if err := sess.runtime.Close(); err != nil {
						t.Error(err)
					}
				}}
				src := newCountingSource(sess, "/review "+goal+"\nnext goal\n")
				err := runREPL(t.Context(), src, out, nil, sess)
				if !errors.Is(err, errModelRestoreFailed) || !errors.Is(err, golemruntime.ErrClosed) {
					t.Fatalf("restore causes missing: %v; output %s", err, out.String())
				}
				if src.goalReads.Load() != 1 {
					t.Fatalf("read another goal after failed restore: %d", src.goalReads.Load())
				}
				wantRecall := 1
				if sensitive {
					wantRecall = 0
				}
				if len(src.goals()) != wantRecall {
					t.Fatalf("recall %q", src.goals())
				}
				if !strings.Contains(out.String(), "recipes: model restoration failed; REPL stopped\n") {
					t.Fatalf("fatal line missing: %s", out.String())
				}
				if sess.selection.requested != "swap" || sess.runtime.Budget().InputCeiling != 14336 {
					t.Fatal("CLI tuple diverged from last publication")
				}
				if sensitive {
					joined := errors.Join(err, errors.New("DISTINCTIVE_PRIVATE_RESTORE_BYTES"))
					if strings.Contains(runFailureMessage("", joined), "DISTINCTIVE_PRIVATE_RESTORE_BYTES") {
						t.Fatal("side error leaked through sensitive combined error")
					}
				}
			})
		})
	}
}

func TestRecipeModelMissingDefaultDoesNotSelectEmptyRole(t *testing.T) {
	fx := newModelSwitchFixture(t, "")
	fx.withSession(t, nil, func(t *testing.T, sess *replSession) {
		cfg := sess.selection.effective.Config()
		cfg.Models[""] = cfg.Models["swap"]
		installModelRecipe(sess, &recipe.ModelHint{UseCase: "unconfigured"})
		src := newCountingSource(sess, "/review project\n")
		var out strings.Builder
		if err := runREPL(t.Context(), src, &out, nil, sess); err != nil {
			t.Fatal(err)
		}
		const want = "recipes: /review: model hint use_case \"unconfigured\" unavailable; using current model\n"
		if strings.Count(out.String(), want) != 1 || fx.alt.chats.Load() != 0 || fx.primary.chats.Load() != 1 {
			t.Fatalf("missing exact default selected empty role: %s", out.String())
		}
	})
}

func TestRecipeModelInterruptRestores(t *testing.T) {
	for _, preparation := range []bool{true, false} {
		t.Run(fmt.Sprint(preparation), func(t *testing.T) {
			fx := newModelSwitchFixture(t, "")
			entered := make(chan struct{})
			if preparation {
				fx.alt.arm()
				entered = fx.alt.entered
			} else {
				fx.alt.chatResponse = func(w http.ResponseWriter, r *http.Request, _ string) bool {
					close(entered)
					<-r.Context().Done()
					return true
				}
			}
			fx.withSession(t, []string{"-think", "high", "-output-reserve", "1024"}, func(t *testing.T, sess *replSession) {
				installModelRecipe(sess, &recipe.ModelHint{Role: "swap"})
				before := snapshotModelTuple(sess).withoutTurnState()
				src := newCountingSource(sess, "/review project\nnext goal\n")
				interrupts := make(chan struct{}, 1)
				done := make(chan error, 1)
				var out strings.Builder
				go func() { done <- runREPL(t.Context(), src, &out, interrupts, sess) }()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("hint never reached barrier")
				}
				interrupts <- struct{}{}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("interrupt did not finish REPL")
				}
				want := []string{"next goal"}
				if !preparation {
					want = []string{"Review project. Focus on correctness.\n\nReport defects.", "next goal"}
				}
				if !reflect.DeepEqual(src.goals(), want) {
					t.Fatalf("canceled recall %q want %q", src.goals(), want)
				}
				if after := snapshotModelTuple(sess).withoutTurnState(); !reflect.DeepEqual(before, after) {
					t.Fatalf("cancellation restoration %+v want %+v", after, before)
				}
				if fx.primary.chats.Load() != 1 || !strings.Contains(fx.primary.chatBodies()[0], `"reasoning_effort":"high"`) {
					t.Fatalf("following original request %q", fx.primary.chatBodies())
				}
			})
		})
	}
}

func TestRecipeModelDispatchWholeTurn(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(fmt.Sprint(pinned), func(t *testing.T) {
			fx := newModelSwitchFixture(t, "")
			var parentSteps atomic.Int32
			fx.alt.chatResponse = func(w http.ResponseWriter, r *http.Request, body string) bool {
				// Only parent requests have dispatch. Its second step must also stay hinted.
				if !strings.Contains(body, `"name":"dispatch"`) {
					return false
				}
				if parentSteps.Add(1) != 1 {
					return false
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+`{"model":"alt-model","choices":[{"delta":{"tool_calls":[{"index":0,"id":"child-1","type":"function","function":{"name":"dispatch","arguments":"{\"tasks\":[\"child goal\"]}"}}]}}]}`+"\n\n")
				_, _ = io.WriteString(w, "data: "+`{"model":"alt-model","choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\ndata: [DONE]\n\n")
				return true
			}
			args := []string{"-dispatch", "-interceptors"}
			if pinned {
				args = append(args, "-dispatch-role", "agentb")
			}
			fx.withSession(t, args, func(t *testing.T, sess *replSession) {
				sess.stdinTerminal = true
				slash(t, sess, "/allow-write")
				before := snapshotModelTuple(sess).withoutTurnState()
				beforeCanary := sess.canary
				grantEntries := maps.Clone(sess.grants.keys)
				res, err := runOnceWithRecipeHint(t.Context(), io.Discard, nil, sess, interceptorScoredGoal, nil, &recipeInvocationHint{command: "/review", hint: recipe.ModelHint{Role: "swap"}})
				if err != nil || res.Answer != "alt answer" || parentSteps.Load() != 2 {
					t.Fatalf("multi-step parent %+v %v steps %d", res, err, parentSteps.Load())
				}
				if res.Risk == nil || res.Risk.Score != 10 {
					t.Fatalf("interceptors lost: %+v", res.Risk)
				}
				childBodies := fx.alt.chatBodies()
				wantChildModel := `"model":"alt-model"`
				if pinned {
					childBodies = fx.primary.chatBodies()
					wantChildModel = `"model":"agent-model-b"`
				}
				var child string
				for _, body := range childBodies {
					if !strings.Contains(body, `"name":"dispatch"`) {
						child = body
					}
				}
				if !strings.Contains(child, wantChildModel) {
					t.Fatalf("child actual model %s", child)
				}
				for _, name := range []string{"read_file", "search", "glob", "list"} {
					if !strings.Contains(child, `"name":"`+name+`"`) {
						t.Errorf("child missing %s", name)
					}
				}
				for _, name := range []string{"dispatch", "write_file", "edit_file"} {
					if strings.Contains(child, `"name":"`+name+`"`) {
						t.Errorf("child gained %s", name)
					}
				}
				if after := snapshotModelTuple(sess).withoutTurnState(); !reflect.DeepEqual(before, after) {
					t.Fatalf("dispatch tuple failed restoration %+v", after)
				}
				if !reflect.DeepEqual(sess.grants.keys, grantEntries) || sess.canary != beforeCanary {
					t.Fatal("hint altered grants or canary binding")
				}
				if _, err := runOnce(t.Context(), io.Discard, nil, sess, "next goal", nil); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestRecipeModelProviderAndCanaryFailure(t *testing.T) {
	for _, mode := range []string{"provider", "canary"} {
		t.Run(mode, func(t *testing.T) {
			fx := newModelSwitchFixture(t, "")
			if mode == "provider" {
				fx.alt.chatResponse = func(w http.ResponseWriter, _ *http.Request, _ string) bool {
					http.Error(w, "provider refused", http.StatusBadRequest)
					return true
				}
			} else {
				fx.alt.echoCanary.Store(true)
			}
			fx.withSession(t, []string{"-interceptors"}, func(t *testing.T, sess *replSession) {
				installModelRecipe(sess, &recipe.ModelHint{Role: "swap"})
				before := snapshotModelTuple(sess).withoutTurnState()
				src := newCountingSource(sess, "/review project\nnext goal\n")
				if err := runREPL(t.Context(), src, io.Discard, nil, sess); err != nil {
					t.Fatal(err)
				}
				want := []string{"Review project. Focus on correctness.\n\nReport defects.", "next goal"}
				if mode == "canary" {
					want = []string{"next goal"}
				}
				if !reflect.DeepEqual(src.goals(), want) {
					t.Fatalf("failure recall %q", src.goals())
				}
				after := snapshotModelTuple(sess).withoutTurnState()
				// Canary renewal legitimately rebuilds the persistent orchestrator.
				if mode == "canary" {
					before.orch = after.orch
				}
				if !reflect.DeepEqual(before, after) || fx.primary.chats.Load() != 1 {
					t.Fatalf("following goal did not use restored tuple %+v want %+v", after, before)
				}
			})
		})
	}
}

func TestRecipeModelPreRunFailuresRestore(t *testing.T) {
	for _, mode := range []string{"journal", "persistence", "project refresh", "canary renewal"} {
		t.Run(mode, func(t *testing.T) {
			fx := newModelSwitchFixture(t, "")
			fx.withSessionOpts(t, mode != "persistence", []string{"-interceptors"}, func(t *testing.T, sess *replSession) {
				before := snapshotModelTuple(sess).withoutTurnState()
				sentinel := errors.New("journal failed")
				if mode == "journal" {
					sess.journal = &checkpointJournal{fatal: sentinel}
				}
				if mode == "persistence" {
					failConversationSave(t, sess.session.db)
				}
				if mode == "project refresh" {
					docs, err := loadProjectContextDocs(t.Context(), sess.root, os.Getenv)
					if err != nil {
						t.Fatal(err)
					}
					state := newProjectContextState(sess.root, docs, false, nil)
					sess.projectContext = &state
				}
				if mode == "canary renewal" {
					sess.canary.burn()
					sess.canary.entropy = errorCanaryReader{}
				}
				ctx := t.Context()
				if mode == "project refresh" {
					c, cancel := context.WithCancel(ctx)
					cancel()
					ctx = c
				}
				var out strings.Builder
				_, err := runOnceWithRecipeHint(ctx, &out, nil, sess, "review project", nil, &recipeInvocationHint{command: "/review", hint: recipe.ModelHint{Role: "swap"}})
				if mode == "journal" && !errors.Is(err, sentinel) {
					t.Fatalf("journal cause %v", err)
				}
				if mode == "persistence" {
					if err != nil || !strings.Contains(out.String(), "warning: session not saved:") {
						t.Fatalf("persistence outcome %v %s", err, out.String())
					}
				} else if err == nil {
					t.Fatalf("expected %s failure", mode)
				}
				if after := snapshotModelTuple(sess).withoutTurnState(); !reflect.DeepEqual(before, after) {
					t.Fatalf("failure changed persistent tuple %+v want %+v", after, before)
				}
				wantAlt := int64(0)
				if mode == "persistence" {
					wantAlt = 1
				}
				if fx.alt.chats.Load() != wantAlt {
					t.Fatalf("candidate chat calls %d", fx.alt.chats.Load())
				}
				sess.journal = nil
				sess.projectContext = nil
				if mode == "canary renewal" {
					return
				}
				if _, err := runOnce(t.Context(), io.Discard, nil, sess, "next goal", nil); err != nil {
					t.Fatal(err)
				}
				if fx.primary.chats.Load() != 1 {
					t.Fatal("following goal did not restore original model")
				}
			})
		})
	}
}

func TestRecipeModelCurrentSystemAndTrustedContext(t *testing.T) {
	fx := newModelSwitchFixture(t, "")
	fx.withSession(t, nil, func(t *testing.T, sess *replSession) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		writeTrustDocument(t, sess.root, "original recipe project guidance")
		docs, err := loadProjectContextDocs(t.Context(), sess.root, os.Getenv)
		if err != nil {
			t.Fatal(err)
		}
		state := newProjectContextState(sess.root, docs, false, nil)
		sess.projectContext = &state
		slash(t, sess, "/trust "+trustFixtureDigest(t, sess.root))
		writeTrustDocument(t, sess.root, "updated recipe project guidance")
		slash(t, sess, "/trust "+trustFixtureDigest(t, sess.root))
		_, err = runOnceWithRecipeHint(t.Context(), io.Discard, nil, sess, "review", nil, &recipeInvocationHint{command: "/review", hint: recipe.ModelHint{Role: "swap"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = runOnce(t.Context(), io.Discard, nil, sess, "next goal", nil); err != nil {
			t.Fatal(err)
		}
		for _, body := range []string{fx.alt.chatBodies()[0], fx.primary.chatBodies()[0]} {
			if !strings.Contains(body, "updated recipe project guidance") || strings.Contains(body, "original recipe project guidance") {
				t.Fatalf("stale project context restored: %s", body)
			}
		}
		// The restoration must read the current system even when a legitimate
		// publication changes it after the saved model tuple was captured.
		restore, err := applyRecipeModelHint(t.Context(), io.Discard, sess, recipeInvocationHint{command: "/review", hint: recipe.ModelHint{Role: "swap"}})
		if err != nil {
			t.Fatal(err)
		}
		sess.baseSystem += "\nrenewed system bytes"
		if err := restore(t.Context()); err != nil {
			t.Fatal(err)
		}
		sess.projectContext = nil
		if _, err := runOnce(t.Context(), io.Discard, nil, sess, "after current system", nil); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(fx.primary.chatBodies()[1], "renewed system bytes") {
			t.Fatal("restoration restored stale system")
		}
	})
}

func TestRecipeModelHintConsumedOnEarlyExit(t *testing.T) {
	for _, mode := range []string{"utf8", "size", "admission", "shortcut revoked consent"} {
		t.Run(mode, func(t *testing.T) {
			fx := newModelSwitchFixture(t, remoteModelURL)
			fx.withSession(t, nil, func(t *testing.T, sess *replSession) {
				role := "swap"
				if mode == "shortcut revoked consent" {
					role = "agent"
				}
				installModelRecipe(sess, &recipe.ModelHint{Role: role})
				script := "/review project\n"
				if mode == "utf8" || mode == "size" {
					r := sess.recipes["review"]
					r.Goal = string([]byte{0xff})
					if mode == "size" {
						r.Goal = strings.Repeat("x", maxGoalBytes+1)
					}
					sess.recipes["review"] = r
				}
				if mode == "admission" || mode == "shortcut revoked consent" {
					route, err := providerbootstrap.PlanRoleRoute(sess.selection.effective.Config(), "cloudrole", "agent")
					if err != nil {
						t.Fatal(err)
					}
					plan, err := providerbootstrap.BuildNetworkPlan(sess.selection.effective, []providerbootstrap.PlannedRoute{route}, sess.selection.planOpts)
					if err != nil {
						t.Fatal(err)
					}
					sess.destAdmission.setPrompt(func(context.Context, string) (bool, error) { return true, nil })
					if _, err := sess.destAdmission.extend(t.Context(), plan.Edges); err != nil {
						t.Fatal(err)
					}
					sess.destAdmission.revoke()
					script += "no\n"
				}
				before := snapshotModelTuple(sess)
				requests := fx.alt.requests() + fx.primary.requests()
				src := newCountingSource(sess, script)
				if err := runREPL(t.Context(), src, io.Discard, nil, sess); err != nil {
					t.Fatal(err)
				}
				if sess.recipeHint != nil || len(src.goals()) != 0 || fx.alt.requests()+fx.primary.requests() != requests {
					t.Fatalf("early exit leaked hint, history, or provider traffic: hint %v recall %q", sess.recipeHint, src.goals())
				}
				assertModelTupleUnchanged(t, before, sess)
				sess.destAdmission.setPrompt(func(context.Context, string) (bool, error) { return true, nil })
				if err := sess.destAdmission.ensure(t.Context()); err != nil {
					t.Fatal(err)
				}
				sess.goalEditor = &fakeGoalEditor{available: true, text: "edit next goal"}
				src = newCountingSource(sess, "/edit\nordinary next goal\n")
				if err := runREPL(t.Context(), src, io.Discard, nil, sess); err != nil {
					t.Fatal(err)
				}
				if fx.alt.chats.Load() != 0 || fx.primary.chats.Load() != 2 {
					t.Fatal("early hint reached a later edit/ordinary goal")
				}
			})
		})
	}
}

// Forward every real registry operation; the count pins admission ordering.
type recipeObservedModels struct {
	capChecker
	lookups int
}

func (m *recipeObservedModels) Lookup(ctx context.Context, key provider.ModelKey) (*provider.ModelProfile, error) {
	m.lookups++
	return m.capChecker.Lookup(ctx, key)
}

type recipeConsentSource struct {
	lineSource
	beforeAnswer func()
}

func (s recipeConsentSource) ReadAnswer(ctx context.Context, prompt string) (string, bool, error) {
	s.beforeAnswer()
	return s.lineSource.ReadAnswer(ctx, prompt)
}

func TestRecipeModelMetadataFollowsConsent(t *testing.T) {
	for _, answer := range []string{"no", "yes"} {
		t.Run(answer, func(t *testing.T) {
			fx := newModelSwitchFixture(t, remoteModelURL)
			fx.withSession(t, nil, func(t *testing.T, sess *replSession) {
				observed := &recipeObservedModels{capChecker: sess.selection.models}
				sess.selection.models = observed
				installModelRecipe(sess, &recipe.ModelHint{Role: "cloudrole"})
				counted := newCountingSource(sess, "/review project\n"+answer+"\n")
				src := recipeConsentSource{lineSource: counted, beforeAnswer: func() {
					if observed.lookups != 0 {
						t.Errorf("candidate metadata before consent: %d", observed.lookups)
					}
				}}
				sess.destAdmission.setPrompt(lineSourcePromptYN(src))
				if err := runREPL(t.Context(), src, io.Discard, nil, sess); err != nil {
					t.Fatal(err)
				}
				if counted.answers.Load() != 1 || len(counted.goals()) != 0 {
					t.Fatalf("answers %d recall %q", counted.answers.Load(), counted.goals())
				}
				if (observed.lookups > 0) != (answer == "yes") {
					t.Fatalf("metadata count %d after %s", observed.lookups, answer)
				}
			})
		})
	}
}

func TestRecipeModelSealTurnRestoration(t *testing.T) {
	for _, mode := range []string{"storage failure", "canceled run uses original cleanup context"} {
		t.Run(mode, func(t *testing.T) {
			fx := newModelSwitchFixture(t, "")
			journal, _, _ := newJournalFixture(t)
			entered := make(chan struct{})
			fx.alt.chatResponse = func(w http.ResponseWriter, r *http.Request, _ string) bool {
				prepared, err := journal.Prepare(testRec("a.txt", "A0", true))
				if err != nil {
					t.Errorf("prepare checkpoint: %v", err)
					return false
				}
				if mode == "storage failure" {
					if err := journal.store.db.Close(); err != nil {
						t.Error(err)
					}
					return false
				}
				if err := prepared.Abort(); err != nil {
					t.Errorf("abort intent: %v", err)
				}
				close(entered)
				<-r.Context().Done()
				return true
			}
			fx.withSession(t, nil, func(t *testing.T, sess *replSession) {
				sess.journal = journal
				before := snapshotModelTuple(sess).withoutTurnState()
				interrupts := make(chan struct{}, 1)
				done := make(chan error, 1)
				var out strings.Builder
				go func() {
					_, err := runOnceWithRecipeHint(t.Context(), &out, interrupts, sess, "review project", nil, &recipeInvocationHint{command: "/review", hint: recipe.ModelHint{Role: "swap"}})
					done <- err
				}()
				if mode != "storage failure" {
					select {
					case <-entered:
					case <-time.After(5 * time.Second):
						t.Fatal("checkpoint was not prepared")
					}
					interrupts <- struct{}{}
				}
				var err error
				select {
				case err = <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("hint did not finish sealing")
				}
				if mode == "storage failure" {
					if journal.fatal == nil || !errors.Is(err, journal.fatal) || !strings.Contains(out.String(), "checkpoint:") {
						t.Fatalf("seal failure not preserved: %v, fatal %v, output %s", err, journal.fatal, out.String())
					}
				} else if !errors.Is(err, context.Canceled) || journal.fatal != nil || journal.cpID != 0 {
					t.Fatalf("canceled run failed original-context seal: %v; fatal %v cpID %d", err, journal.fatal, journal.cpID)
				}
				if after := snapshotModelTuple(sess).withoutTurnState(); !reflect.DeepEqual(before, after) {
					t.Fatalf("seal failure did not restore tuple %+v want %+v", after, before)
				}
				// A failed storage device prevents future journaled runs; inspecting the
				// restored route separately must still use the persistent configuration.
				sess.journal = nil
				if _, err := runOnce(t.Context(), io.Discard, nil, sess, "next goal", nil); err != nil {
					t.Fatal(err)
				}
				if fx.primary.chats.Load() != 1 || !strings.Contains(fx.primary.chatBodies()[0], `"model":"agent-model"`) {
					t.Fatalf("following request %q", fx.primary.chatBodies())
				}
			})
		})
	}
}

func TestRecipeModelActualFallbackRoute(t *testing.T) {
	for _, arm := range []string{"role", "use_case"} {
		t.Run(arm, func(t *testing.T) {
			fx := newModelSwitchFixture(t, "")
			fx.alt.chatResponse = func(w http.ResponseWriter, _ *http.Request, _ string) bool {
				http.Error(w, "candidate failed", http.StatusServiceUnavailable)
				return true
			}
			fx.withSession(t, nil, func(t *testing.T, sess *replSession) {
				cfg := sess.selection.effective.Config()
				model := cfg.Models["swap"]
				model.Fallbacks = []string{"agentb"}
				cfg.Models["swap"] = model
				cfg.Defaults["code-review"] = "swap"
				hint := recipe.ModelHint{Role: "swap"}
				if arm == "use_case" {
					hint = recipe.ModelHint{UseCase: "code-review"}
				}
				result, err := runOnceWithRecipeHint(t.Context(), io.Discard, nil, sess, "review fallback", nil, &recipeInvocationHint{command: "/review", hint: hint})
				if err != nil || result.Answer != "primary answer" || len(result.Steps) != 1 {
					t.Fatalf("hinted fallback %+v: %v", result, err)
				}
				route := result.Steps[0].RouteOutcome
				if route == nil || route.UseCase != "agent" || len(route.Attempts) != 2 {
					t.Fatalf("actual hinted route %+v", route)
				}
				got := []provider.ModelKey{route.Attempts[0].Key, route.Attempts[1].Key}
				want := []provider.ModelKey{{Provider: "alt", Model: "alt-model"}, {Provider: "primary", Model: "agent-model-b"}}
				if !reflect.DeepEqual(got, want) || route.PlannedModel != want[0] || route.ActualModel != want[1] || route.FallbacksUsed != 1 {
					t.Fatalf("ordered actual route %+v; want %v", route, want)
				}
				if fx.alt.chats.Load() != 1 || fx.primary.chats.Load() != 1 || !strings.Contains(fx.alt.chatBodies()[0], `"model":"alt-model"`) || !strings.Contains(fx.primary.chatBodies()[0], `"model":"agent-model-b"`) {
					t.Fatalf("wire request order alt %q primary %q", fx.alt.chatBodies(), fx.primary.chatBodies())
				}
			})
		})
	}
}

// TestRecipeModelUseCaseFallbackResolves pins that a use_case hint resolves
// through the config's side-task fallback table exactly as the router and
// -goal planning do: "planning" has no explicit default here, so it must reach
// the "reasoning" default (swap -> alt backend) instead of degrading to the
// current model with an advisory notice. A literal Defaults[key] lookup fails
// this test.
func TestRecipeModelUseCaseFallbackResolves(t *testing.T) {
	fx := newModelSwitchFixture(t, "")
	fx.withSession(t, nil, func(t *testing.T, sess *replSession) {
		cfg := sess.selection.effective.Config()
		cfg.Defaults["reasoning"] = "swap"
		installModelRecipe(sess, &recipe.ModelHint{UseCase: "planning"})
		src := newCountingSource(sess, "/review project\n")
		var out strings.Builder
		if err := runREPL(t.Context(), src, &out, nil, sess); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "unavailable; using current model") {
			t.Fatalf("fallback use case treated as unbound: %s", out.String())
		}
		if fx.alt.chats.Load() != 1 || fx.primary.chats.Load() != 0 || len(src.goals()) != 1 {
			t.Fatalf("fallback route not used: primary %d alt %d recall %q: %s", fx.primary.chats.Load(), fx.alt.chats.Load(), src.goals(), out.String())
		}
		if status := slash(t, sess, "/model"); !strings.Contains(status, "chain: primary/agent-model (strict; use case: agent)\n") {
			t.Fatalf("persistent selection not restored after fallback hint: %s", status)
		}
	})
}
