package provider

import (
	"reflect"
	"strings"
	"testing"
)

// TestQwen3CoderNextVariantsInSync guards against drift between the
// "latest" alias and the canonical "80b" variant. If 80b's profile is
// retuned, latest must track — or this test must be updated deliberately.
func TestQwen3CoderNextVariantsInSync(t *testing.T) {
	cat, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog() error: %v", err)
	}

	fam, ok := cat.Families["qwen3-coder-next"]
	if !ok {
		t.Fatal("catalog missing qwen3-coder-next family")
	}

	latest, hasLatest := fam.Variants["latest"]
	canonical, hasCanonical := fam.Variants["80b"]
	if !hasLatest || !hasCanonical {
		t.Fatalf("qwen3-coder-next variants: latest=%v 80b=%v", hasLatest, hasCanonical)
	}
	if !reflect.DeepEqual(latest, canonical) {
		t.Errorf("qwen3-coder-next latest != 80b — variants drifted\nlatest: %+v\n80b:    %+v", latest, canonical)
	}
}

func TestCatalogLoad(t *testing.T) {
	cat, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog() error: %v", err)
	}
	if len(cat.Families) == 0 {
		t.Fatal("loadCatalog() returned empty catalog")
	}

	// Spot-check that all required families are present.
	families := []string{
		"qwen2.5", "qwen3", "qwen3.5", "qwen3.6", "qwen3-coder-next",
		"deepseek-r1", "deepseek-coder-v2",
		"codellama", "starcoder", "starcoder2",
		"llama3", "llama3.1", "llama3.2", "llama3.3",
		"gemma2", "gemma3", "gemma4",
		"phi3", "phi4",
		"mistral", "mixtral",
		"nomic-embed-text", "mxbai-embed-large",
	}
	for _, f := range families {
		if _, ok := cat.Families[f]; !ok {
			t.Errorf("catalog missing family %q", f)
		}
	}
}

func TestCatalogLookupExact(t *testing.T) {
	cat, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog() error: %v", err)
	}

	sc := newStaticCatalog(cat)

	tests := []struct {
		name       string
		family     string
		paramSize  string
		wantFIM    bool
		wantThink  ThinkMode
		wantRAMMin float64
	}{
		{
			name:       "qwen3 8b has FIM and toggle think",
			family:     "qwen3",
			paramSize:  "8b",
			wantFIM:    true,
			wantThink:  ThinkToggle,
			wantRAMMin: 5.0,
		},
		{
			name:       "deepseek-r1 7b has no FIM but always thinks",
			family:     "deepseek-r1",
			paramSize:  "7b",
			wantFIM:    false,
			wantThink:  ThinkAlways,
			wantRAMMin: 5.0,
		},
		{
			name:       "codellama 7b has FIM with PRE/SUF/MID tokens",
			family:     "codellama",
			paramSize:  "7b",
			wantFIM:    true,
			wantThink:  ThinkNone,
			wantRAMMin: 5.0,
		},
		{
			name:       "starcoder2 15b has FIM",
			family:     "starcoder2",
			paramSize:  "15b",
			wantFIM:    true,
			wantThink:  ThinkNone,
			wantRAMMin: 10.0,
		},
		{
			name:       "llama3 8b has no FIM and no think",
			family:     "llama3",
			paramSize:  "8b",
			wantFIM:    false,
			wantThink:  ThinkNone,
			wantRAMMin: 5.0,
		},
		{
			name:       "gemma2 27b has no FIM and no think",
			family:     "gemma2",
			paramSize:  "27b",
			wantFIM:    false,
			wantThink:  ThinkNone,
			wantRAMMin: 17.0,
		},
		{
			name:       "phi3 3.8b has no FIM and no think",
			family:     "phi3",
			paramSize:  "3.8b",
			wantFIM:    false,
			wantThink:  ThinkNone,
			wantRAMMin: 3.0,
		},
		{
			name:       "mistral 7b has FIM with bracket tokens",
			family:     "mistral",
			paramSize:  "7b",
			wantFIM:    true,
			wantThink:  ThinkNone,
			wantRAMMin: 5.0,
		},
		{
			name:       "qwen2.5 72b is best quality",
			family:     "qwen2.5",
			paramSize:  "72b",
			wantFIM:    true,
			wantThink:  ThinkNone,
			wantRAMMin: 44.0,
		},
		{
			name:       "deepseek-coder-v2 16b has FIM and no think",
			family:     "deepseek-coder-v2",
			paramSize:  "16b",
			wantFIM:    true,
			wantThink:  ThinkNone,
			wantRAMMin: 10.0,
		},
		{
			name:       "qwen3-coder-next latest retains scoring data",
			family:     "qwen3-coder-next",
			paramSize:  "latest",
			wantFIM:    true,
			wantThink:  ThinkToggle,
			wantRAMMin: 46.0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile := sc.lookup(tt.family, tt.paramSize)
			if profile == nil {
				t.Fatalf("lookup(%q, %q) returned nil", tt.family, tt.paramSize)
			}
			if (profile.FIM != nil) != tt.wantFIM {
				t.Errorf("FIM: got %v, want hasFIM=%v", profile.FIM, tt.wantFIM)
			}
			if profile.ThinkMode != tt.wantThink {
				t.Errorf("ThinkMode = %v, want %v", profile.ThinkMode, tt.wantThink)
			}
			if profile.Resources.RAMRequired < tt.wantRAMMin {
				t.Errorf("RAMRequired = %f, want >= %f", profile.Resources.RAMRequired, tt.wantRAMMin)
			}
		})
	}
}

