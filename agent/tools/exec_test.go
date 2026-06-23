package tools

import (
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

func TestRunCommandSpec(t *testing.T) {
	rc := NewRunCommand(nil, nil)
	s := rc.Spec()
	if s.Name != "run_command" {
		t.Fatalf("name = %q, want run_command", s.Name)
	}
	for _, want := range []string{"argv", "dir", "timeout_seconds"} {
		if !strings.Contains(string(s.Parameters), want) {
			t.Errorf("schema missing %q: %s", want, s.Parameters)
		}
	}
	if strings.Contains(string(s.Parameters), "oneOf") {
		t.Error("schema must not use oneOf")
	}
}

func TestRunCommandEffect(t *testing.T) {
	e := NewRunCommand(nil, nil).Effect()
	for _, c := range []agent.EffectClass{agent.Read, agent.Write, agent.Exec, agent.Network} {
		if !e.Class.Has(c) {
			t.Errorf("effect class missing bit %v", c)
		}
	}
	if e.Approval != agent.ApprovalAlways {
		t.Errorf("approval = %v, want ApprovalAlways", e.Approval)
	}
	if e.OutputCap != execRuntimeCap {
		t.Errorf("OutputCap = %d, want %d", e.OutputCap, execRuntimeCap)
	}
}

func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{cap: 4}
	n, err := b.Write([]byte("abcdef"))
	if err != nil || n != 6 {
		t.Fatalf("Write = %d,%v; want 6,nil (must consume all to avoid pipe deadlock)", n, err)
	}
	if string(b.buf) != "abcd" {
		t.Errorf("buf = %q, want abcd", b.buf)
	}
	if !b.truncated {
		t.Error("truncated should be true")
	}
	n, _ = b.Write([]byte("g"))
	if n != 1 || string(b.buf) != "abcd" {
		t.Errorf("post-cap write: n=%d buf=%q", n, b.buf)
	}
}
