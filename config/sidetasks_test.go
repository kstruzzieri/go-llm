package config

import (
	"reflect"
	"testing"
)

func TestUseCaseConstants(t *testing.T) {
	cases := []struct {
		got  string
		want string
	}{
		{UseCaseSummarize, "summarize"},
		{UseCaseRoute, "route"},
		{UseCaseRerank, "rerank"},
		{UseCaseVerify, "verify"},
		{UseCaseExtract, "extract"},
		{UseCaseApproval, "approval"},
		{UseCaseVision, "vision"},
		{UseCasePlanning, "planning"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("constant = %q, want %q", c.got, c.want)
		}
	}
}

func TestSideTaskUseCases_SortedAndMatchesFallbackKeys(t *testing.T) {
	got := SideTaskUseCases()
	want := []string{"approval", "extract", "planning", "rerank", "route", "summarize", "verify", "vision"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SideTaskUseCases() = %v, want %v (sorted)", got, want)
	}
	if len(got) != len(sideTaskUseCaseFallbacks) {
		t.Fatalf("SideTaskUseCases() has %d entries, fallback table has %d", len(got), len(sideTaskUseCaseFallbacks))
	}
	for _, uc := range got {
		if _, ok := sideTaskUseCaseFallbacks[uc]; !ok {
			t.Errorf("SideTaskUseCases() returned %q absent from fallback table", uc)
		}
	}
}

func TestSideTaskUseCaseFallbacks_ValuesAndOrder(t *testing.T) {
	want := map[string][]string{
		"summarize": {"analysis", "chat"},
		"route":     {"analysis", "chat"},
		"rerank":    {"analysis", "chat"},
		"verify":    {"analysis", "chat"},
		"extract":   {"analysis", "chat"},
		"approval":  {"agent", "chat"},
		"vision":    {"chat"},
		"planning":  {"reasoning", "analysis", "agent"},
	}
	if !reflect.DeepEqual(sideTaskUseCaseFallbacks, want) {
		t.Fatalf("sideTaskUseCaseFallbacks = %v, want %v", sideTaskUseCaseFallbacks, want)
	}
}

func TestRoleForUseCase(t *testing.T) {
	cfg := &Config{Defaults: map[string]string{
		"chat":     "general",
		"analysis": "general",
		"agent":    "agentRole",
	}}
	tests := []struct {
		useCase  string
		wantRole string
		wantOK   bool
	}{
		{"chat", "general", true},            // direct hit
		{UseCaseSummarize, "general", true},  // fallback -> analysis
		{UseCaseApproval, "agentRole", true}, // fallback -> agent
		{"nope", "", false},                  // unknown, no fallback
	}
	for _, tt := range tests {
		t.Run(tt.useCase, func(t *testing.T) {
			role, ok := cfg.RoleForUseCase(tt.useCase)
			if role != tt.wantRole || ok != tt.wantOK {
				t.Errorf("RoleForUseCase(%q) = (%q,%v), want (%q,%v)", tt.useCase, role, ok, tt.wantRole, tt.wantOK)
			}
		})
	}
}

func TestRoleForUseCase_ExplicitWins(t *testing.T) {
	cfg := &Config{Defaults: map[string]string{
		"analysis":  "general",
		"summarize": "light",
	}}
	if role, ok := cfg.RoleForUseCase(UseCaseSummarize); role != "light" || !ok {
		t.Fatalf("RoleForUseCase(summarize) = (%q,%v), want explicit (light,true)", role, ok)
	}
}

func TestRoleForUseCase_WalksToSecondFallback(t *testing.T) {
	// summarize -> {analysis, chat}; with analysis absent it must walk past the
	// first fallback entry to the second one ("chat").
	cfg := &Config{Defaults: map[string]string{"chat": "general"}}
	if role, ok := cfg.RoleForUseCase(UseCaseSummarize); role != "general" || !ok {
		t.Fatalf("RoleForUseCase(summarize) = (%q,%v), want chat fallback (general,true)", role, ok)
	}
}

func TestUseCasePlanning_FallbackOrder(t *testing.T) {
	// Exact order pin (#476 D2). Planning degrades to a strong reasoner first,
	// then the general analysis route, and finally the agent route a runnable
	// config always has. Reordering these is a behavior change, not a
	// refactor: it decides which authored role -- and so which provider --
	// authors plans on a config that never mentions planning.
	want := []string{"reasoning", "analysis", "agent"}
	if !reflect.DeepEqual(sideTaskUseCaseFallbacks[UseCasePlanning], want) {
		t.Fatalf("planning fallbacks = %v, want %v",
			sideTaskUseCaseFallbacks[UseCasePlanning], want)
	}
}

func TestRoleForUseCase_PlanningHops(t *testing.T) {
	tests := []struct {
		name     string
		defaults map[string]string
		wantRole string
		wantOK   bool
	}{
		{"explicit planning beats every fallback",
			map[string]string{"planning": "p", "reasoning": "r", "analysis": "a", "agent": "g"}, "p", true},
		{"reasoning is preferred over analysis",
			map[string]string{"reasoning": "r", "analysis": "a", "agent": "g"}, "r", true},
		{"analysis is preferred over agent",
			map[string]string{"analysis": "a", "agent": "g"}, "a", true},
		{"agent is the last resort",
			map[string]string{"agent": "g"}, "g", true},
		{"chat alone does not route planning",
			map[string]string{"chat": "c"}, "", false},
		{"no defaults at all",
			map[string]string{}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Defaults: tt.defaults}
			role, ok := cfg.RoleForUseCase(UseCasePlanning)
			if role != tt.wantRole || ok != tt.wantOK {
				t.Errorf("RoleForUseCase(planning) = (%q,%v), want (%q,%v)",
					role, ok, tt.wantRole, tt.wantOK)
			}
		})
	}
}