// TestCatalogQwen3EmbeddingResolves guards the qwen3-embedding family added so
// the default local embedding model gets a non-zero context window. Without it,
// catalog lookup misses, ContextWindow stays 0, and the router rejects every
// embed request with ErrBudgetExceeded (Validate: "no context window
// configured"), breaking RAG auto-index on the openai-compat backend (which
// reports no context via /v1/models).
func TestCatalogQwen3EmbeddingResolves(t *testing.T) {
	cat, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog() error: %v", err)
	}
	sc := newStaticCatalog(cat)

	profile := sc.lookup("qwen3-embedding", "8b")
	if profile == nil {
		t.Fatal("lookup(qwen3-embedding, 8b) returned nil; RAG embed budget will reject")
	}
	if profile.ContextWindow <= 0 {
		t.Errorf("ContextWindow = %d, want > 0 (0 triggers router budget reject)", profile.ContextWindow)
	}
	if !profile.Caps.Has(CapEmbed) {
		t.Errorf("Caps = %v, want CapEmbed", profile.Caps)
	}
}

func TestCatalogLookupFamilyOnly(t *testing.T) {
	cat, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog() error: %v", err)
	}

	sc := newStaticCatalog(cat)

	// lookupFamily returns family-level info without variant resource data.
	profile := sc.lookupFamily("qwen3")
	if profile == nil {
		t.Fatal("lookupFamily(qwen3) returned nil")
	}
	if profile.FIM == nil {
		t.Error("expected FIM config for qwen3 family")
	}
	if profile.ThinkMode != ThinkToggle {
		t.Errorf("ThinkMode = %v, want ThinkToggle", profile.ThinkMode)
	}
	if profile.ContextWindow != 40960 {
		t.Errorf("ContextWindow = %d, want 40960", profile.ContextWindow)
	}
	// Family-only lookup should have zero resource profile.
	if profile.Resources.RAMRequired != 0 {
		t.Errorf("RAMRequired = %f, want 0 for family-only lookup", profile.Resources.RAMRequired)
	}
}

