//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
)

func TestCredentialScreenBudget(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	for _, tc := range []struct {
		label   string
		set     time.Duration
		wantErr string
		want    time.Duration
	}{
		{"0", 0, "", 5 * time.Second},
		{"5s", 5 * time.Second, "", 5 * time.Second},
		{"6s", 6 * time.Second, "", 6 * time.Second},
		{"5m", 5 * time.Minute, "", 5 * time.Minute},
		{"4999ms", 4999 * time.Millisecond, "tools: credential screen budget 4.999s is outside 5s to 5m0s", 7 * time.Second},
		{"5m0.000000001s", 5*time.Minute + time.Nanosecond, "tools: credential screen budget 5m0.000000001s is outside 5s to 5m0s", 7 * time.Second},
		{"-1s", -time.Second, "tools: credential screen budget -1s is outside 5s to 5m0s", 7 * time.Second},
	} {
		t.Run(tc.label, func(t *testing.T) {
			ws, err := NewWorkspace(t.TempDir())
			if err != nil {
				failNoPath(t, tc.label, err)
			}
			if err := ws.SetCredentialScreenBudget(7 * time.Second); err != nil {
				t.Fatalf("%s: setting 7s failed", tc.label)
			}
			err = ws.SetCredentialScreenBudget(tc.set)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("%s: unexpected error %q", tc.label, err)
			case tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr):
				t.Fatalf("%s: error = %v, want %q", tc.label, err, tc.wantErr)
			}
			if got := ws.screenBudget(); got != tc.want {
				t.Fatalf("%s: screenBudget = %s, want %s", tc.label, got, tc.want)
			}
		})
	}
}

func TestCredentialScreenBudgetWalk(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	files := map[string]string{
		"a/f":    "plain\n",
		"b/.env": "TOKEN=budget-0001-abcdef\n",
	}
	t.Run("expires", func(t *testing.T) {
		ws, _, _ := tolerantFixture(t, files, nil)
		if err := ws.SetCredentialScreenBudget(8 * time.Second); err != nil {
			t.Fatal("expires: setting 8s failed")
		}
		var mu sync.Mutex
		askedB := false
		ws.beforeReadDir = func(rel string) {
			switch rel {
			case "a":
				time.Sleep(5500 * time.Millisecond)
			case "b":
				mu.Lock()
				askedB = true
				mu.Unlock()
				time.Sleep(3 * time.Second)
			}
		}
		s := mustBuild(t, "expires", ws)
		mu.Lock()
		defer mu.Unlock()
		if !askedB {
			t.Fatal("expires: b was never listed")
		}
		want := withheldNote + "time budget of 8s exhausted)"
		if notes := s.notes(); len(notes) != 1 || notes[0] != want {
			t.Fatalf("expires: notes differ (%d notes)", len(notes))
		}
	})
	t.Run("canceled mid-walk", func(t *testing.T) {
		ws, _, _ := tolerantFixture(t, files, nil)
		if err := ws.SetCredentialScreenBudget(5 * time.Minute); err != nil {
			t.Fatal("canceled mid-walk: setting 5m failed")
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		ws.beforeReadDir = func(rel string) {
			if rel == "b" {
				cancel()
			}
		}
		var mu sync.Mutex
		var opened []string
		ws.screenSeams = &screenSeams{openErr: func(rel string) error {
			mu.Lock()
			opened = append(opened, rel)
			mu.Unlock()
			return nil
		}}
		start := time.Now()
		s, err := buildOutputScreen(ctx, ws)
		if elapsed := time.Since(start); elapsed > 30*time.Second {
			t.Fatalf("canceled mid-walk: build took %s", elapsed)
		}
		if s != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled mid-walk: screen returned %t, error is Canceled %t", s != nil, errors.Is(err, context.Canceled))
		}
		mu.Lock()
		defer mu.Unlock()
		if slices.Contains(opened, "b/.env") {
			t.Fatal("canceled mid-walk: the walk went on after the cancel")
		}
	})
	// The control: with no cancel the same seam records b/.env, so the row
	// above would see a walk that went on.
	t.Run("not canceled (control)", func(t *testing.T) {
		ws, _, _ := tolerantFixture(t, files, nil)
		var mu sync.Mutex
		var opened []string
		ws.screenSeams = &screenSeams{openErr: func(rel string) error {
			mu.Lock()
			opened = append(opened, rel)
			mu.Unlock()
			return nil
		}}
		s := mustBuild(t, "not canceled (control)", ws)
		if notes := s.notes(); len(notes) != 0 {
			t.Fatalf("not canceled (control): %d notes, want a complete screen", len(notes))
		}
		mu.Lock()
		defer mu.Unlock()
		if !slices.Contains(opened, "b/.env") {
			t.Fatal("not canceled (control): b/.env was never asked to open")
		}
	})
}

// budgetTools are the pieces of one options constructor's tool set the
// budget rows read.
type budgetTools struct {
	rc *RunCommand
	sc *StartCommand // nil for the foreground-only constructors
}

