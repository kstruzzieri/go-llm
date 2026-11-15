package main

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/conversation"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
)

var foreignExposure = agent.Exposure{Recorded: true, Foreign: true, First: "tool:mcp__docs__fetch"}

// newExposureSession builds a sessioned REPL whose runtime shares the CLI
// session's store, so Runtime.ThreadExposure sees what the slash commands do.
func newExposureSession(t *testing.T) *replSession {
	t.Helper()
	root := t.TempDir()
	sess := newSessionedTestSession(t, &captureCaller{answer: "ok"}, root, "user:active")
	sess.root = root
	sess.grants = newApprovalGrants() // the helper leaves it nil, which would make grant a no-op
	installCompactRuntime(t, sess, golemruntime.Options{Profile: golemruntime.ProfileTrusted})
	return sess
}

// seedExposed stores a conversation with messages and the foreign record.
func seedExposed(t *testing.T, sess *replSession, id string, exposure agent.Exposure) {
	t.Helper()
	if _, err := sess.session.store.Save(t.Context(), conversation.Conversation{
		ID:       id,
		Title:    "seeded",
		Messages: []conversation.Message{{Role: "user", Content: "q"}, {Role: "assistant", Content: "a"}},
		Exposure: exposure,
	}); err != nil {
		t.Fatal(err)
	}
}

func wantExposure(t *testing.T, sess *replSession, id string, want agent.Exposure) {
	t.Helper()
	got, err := sess.runtime.ThreadExposure(t.Context(), id)
	if err != nil {
		t.Fatalf("ThreadExposure(%q): %v", id, err)
	}
	if got != want {
		t.Fatalf("ThreadExposure(%q) = %+v, want %+v", id, got, want)
	}
}

