package main

import (
	"context"
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

func TestExposureLifecycle(t *testing.T) {
	t.Run("clearing grants keeps exposure", func(t *testing.T) {
		sess := newExposureSession(t)
		id := seedActive(t, sess)
		sess.grants.grant(grantScopeExec, "exec:abc")
		var out strings.Builder
		dispatchSlash(t.Context(), &out, sess, "/grants clear")
		if sess.grants.granted(grantScopeExec, "exec:abc") {
			t.Fatalf("/grants clear did not clear the grant: %q", out.String())
		}
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
	})

	t.Run("successful clear starts clean", func(t *testing.T) {
		sess := newExposureSession(t)
		id := seedActive(t, sess)
		var out strings.Builder
		dispatchSlash(t.Context(), &out, sess, "/clear")
		if !strings.Contains(out.String(), "session cleared") {
			t.Fatalf("/clear output = %q", out.String())
		}
		wantExposure(t, sess, id, agent.Exposure{Recorded: true})
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
		wantExposure(t, sess, newID, agent.Exposure{Recorded: true})
		wantExposure(t, sess, oldID, foreignExposure)
	})

	t.Run("resume restores the chosen record", func(t *testing.T) {
		sess := newExposureSession(t)
		seedExposed(t, sess, "user:other", foreignExposure)
		wantExposure(t, sess, sess.session.id, agent.Exposure{Recorded: true})
		var out strings.Builder
		dispatchSlash(t.Context(), &out, sess, "/resume user:other")
		if sess.session.id != "user:other" {
			t.Fatalf("resume failed: %q", out.String())
		}
		wantExposure(t, sess, sess.session.id, foreignExposure)
		// A real turn answers without a tool call; the record must survive it.
		in := strings.NewReader("follow up\n")
		if err := runREPL(t.Context(), newScannerSource(in, &out), &out, nil, sess); err != nil {
			t.Fatalf("runREPL: %v", err)
		}
		stored, err := sess.session.store.Load(context.Background(), "user:other")
		if err != nil || len(stored.Messages) != 4 {
			t.Fatalf("turn not stored: %v, %+v", err, stored)
		}
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
	})
}