func TestCatalogLookupMiss(t *testing.T) {
	cat, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog() error: %v", err)
	}

	sc := newStaticCatalog(cat)

	// Unknown family returns nil.
	profile := sc.lookup("nonexistent-model", "8b")
	if profile != nil {
		t.Errorf("lookup(nonexistent) should return nil, got %+v", profile)
	}

	profile = sc.lookupFamily("nonexistent-model")
	if profile != nil {
		t.Errorf("lookupFamily(nonexistent) should return nil, got %+v", profile)
	}

	// Known family but unknown variant returns nil.
	profile = sc.lookup("qwen3", "999b")
	if profile != nil {
		t.Errorf("lookup(qwen3, 999b) should return nil, got %+v", profile)
	}
}

func TestCatalogFIMPolicy(t *testing.T) {
	cat, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog() error: %v", err)
	}

	sc := newStaticCatalog(cat)

	tests := []struct {
		family     string
		wantStops  []string
		wantBudget int
	}{
		{"qwen3", []string{"<|endoftext|>", "<|fim_pad|>"}, 75},
		{"qwen2.5", []string{"<|endoftext|>", "<|fim_pad|>"}, 75},
		{"qwen3.5", []string{"<|endoftext|>", "<|fim_pad|>"}, 75},
		{"deepseek-coder-v2", []string{"<|EOT|>", "<|end_of_sentence|>"}, 70},
		{"codellama", []string{"<EOT>", "</s>"}, 65},
		{"starcoder", []string{"<|endoftext|>"}, 70},
		{"starcoder2", []string{"<|endoftext|>"}, 70},
		{"mistral", []string{"[/MIDDLE]", "</s>"}, 70},
	}
	for _, tt := range tests {
		t.Run(tt.family, func(t *testing.T) {
			profile := sc.lookupFamily(tt.family)
			if profile == nil {
				t.Fatalf("lookupFamily(%q) returned nil", tt.family)
			}
			if profile.FIM == nil {
				t.Fatalf("FIM config is nil for %q", tt.family)
			}
			if len(profile.FIM.StopTokens) != len(tt.wantStops) {
				t.Fatalf("StopTokens len = %d, want %d", len(profile.FIM.StopTokens), len(tt.wantStops))
			}
			for i, want := range tt.wantStops {
				if profile.FIM.StopTokens[i] != want {
					t.Errorf("StopTokens[%d] = %q, want %q", i, profile.FIM.StopTokens[i], want)
				}
			}
			if profile.FIM.PrefixBudgetPct != tt.wantBudget {
				t.Errorf("PrefixBudgetPct = %d, want %d", profile.FIM.PrefixBudgetPct, tt.wantBudget)
			}
		})
	}
}

func TestCatalogEmbeddingDimensions(t *testing.T) {
	cat, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog() error: %v", err)
	}

	sc := newStaticCatalog(cat)

	tests := []struct {
		family string
		want   int
	}{
		{"nomic-embed-text", 768},
		{"mxbai-embed-large", 1024},
		{"qwen3", 0},
		{"llama3", 0},
	}
	for _, tt := range tests {
		t.Run(tt.family, func(t *testing.T) {
			profile := sc.lookupFamily(tt.family)
			if profile == nil {
				t.Fatalf("lookupFamily(%q) returned nil", tt.family)
			}
			if profile.Dimensions != tt.want {
				t.Errorf("Dimensions = %d, want %d", profile.Dimensions, tt.want)
			}
		})
	}
}