// seedActive stores the foreign record under the active ID and reloads the
// CLI's cache from it.
func seedActive(t *testing.T, sess *replSession) string {
	t.Helper()
	id := sess.session.id
	seedExposed(t, sess, id, foreignExposure)
	if _, err := sess.session.switchTo(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	wantExposure(t, sess, id, foreignExposure)
	return id
}

// runTurn runs one real turn, answered without a tool call, on whichever
// thread the session has active.
func runTurn(t *testing.T, sess *replSession) {
	t.Helper()
	var out strings.Builder
	in := strings.NewReader("follow up\n")
	if err := runREPL(t.Context(), newScannerSource(in, &out), &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
}

// wantMessages asserts how many messages the stored conversation holds.
func wantMessages(t *testing.T, sess *replSession, id string, want int) {
	t.Helper()
	stored, err := sess.session.store.Load(t.Context(), id)
	if err != nil {
		t.Fatalf("Load(%q): %v", id, err)
	}
	if len(stored.Messages) != want {
		t.Fatalf("%q holds %d messages, want %d", id, len(stored.Messages), want)
	}
}

func TestExposureLifecycle(t *testing.T) {
	clean := agent.Exposure{Recorded: true}

	t.Run("clearing grants keeps exposure", func(t *testing.T) {
		sess := newExposureSession(t)
		id := seedActive(t, sess)
		sess.grants.grant(grantScopeExec, "exec:abc")
		if !sess.grants.granted(grantScopeExec, "exec:abc") {
			t.Fatal("grant was not held before the command")
		}
		var out strings.Builder
		dispatchSlash(t.Context(), &out, sess, "/grants clear")
		if sess.grants.granted(grantScopeExec, "exec:abc") {
			t.Fatalf("/grants clear did not clear the grant: %q", out.String())
		}
		wantExposure(t, sess, id, foreignExposure)
		runTurn(t, sess)
		wantMessages(t, sess, id, 4)
		wantExposure(t, sess, id, foreignExposure)
	})

	t.Run("failed clear keeps exposure", func(t *testing.T) {
		sess := newExposureSession(t)
		id := seedActive(t, sess)
		sess.session.store = canaryDeleteFailureStore{sess.session.store}
		var out strings.Builder
		dispatchSlash(t.Context(), &out, sess, "/clear")
		if out.String() != "clear failed: delete failed\n" {
			t.Fatalf("/clear output = %q, want the delete failure", out.String())
		}
		wantExposure(t, sess, id, foreignExposure)
		runTurn(t, sess)
		if sess.session.id != id {
			t.Fatalf("active id = %q, want %q", sess.session.id, id)
		}
		wantMessages(t, sess, id, 4)
		wantExposure(t, sess, id, foreignExposure)
	})

	t.Run("successful clear starts clean", func(t *testing.T) {
		sess := newExposureSession(t)
		id := seedActive(t, sess)
		var out strings.Builder
		dispatchSlash(t.Context(), &out, sess, "/clear")
		if !strings.Contains(out.String(), "session cleared") {
			t.Fatalf("/clear output = %q", out.String())
		}
		wantExposure(t, sess, id, clean)
		runTurn(t, sess)
		wantMessages(t, sess, id, 2)
		wantExposure(t, sess, id, clean)
	})

	t.Run("new starts clean and leaves the old conversation alone", func(t *testing.T) {
		sess := newExposureSession(t)
		oldID := seedActive(t, sess)
		var out strings.Builder
		dispatchSlash(t.Context(), &out, sess, "/new")
		newID := sess.session.id
		if newID == oldID {
			t.Fatalf("/new kept the id: %q", out.String())
		}
		wantExposure(t, sess, newID, clean)
		wantExposure(t, sess, oldID, foreignExposure)
		runTurn(t, sess)
		if sess.session.id != newID {
			t.Fatalf("active id = %q, want %q", sess.session.id, newID)
		}
		wantMessages(t, sess, newID, 2)
		wantExposure(t, sess, newID, clean)
		wantMessages(t, sess, oldID, 2)
		wantExposure(t, sess, oldID, foreignExposure)
	})

	t.Run("resume restores the chosen record", func(t *testing.T) {
		sess := newExposureSession(t)
		seedExposed(t, sess, "user:other", foreignExposure)
		wantExposure(t, sess, sess.session.id, clean)
		var out strings.Builder
		dispatchSlash(t.Context(), &out, sess, "/resume user:other")
		if sess.session.id != "user:other" {
			t.Fatalf("resume failed: %q", out.String())
		}
		wantExposure(t, sess, sess.session.id, foreignExposure)
		runTurn(t, sess)
		wantMessages(t, sess, "user:other", 4)
		wantExposure(t, sess, "user:other", foreignExposure)
	})

	t.Run("failed resume keeps the active record", func(t *testing.T) {
		sess := newExposureSession(t)
		id := seedActive(t, sess)
		var out strings.Builder
		dispatchSlash(t.Context(), &out, sess, "/resume does-not-exist")
		if !strings.Contains(out.String(), "resume failed") || sess.session.id != id {
			t.Fatalf("failed resume changed the session (%q): %q", sess.session.id, out.String())
		}
		wantExposure(t, sess, id, foreignExposure)
		runTurn(t, sess)
		if sess.session.id != id {
			t.Fatalf("active id = %q, want %q", sess.session.id, id)
		}
		wantMessages(t, sess, id, 4)
		wantExposure(t, sess, id, foreignExposure)
		if _, err := sess.session.store.Load(t.Context(), "user:does-not-exist"); !errors.Is(err, conversation.ErrNotFound) {
			t.Fatalf("the failed resume's target has a stored row: %v", err)
		}
	})

	t.Run("resuming a conversation with no record reads as exposed", func(t *testing.T) {
		sess := newExposureSession(t)
		seedExposed(t, sess, "user:legacy", agent.Exposure{})
		var out strings.Builder
		dispatchSlash(t.Context(), &out, sess, "/resume user:legacy")
		if sess.session.id != "user:legacy" {
			t.Fatalf("resume failed: %q", out.String())
		}
		got, err := sess.runtime.ThreadExposure(t.Context(), "user:legacy")
		if err != nil {
			t.Fatal(err)
		}
		if got != (agent.Exposure{}) || !got.Exposed() {
			t.Fatalf("ThreadExposure = %+v (Exposed=%v), want the zero record reading as exposed", got, got.Exposed())
		}
		runTurn(t, sess)
		wantMessages(t, sess, "user:legacy", 4)
		wantExposure(t, sess, "user:legacy", agent.Exposure{Recorded: true, Unknown: true, First: "history"})
	})

	// #584: under -strict-approval, /grants shows the record each step above
	// leaves. These prove the display only; strict_test.go's
	// TestStrictGateEffect proves the gate acts on it.
	t.Run("strict grants line follows the record", func(t *testing.T) {
		const foreign, unknown, cleanLine = "exposure: foreign (mcp__docs__fetch)", "exposure: unknown (history)", "exposure: clean"
		for _, tc := range []struct {
			name  string
			steps func(t *testing.T, sess *replSession)
			want  string
		}{
			{"grants clear keeps it", func(t *testing.T, sess *replSession) {
				seedActive(t, sess)
				dispatchSlash(t.Context(), io.Discard, sess, "/grants clear")
			}, foreign},
			{"failed clear keeps it", func(t *testing.T, sess *replSession) {
				seedActive(t, sess)
				sess.session.store = canaryDeleteFailureStore{sess.session.store}
				dispatchSlash(t.Context(), io.Discard, sess, "/clear")
			}, foreign},
			{"successful clear is clean", func(t *testing.T, sess *replSession) {
				seedActive(t, sess)
				dispatchSlash(t.Context(), io.Discard, sess, "/clear")
			}, cleanLine},
			{"new is clean", func(t *testing.T, sess *replSession) {
				seedActive(t, sess)
				dispatchSlash(t.Context(), io.Discard, sess, "/new")
			}, cleanLine},
			{"resuming the first conversation restores it", func(t *testing.T, sess *replSession) {
				id := seedActive(t, sess)
				dispatchSlash(t.Context(), io.Discard, sess, "/new")
				dispatchSlash(t.Context(), io.Discard, sess, "/resume "+id)
			}, foreign},
			{"failed resume keeps it", func(t *testing.T, sess *replSession) {
				seedActive(t, sess)
				dispatchSlash(t.Context(), io.Discard, sess, "/resume does-not-exist")
			}, foreign},
			{"resuming a conversation with no record is unknown", func(t *testing.T, sess *replSession) {
				seedExposed(t, sess, "user:legacy", agent.Exposure{})
				dispatchSlash(t.Context(), io.Discard, sess, "/resume user:legacy")
			}, unknown},
		} {
			t.Run(tc.name, func(t *testing.T) {
				sess := newExposureSession(t)
				sess.strictApproval = true
				tc.steps(t, sess)
				var out strings.Builder
				dispatchSlash(t.Context(), &out, sess, "/grants")
				if want := "session grants: 0\n" + tc.want + "\n"; out.String() != want {
					t.Fatalf("/grants = %q, want %q", out.String(), want)
				}
			})
		}
	})
}
