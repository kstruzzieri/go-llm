package interceptor_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agent/interceptor"
	"github.com/kstruzzieri/go-llm/provider"
)

func blockedBy(name string, verdict agent.Verdict) *agent.BlockedError {
	return &agent.BlockedError{Hook: agent.HookOutput, Findings: []agent.Finding{{Interceptor: name, Rule: "r", Verdict: verdict}}}
}

// vaultGuard is ConfiguredSecrets under a host's own name: only the
// pipeline's Sensitive mark, never a name check, can identify its block.
type vaultGuard struct{ interceptor.ConfiguredSecrets }

func (vaultGuard) Name() string { return "vault" }

// markedBlock is a real pipeline block by vaultGuard. Only the pipeline can
// set the mark, so the test cannot build one by struct literal.
func markedBlock(t *testing.T) *agent.BlockedError {
	t.Helper()
	fixture := configuredFixture(t)
	chain := []agent.Interceptor{vaultGuard{newTestConfiguredSecrets(t, fixture)}}
	_, _, _, _, err := runGuarded(t, chain, []provider.ToolCall{toolCall("call-1", "lookup", `{"q":"`+fixture+`"}`)})
	blocked := requireOutputBlock(t, err)
	if !blocked.Sensitive() || len(blocked.Findings) != 1 || blocked.Findings[0].Interceptor != "vault" {
		t.Fatalf("block = %+v, Sensitive %v; want one marked vault finding", blocked.Findings, blocked.Sensitive())
	}
	return blocked
}

func TestSensitiveBlockedIdentities(t *testing.T) {
	secrets := blockedBy("secrets", agent.VerdictBlock)
	configured := blockedBy("configured_secrets", agent.VerdictBlock)
	marked := markedBlock(t)
	unrelated := errors.New("unrelated")
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"secrets block", secrets, true},
		{"configured_secrets block", configured, true},
		{"secrets abort", blockedBy("secrets", agent.VerdictAbort), true},
		{"secrets joined under unrelated errors", errors.Join(unrelated, secrets, unrelated), true},
		{"configured_secrets joined under unrelated errors", errors.Join(unrelated, configured), true},
		{"secrets wrapped", fmt.Errorf("run: %w", secrets), true},
		{"configured_secrets wrapped", fmt.Errorf("run: %w", configured), true},
		{"secrets wrapped under a multi-wrap", fmt.Errorf("a: %w, b: %w", unrelated, secrets), true},
		{"configured_secrets joined then wrapped", fmt.Errorf("run: %w", errors.Join(unrelated, configured)), true},
		{"hidden behind a higher-verdict unrelated block", errors.Join(blockedBy("canary", agent.VerdictAbort), secrets), true},
		{"sensitive finding beside another in one block", &agent.BlockedError{Findings: []agent.Finding{
			{Interceptor: "canary", Verdict: agent.VerdictAbort}, {Interceptor: "configured_secrets", Verdict: agent.VerdictBlock},
		}}, true},
		{"marked block under an unrelated name", marked, true},
		{"marked block wrapped", fmt.Errorf("run: %w", marked), true},
		{"marked block joined under unrelated errors", errors.Join(unrelated, marked, unrelated), true},
		{"marked block joined then wrapped", fmt.Errorf("run: %w", errors.Join(unrelated, marked)), true},
		{"unmarked block under the same name", blockedBy("vault", agent.VerdictBlock), false},
		{"invariants block", blockedBy("invariants", agent.VerdictBlock), false},
		{"invariants block joined and wrapped", fmt.Errorf("run: %w", errors.Join(unrelated, blockedBy("invariants", agent.VerdictBlock))), false},
		{"canary abort", blockedBy("canary", agent.VerdictAbort), false},
		{"secrets tag below block", blockedBy("secrets", agent.VerdictTag), false},
		{"unrelated error", unrelated, false},
		{"typed nil BlockedError", (*agent.BlockedError)(nil), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := interceptor.SensitiveBlocked(tc.err); got != tc.want {
				t.Fatalf("SensitiveBlocked(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestBlockedHonorsNameAndMinimum(t *testing.T) {
	canary := blockedBy("canary", agent.VerdictAbort)
	tag := blockedBy("secrets", agent.VerdictTag)
	cases := []struct {
		name    string
		err     error
		ic      string
		minimum agent.Verdict
		want    bool
	}{
		{"abort meets abort", canary, "canary", agent.VerdictAbort, true},
		{"abort exceeds block", canary, "canary", agent.VerdictBlock, true},
		{"block below abort", blockedBy("canary", agent.VerdictBlock), "canary", agent.VerdictAbort, false},
		{"tag meets tag", tag, "secrets", agent.VerdictTag, true},
		{"tag below block", tag, "secrets", agent.VerdictBlock, false},
		{"other name", canary, "secrets", agent.VerdictBlock, false},
		{"joined", errors.Join(errors.New("x"), canary), "canary", agent.VerdictAbort, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := interceptor.Blocked(tc.err, tc.ic, tc.minimum); got != tc.want {
				t.Fatalf("Blocked(%v, %q, %v) = %v, want %v", tc.err, tc.ic, tc.minimum, got, tc.want)
			}
		})
	}
}