func TestExecToolsOptionsScreenBudget(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	type ctor struct {
		label string
		build func(t *testing.T, root string, opts ExecToolsOptions) ([]agent.Tool, error)
	}
	ctors := []ctor{
		{"NewExecToolsWithOptions", func(_ *testing.T, root string, opts ExecToolsOptions) ([]agent.Tool, error) {
			return NewExecToolsWithOptions(root, opts)
		}},
		{"NewSandboxedExecToolsWithOptions", func(_ *testing.T, root string, opts ExecToolsOptions) ([]agent.Tool, error) {
			return NewSandboxedExecToolsWithOptions(root, SandboxConfig{}, opts)
		}},
		{"NewExecToolsWithBackgroundOptions", func(t *testing.T, root string, opts ExecToolsOptions) ([]agent.Tool, error) {
			m, err := NewSandboxedBackgroundManager(SandboxConfig{})
			if err != nil {
				return nil, err
			}
			t.Cleanup(m.Shutdown)
			return NewExecToolsWithBackgroundOptions(root, m, opts)
		}},
	}
	pick := func(t *testing.T, label string, set []agent.Tool) budgetTools {
		t.Helper()
		var bt budgetTools
		for _, tool := range set {
			switch v := tool.(type) {
			case *RunCommand:
				bt.rc = v
			case *StartCommand:
				bt.sc = v
			}
		}
		if bt.rc == nil {
			t.Fatalf("%s: no run_command", label)
		}
		return bt
	}
	build := func(t *testing.T, label string, c ctor, scratch bool, budget time.Duration) budgetTools {
		t.Helper()
		opts := ExecToolsOptions{CredentialScreenBudget: budget}
		if scratch {
			opts.Scratch = ScratchConfig{Enabled: true}
		}
		set, err := c.build(t, t.TempDir(), opts)
		if err != nil {
			failNoPath(t, label, err)
		}
		return pick(t, label, set)
	}
	planTimeout := func(t *testing.T, label string, tool agent.PlanningTool) time.Duration {
		t.Helper()
		p, err := tool.Plan(t.Context(), json.RawMessage(`{"argv":["true"]}`))
		if err != nil {
			failNoPath(t, label+": plan", err)
		}
		return p.Effect.Timeout
	}
	for _, c := range ctors {
		for _, scratch := range []bool{false, true} {
			mode := c.label + "/scratch off"
			if scratch {
				mode = c.label + "/scratch on"
			}
			t.Run(mode+"/raised", func(t *testing.T) {
				hi := build(t, mode+" 7s", c, scratch, 7*time.Second)
				lo := build(t, mode+" 5s", c, scratch, 5*time.Second)
				if got := hi.rc.ws.screenBudget(); got != 7*time.Second {
					t.Fatalf("%s: run_command budget = %s", mode, got)
				}
				if d := planTimeout(t, mode, hi.rc) - planTimeout(t, mode, lo.rc); d != 2*time.Second {
					t.Fatalf("%s: run_command plan timeout differs by %s, want 2s", mode, d)
				}
				if hi.sc == nil {
					if c.label == "NewExecToolsWithBackgroundOptions" {
						t.Fatalf("%s: no start_command", mode)
					}
					return
				}
				if got := hi.sc.ws.screenBudget(); got != 7*time.Second {
					t.Fatalf("%s: start_command budget = %s", mode, got)
				}
				if got := hi.sc.Effect().Timeout; got != bgToolTimeout+7*time.Second {
					t.Fatalf("%s: start_command effect timeout = %s", mode, got)
				}
				if d := hi.sc.Effect().Timeout - lo.sc.Effect().Timeout; d != 2*time.Second {
					t.Fatalf("%s: start_command effect timeout differs by %s, want 2s", mode, d)
				}
				if scratch {
					if d := planTimeout(t, mode, hi.sc) - planTimeout(t, mode, lo.sc); d != 2*time.Second {
						t.Fatalf("%s: start_command plan timeout differs by %s, want 2s", mode, d)
					}
				}
			})
			t.Run(mode+"/zero", func(t *testing.T) {
				bt := build(t, mode, c, scratch, 0)
				if got := bt.rc.ws.screenBudget(); got != 5*time.Second {
					t.Fatalf("%s: run_command budget = %s", mode, got)
				}
				if bt.sc != nil {
					if got := bt.sc.ws.screenBudget(); got != 5*time.Second {
						t.Fatalf("%s: start_command budget = %s", mode, got)
					}
				}
			})
			for _, b := range []struct {
				label string
				d     time.Duration
				err   string
			}{
				{"5s", 5 * time.Second, ""},
				{"5m", 5 * time.Minute, ""},
				{"4999ms", 4999 * time.Millisecond, "tools: credential screen budget 4.999s is outside 5s to 5m0s"},
				{"5m0.000000001s", 5*time.Minute + time.Nanosecond, "tools: credential screen budget 5m0.000000001s is outside 5s to 5m0s"},
				{"-1s", -time.Second, "tools: credential screen budget -1s is outside 5s to 5m0s"},
			} {
				t.Run(mode+"/"+b.label, func(t *testing.T) {
					opts := ExecToolsOptions{CredentialScreenBudget: b.d}
					if scratch {
						opts.Scratch = ScratchConfig{Enabled: true}
					}
					set, err := c.build(t, t.TempDir(), opts)
					if b.err == "" {
						if err != nil {
							failNoPath(t, mode+"/"+b.label, err)
						}
						if got := pick(t, mode, set).rc.ws.screenBudget(); got != b.d {
							t.Fatalf("%s/%s: budget = %s", mode, b.label, got)
						}
						return
					}
					if err == nil || err.Error() != b.err {
						t.Fatalf("%s/%s: error = %v, want %q", mode, b.label, err, b.err)
					}
				})
			}
		}
	}
	t.Run("nil workspace", func(t *testing.T) {
		if got := (*Workspace)(nil).screenBudget(); got != 5*time.Second {
			t.Fatalf("nil workspace: budget = %s", got)
		}
	})
	t.Run("zero StartCommand", func(t *testing.T) {
		if got := (&StartCommand{}).Effect().Timeout; got != bgToolTimeout+5*time.Second {
			t.Fatalf("zero StartCommand: timeout = %s", got)
		}
	})
	t.Run("nil StartCommand", func(t *testing.T) {
		if got := (*StartCommand)(nil).Effect().Timeout; got != bgToolTimeout+5*time.Second {
			t.Fatalf("nil StartCommand: timeout = %s", got)
		}
	})
}