func TestCatalogToModelProfile(t *testing.T) {
	cat, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog() error: %v", err)
	}

	sc := newStaticCatalog(cat)

	t.Run("qwen3 8b full profile", func(t *testing.T) {
		p := sc.lookup("qwen3", "8b")
		if p == nil {
			t.Fatal("lookup returned nil")
		}
		if p.Family != "qwen3" {
			t.Errorf("Family = %q, want %q", p.Family, "qwen3")
		}
		if p.Source != SourceStatic {
			t.Errorf("Source = %v, want SourceStatic", p.Source)
		}
		if p.ThinkMode != ThinkToggle {
			t.Errorf("ThinkMode = %v, want ThinkToggle", p.ThinkMode)
		}
		if p.ThinkTags == nil {
			t.Fatal("ThinkTags is nil")
		}
		if p.ThinkTags.Open != "<think>" || p.ThinkTags.Close != "</think>" {
			t.Errorf("ThinkTags = %+v, want <think>/<think>", p.ThinkTags)
		}
		if p.FIM == nil {
			t.Fatal("FIM is nil")
		}
		if len(p.FIM.StopTokens) != 2 {
			t.Errorf("StopTokens len = %d, want 2", len(p.FIM.StopTokens))
		}
		if p.Resources.RAMRequired != 5.0 {
			t.Errorf("RAMRequired = %f, want 5.0", p.Resources.RAMRequired)
		}
		if p.Resources.RAMRecommended != 8.0 {
			t.Errorf("RAMRecommended = %f, want 8.0", p.Resources.RAMRecommended)
		}
		if p.Quality != TierGood {
			t.Errorf("Quality = %v, want TierGood", p.Quality)
		}
		// Speed "fast" maps to TierGreat.
		if p.Speed != TierGreat {
			t.Errorf("Speed = %v, want TierGreat", p.Speed)
		}
		if p.ContextWindow != 40960 {
			t.Errorf("ContextWindow = %d, want 40960", p.ContextWindow)
		}
		// Capabilities: "completion" + "tools" -> chat|generate|stream|tool_call.
		wantCaps := CapChat | CapGenerate | CapStream | CapToolCall
		if p.Caps != wantCaps {
			t.Errorf("Caps = %v, want %v", p.Caps, wantCaps)
		}
	})

	t.Run("nomic-embed-text embedding profile", func(t *testing.T) {
		p := sc.lookup("nomic-embed-text", "latest")
		if p == nil {
			t.Fatal("lookup returned nil")
		}
		if p.Dimensions != 768 {
			t.Errorf("Dimensions = %d, want 768", p.Dimensions)
		}
		if !p.Caps.Has(CapEmbed) {
			t.Error("expected CapEmbed capability")
		}
		if p.Caps.Has(CapChat) {
			t.Error("embedding model should not have CapChat")
		}
		if p.FIM != nil {
			t.Errorf("embedding model should have nil FIM, got %+v", p.FIM)
		}
		if p.Resources.RAMRequired != 0.5 {
			t.Errorf("RAMRequired = %f, want 0.5", p.Resources.RAMRequired)
		}
	})

	t.Run("deepseek-r1 always think", func(t *testing.T) {
		p := sc.lookup("deepseek-r1", "32b")
		if p == nil {
			t.Fatal("lookup returned nil")
		}
		if p.ThinkMode != ThinkAlways {
			t.Errorf("ThinkMode = %v, want ThinkAlways", p.ThinkMode)
		}
		if p.FIM != nil {
			t.Errorf("deepseek-r1 should have nil FIM, got %+v", p.FIM)
		}
		if p.Resources.RAMRequired != 20.0 {
			t.Errorf("RAMRequired = %f, want 20.0", p.Resources.RAMRequired)
		}
	})

	t.Run("mixtral MoE variant", func(t *testing.T) {
		p := sc.lookup("mixtral", "8x7b")
		if p == nil {
			t.Fatal("lookup returned nil")
		}
		if p.Resources.RAMRequired != 28.0 {
			t.Errorf("RAMRequired = %f, want 28.0", p.Resources.RAMRequired)
		}
		if p.Quality != TierGreat {
			t.Errorf("Quality = %v, want TierGreat", p.Quality)
		}
	})
}

func TestCatalogFamilies(t *testing.T) {
	cat, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog() error: %v", err)
	}

	sc := newStaticCatalog(cat)
	names := sc.families()

	if len(names) < 20 {
		t.Errorf("families() returned %d families, want at least 20", len(names))
	}

	// Ensure the list contains expected entries.
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}
	for _, want := range []string{"qwen3", "deepseek-r1", "codellama", "nomic-embed-text"} {
		if !nameSet[want] {
			t.Errorf("families() missing %q", want)
		}
	}
}

