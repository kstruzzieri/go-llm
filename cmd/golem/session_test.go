package main

import (
	"strings"
	"testing"
)

func TestResolveSessionID(t *testing.T) {
	tests := []struct {
		name    string
		opts    sessionIDOpts
		want    string // exact, or prefix when hasPrefix=true
		prefix  bool
		wantErr bool
	}{
		{name: "workspace default", opts: sessionIDOpts{root: "/abs/x"}, want: "workspace:", prefix: true},
		{name: "explicit valid", opts: sessionIDOpts{explicit: "my.chat-1"}, want: "user:my.chat-1"},
		{name: "explicit trims", opts: sessionIDOpts{explicit: "  ok  "}, want: "user:ok"},
		{name: "explicit blank", opts: sessionIDOpts{explicit: "   "}, wantErr: true},
		{name: "explicit illegal char", opts: sessionIDOpts{explicit: "bad id!"}, wantErr: true},
		{name: "fresh", opts: sessionIDOpts{fresh: true}, want: "golem:", prefix: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveSessionID(tc.opts)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got id %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveSessionID: %v", err)
			}
			if tc.prefix {
				if !strings.HasPrefix(got, tc.want) {
					t.Errorf("id = %q, want prefix %q", got, tc.want)
				}
			} else if got != tc.want {
				t.Errorf("id = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveSessionID_WorkspaceDeterministic(t *testing.T) {
	a, _ := resolveSessionID(sessionIDOpts{root: "/abs/x"})
	b, _ := resolveSessionID(sessionIDOpts{root: "/abs/x"})
	c, _ := resolveSessionID(sessionIDOpts{root: "/abs/y"})
	if a != b {
		t.Errorf("same root must give same id: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("different roots must give different ids: both %q", a)
	}
}

func TestResolveSessionID_FreshUnique(t *testing.T) {
	a, _ := resolveSessionID(sessionIDOpts{fresh: true})
	b, _ := resolveSessionID(sessionIDOpts{fresh: true})
	if a == b {
		t.Errorf("fresh ids must differ: both %q", a)
	}
}

func TestSessionDBPath(t *testing.T) {
	xdg := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return "/data"
		}
		return ""
	}
	got, err := sessionDBPath(xdg)
	if err != nil || got != "/data/golem/sessions.db" {
		t.Fatalf("xdg path = %q err=%v", got, err)
	}

	homeOnly := func(k string) string {
		if k == "HOME" {
			return "/home/u"
		}
		return ""
	}
	got, err = sessionDBPath(homeOnly)
	if err != nil || got != "/home/u/.local/share/golem/sessions.db" {
		t.Fatalf("home path = %q err=%v", got, err)
	}

	if _, err := sessionDBPath(func(string) string { return "" }); err == nil {
		t.Fatal("want error when HOME and XDG_DATA_HOME are both unset")
	}
}
