package tools

import (
	"strings"
	"testing"
	"time"

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

func TestResolveExecTimeout(t *testing.T) {
	p := func(n int) *int { return &n }
	cases := []struct {
		name      string
		in        *int
		wantEff   time.Duration
		wantReq   int
		wantClamp bool
		wantErr   bool
	}{
		{"default", nil, 60 * time.Second, 0, false, false},
		{"explicit", p(120), 120 * time.Second, 120, false, false},
		{"zero", p(0), 0, 0, false, true},
		{"negative", p(-5), 0, 0, false, true},
		{"max", p(600), 600 * time.Second, 600, false, false},
		{"clamp", p(900), 600 * time.Second, 900, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eff, req, clamp, err := resolveExecTimeout(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if err != nil {
				return
			}
			if eff != c.wantEff || req != c.wantReq || clamp != c.wantClamp {
				t.Errorf("got (%v,%d,%v) want (%v,%d,%v)", eff, req, clamp, c.wantEff, c.wantReq, c.wantClamp)
			}
		})
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