func TestCatalogCaseInsensitive(t *testing.T) {
	cat, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog() error: %v", err)
	}

	sc := newStaticCatalog(cat)

	// lookup normalizes to lowercase, so mixed-case input should still match.
	p := sc.lookup("Qwen3", "8B")
	if p == nil {
		t.Fatal("lookup(Qwen3, 8B) returned nil -- case insensitivity failed")
	}
	if p.Family != "qwen3" {
		t.Errorf("Family = %q, want %q", p.Family, "qwen3")
	}

	p = sc.lookupFamily("DEEPSEEK-R1")
	if p == nil {
		t.Fatal("lookupFamily(DEEPSEEK-R1) returned nil -- case insensitivity failed")
	}
}

func TestParseThinkMode(t *testing.T) {
	tests := []struct {
		input string
		want  ThinkMode
	}{
		{"none", ThinkNone},
		{"always", ThinkAlways},
		{"toggle", ThinkToggle},
		{"auto", ThinkAuto},
		{"None", ThinkNone},
		{"ALWAYS", ThinkAlways},
		{"", ThinkNone},
		{"unknown", ThinkNone},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseThinkMode(tt.input)
			if got != tt.want {
				t.Errorf("parseThinkMode(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseTier(t *testing.T) {
	tests := []struct {
		input string
		want  Tier
	}{
		{"basic", TierBasic},
		{"good", TierGood},
		{"great", TierGreat},
		{"best", TierBest},
		{"fast", TierGreat},
		{"medium", TierGood},
		{"slow", TierBasic},
		{"", TierBasic},
		{"unknown", TierBasic},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseTier(tt.input)
			if got != tt.want {
				t.Errorf("parseTier(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseCaps(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  Capability
	}{
		{
			name:  "completion gives chat+generate+stream",
			input: []string{"completion"},
			want:  CapChat | CapGenerate | CapStream,
		},
		{
			name:  "tools gives tool_call",
			input: []string{"tools"},
			want:  CapToolCall,
		},
		{
			name:  "embedding gives embed",
			input: []string{"embedding"},
			want:  CapEmbed,
		},
		{
			name:  "insert gives generate+stream+insert",
			input: []string{"insert"},
			want:  CapGenerate | CapInsert | CapStream,
		},
		{
			name:  "completion+tools combined",
			input: []string{"completion", "tools"},
			want:  CapChat | CapGenerate | CapStream | CapToolCall,
		},
		{
			name:  "empty input gives none",
			input: nil,
			want:  0,
		},
		{
			name:  "unknown cap ignored",
			input: []string{"completion", "nonexistent"},
			want:  CapChat | CapGenerate | CapStream,
		},
		{
			name:  "canonical chat token",
			input: []string{"chat"},
			want:  CapChat,
		},
		{
			name:  "canonical generate token",
			input: []string{"generate"},
			want:  CapGenerate,
		},
		{
			name:  "canonical stream token",
			input: []string{"stream"},
			want:  CapStream,
		},
		{
			name:  "canonical embed token",
			input: []string{"embed"},
			want:  CapEmbed,
		},
		{
			name:  "canonical tool_call token",
			input: []string{"tool_call"},
			want:  CapToolCall,
		},
		{
			name:  "mixed canonical and aliases",
			input: []string{"chat", "stream", "tools"},
			want:  CapChat | CapStream | CapToolCall,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCaps(tt.input)
			if got != tt.want {
				t.Errorf("parseCaps(%v) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseCapsStrict(t *testing.T) {
	t.Run("canonical tokens succeed (each maps to single bit)", func(t *testing.T) {
		got, err := ParseCapsStrict([]string{"chat", "stream", "embed", "insert"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := CapChat | CapStream | CapEmbed | CapInsert
		if got != want {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	t.Run("insert is single-bit only (no generate+stream expansion)", func(t *testing.T) {
		// Critical: aliasedCapability expands "insert" to generate+stream+insert,
		// but ParseCapsStrict must NOT — otherwise user config ["chat","insert"]
		// silently adds generate+stream, violating the REPLACES contract.
		got, err := ParseCapsStrict([]string{"insert"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != CapInsert {
			t.Errorf("got %v, want %v (insert must NOT expand under strict parser)", got, CapInsert)
		}
	})
	t.Run("multi-bit aliases rejected", func(t *testing.T) {
		// "completion", "tools", "embedding" are catalog shorthand that
		// expand multiple bits. They MUST NOT be accepted by the user-config
		// path — otherwise the REPLACES contract leaks expanded bits.
		for _, alias := range []string{"completion", "tools", "embedding"} {
			_, err := ParseCapsStrict([]string{alias})
			if err == nil {
				t.Errorf("expected error for alias %q, got nil", alias)
			}
		}
	})
	t.Run("known alias rejection includes did-you-mean hint", func(t *testing.T) {
		// Known alias -> canonical mappings make the rejection actionable.
		// Without the hint the user only sees the full vocabulary dump and
		// has to guess which canonical token they meant.
		cases := map[string]string{
			"tools":      "tool_call",
			"Tools":      "tool_call", // case-insensitive lookup
			"embedding":  "embed",
			"completion": "chat",
		}
		for alias, wantSubstr := range cases {
			_, err := ParseCapsStrict([]string{alias})
			if err == nil {
				t.Fatalf("expected error for alias %q, got nil", alias)
			}
			if !strings.Contains(err.Error(), "did you mean") {
				t.Errorf("error for %q should include 'did you mean' hint, got: %v", alias, err)
			}
			if !strings.Contains(err.Error(), wantSubstr) {
				t.Errorf("error for %q should suggest %q, got: %v", alias, wantSubstr, err)
			}
		}
	})
	t.Run("unknown (non-alias) rejection falls back to canonical-names list", func(t *testing.T) {
		// Tokens not in aliasSuggestions should still get the vocabulary
		// dump so the user has something to work with.
		_, err := ParseCapsStrict([]string{"totallyfake"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "canonical names:") {
			t.Errorf("unknown token error should list canonical names, got: %v", err)
		}
		if strings.Contains(err.Error(), "did you mean") {
			t.Errorf("unknown (non-alias) token error should NOT include did-you-mean, got: %v", err)
		}
	})
	t.Run("unknown token rejected", func(t *testing.T) {
		_, err := ParseCapsStrict([]string{"chat", "nonexistent"})
		if err == nil {
			t.Fatal("expected error for unknown token, got nil")
		}
		if !strings.Contains(err.Error(), "nonexistent") {
			t.Errorf("error should mention the bad token, got: %v", err)
		}
	})
	t.Run("empty input is no error", func(t *testing.T) {
		got, err := ParseCapsStrict(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != 0 {
			t.Errorf("got %v, want 0", got)
		}
	})
	t.Run("case-insensitive (mixed case accepted)", func(t *testing.T) {
		got, err := ParseCapsStrict([]string{"Chat", "STREAM"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := CapChat | CapStream
		if got != want {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

func TestCanonicalCapabilityNames_Stable(t *testing.T) {
	// Guard against accidental drift: the public list is consumed by adjacent
	// packages as the single source of truth for user-config vocabulary. If
	// names are added/removed, downstream config validation must update too.
	want := map[string]bool{
		"chat": true, "generate": true, "stream": true, "embed": true,
		"tool_call": true, "thinking": true, "insert": true,
	}
	if len(CanonicalCapabilityNames) != len(want) {
		t.Fatalf("CanonicalCapabilityNames length = %d, want %d (%v vs %v)",
			len(CanonicalCapabilityNames), len(want), CanonicalCapabilityNames, want)
	}
	for _, name := range CanonicalCapabilityNames {
		if !want[name] {
			t.Errorf("unexpected canonical name %q", name)
		}
	}
}
