package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agentflow"
)

func TestResumeRefusesUnverifiedExistingProofBeforeMutation(t *testing.T) {
	for _, test := range retainedTimeoutStates {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newResumeFixture(t)
			fixture.useAdvisoryLease()
			initial := test.state(t, &fixture)
			writeAgentFile(t, fixture.root, "proof-pack.json", "retained proof")
			verificationErr := errors.New("existing proof is invalid")
			af := &fakeAF{
				nextSteps:   []string{"P1"},
				nextActions: []agentflow.NextActionState{initial},
				failAt:      map[string]error{"verify-proof": verificationErr},
			}
			models := 0
			d := &driver{af: af, plan: recoveryPlan(), root: fixture.root,
				runStep: func(context.Context, agentflow.Step, string, string) error {
					models++
					return nil
				},
			}
			_, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
			if !errors.Is(err, verificationErr) || !strings.HasPrefix(err.Error(), "verify existing proof before resume") {
				t.Errorf("resume(%s) error = %v, want verification failure wrapped with pre-resume context", test.name, err)
			}
			if want := []string{"probe", "next-action", "verify-proof"}; !reflect.DeepEqual(af.seq, want) || models != 0 {
				t.Errorf("resume(%s) calls = %v, model calls = %d, want %v and zero models", test.name, af.seq, models, want)
			}
			proof, readErr := os.ReadFile(filepath.Join(fixture.root, ".agent", "proof-pack.json"))
			if readErr != nil || string(proof) != "retained proof" {
				t.Errorf("resume(%s) retained proof = %q, error = %v, want original proof", test.name, proof, readErr)
			}
		})
	}
}

// proofChangingAF acts as another writer at an existing command boundary.
// The third NextAction is the final step-work check on both recovery paths.
type proofChangingAF struct {
	*fakeAF
	afterVerify     func()
	afterNextAction func(int)
	nextActionCalls int
}

func (f *proofChangingAF) VerifyProof(ctx context.Context) error {
	err := f.fakeAF.VerifyProof(ctx)
	if f.afterVerify != nil {
		f.afterVerify()
	}
	return err
}

func (f *proofChangingAF) NextAction(ctx context.Context) (agentflow.NextActionState, error) {
	state, err := f.fakeAF.NextAction(ctx)
	f.nextActionCalls++
	if f.afterNextAction != nil {
		f.afterNextAction(f.nextActionCalls)
	}
	return state, err
}

func TestResumeProofChangeBeforeFinishRun(t *testing.T) {
	for _, path := range []string{"settlement", "serial"} {
		for _, change := range []string{"changed", "created empty", "removed", "unchanged"} {
			t.Run(path+"/"+change, func(t *testing.T) {
				t.Parallel()
				fixture := newResumeFixture(t)
				fixture.useAdvisoryLease()
				var initial, second agentflow.NextActionState
				var nextSteps []string
				wantModels := 0
				if path == "settlement" {
					initial = fixture.attemptState(t, "step_uncompleted", nil)
					second = fixture.noAttemptState(t, "run_unverified", false)
				} else {
					initial = fixture.noAttemptState(t, "step_unclaimed", true)
					inFlight := newResumeFixture(t)
					inFlight.useAdvisoryLease()
					second = inFlight.attemptState(t, "validation_missing", []agentflow.ResumabilityGate{
						{Kind: "command", Label: "go test ./...", Status: "missing"},
						{Kind: "command", Label: "go vet ./...", Status: "missing"},
					})
					second.Resumability.Attempt.ID = "A-P1"
					nextSteps, wantModels = []string{"P1"}, 1
				}
				complete := fixture.noAttemptState(t, "complete", false)
				proofPath := filepath.Join(fixture.root, ".agent", "proof-pack.json")
				if change != "created empty" {
					writeAgentFile(t, fixture.root, "proof-pack.json", "verified proof")
				}
				af := &proofChangingAF{fakeAF: &fakeAF{
					nextSteps:   nextSteps,
					nextActions: []agentflow.NextActionState{initial, second, finishRunGuard, complete},
				}}
				af.afterNextAction = func(call int) {
					if call != 3 {
						return
					}
					switch change {
					case "changed":
						writeAgentFile(t, fixture.root, "proof-pack.json", "replacement proof")
					case "created empty":
						writeAgentFile(t, fixture.root, "proof-pack.json", "")
					case "removed":
						if err := os.Remove(proofPath); err != nil {
							t.Fatal(err)
						}
					}
				}
				models := 0
				d := &driver{af: af, plan: recoveryPlan(), root: fixture.root,
					runStep: func(context.Context, agentflow.Step, string, string) error {
						models++
						return nil
					},
				}
				final, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
				if models != wantModels || af.nextActionCalls < 3 {
					t.Errorf("resume(%s, %s) model calls = %d, next-action calls = %d, want %d and final step-work check", path, change, models, af.nextActionCalls, wantModels)
				}
				if change == "unchanged" {
					if err != nil || final.State != "complete" || !slices.Contains(af.seq, "verify-proof") || !slices.Contains(af.seq, "finish-run") {
						t.Errorf("resume(%s, unchanged) state = %q, error = %v, calls = %v, want verified proof accepted and finish-run completed", path, final.State, err, af.seq)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "proof changed during resume") || slices.Contains(af.seq, "finish-run") {
					t.Errorf("resume(%s, %s) error = %v, calls = %v, want proof-change refusal before finish-run", path, change, err, af.seq)
				}
				proof, readErr := os.ReadFile(proofPath)
				if change == "removed" {
					if !errors.Is(readErr, os.ErrNotExist) {
						t.Errorf("resume(%s, removed) proof read error = %v, want no recreated proof", path, readErr)
					}
				} else {
					wantProof := "replacement proof"
					if change == "created empty" {
						wantProof = ""
					}
					if readErr != nil || string(proof) != wantProof {
						t.Errorf("resume(%s, %s) proof = %q, error = %v, want untouched %q", path, change, proof, readErr, wantProof)
					}
				}
			})
		}
	}
}

func TestResumeRefusesProofChangedDuringVerification(t *testing.T) {
	t.Parallel()
	fixture := newResumeFixture(t)
	fixture.useAdvisoryLease()
	initial := fixture.attemptState(t, "step_uncompleted", nil)
	writeAgentFile(t, fixture.root, "proof-pack.json", "original proof")
	af := &proofChangingAF{
		fakeAF: &fakeAF{nextActions: []agentflow.NextActionState{initial}},
		afterVerify: func() {
			writeAgentFile(t, fixture.root, "proof-pack.json", "unverified replacement")
		},
	}
	d := &driver{af: af, plan: recoveryPlan(), root: fixture.root}
	_, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
	if err == nil || !strings.Contains(err.Error(), "proof changed during resume") {
		t.Errorf("resume(proof changed during verification) error = %v, want proof-change refusal", err)
	}
	if want := []string{"probe", "next-action", "verify-proof"}; !reflect.DeepEqual(af.seq, want) {
		t.Errorf("resume(proof changed during verification) calls = %v, want %v before settlement", af.seq, want)
	}
}
