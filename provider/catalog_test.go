package provider

import "testing"

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
		"qwen2.5", "qwen3", "qwen3.5",
		"deepseek-r1", "deepseek-coder-v2",
		"codellama", "starcoder", "starcoder2",
		"llama3", "llama3.1", "llama3.2", "llama3.3",
		"gemma2", "gemma3",
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

func TestCatalogFIMTokens(t *testing.T) {
	cat, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog() error: %v", err)
	}

	sc := newStaticCatalog(cat)

	tests := []struct {
		family     string
		wantPrefix string
		wantSuffix string
		wantMiddle string
	}{
		{"qwen3", "<|fim_prefix|>", "<|fim_suffix|>", "<|fim_middle|>"},
		{"qwen2.5", "<|fim_prefix|>", "<|fim_suffix|>", "<|fim_middle|>"},
		{"codellama", "<PRE>", " <SUF>", " <MID>"},
		{"starcoder2", "<fim_prefix>", "<fim_suffix>", "<fim_middle>"},
		{"starcoder", "<fim_prefix>", "<fim_suffix>", "<fim_middle>"},
		{"mistral", "[PREFIX]", "[SUFFIX]", "[MIDDLE]"},
		{"deepseek-coder-v2", "<|fim_prefix|>", "<|fim_suffix|>", "<|fim_middle|>"},
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
			if profile.FIM.Prefix != tt.wantPrefix {
				t.Errorf("FIM.Prefix = %q, want %q", profile.FIM.Prefix, tt.wantPrefix)
			}
			if profile.FIM.Suffix != tt.wantSuffix {
				t.Errorf("FIM.Suffix = %q, want %q", profile.FIM.Suffix, tt.wantSuffix)
			}
			if profile.FIM.Middle != tt.wantMiddle {
				t.Errorf("FIM.Middle = %q, want %q", profile.FIM.Middle, tt.wantMiddle)
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
		if p.FIM.Prefix != "<|fim_prefix|>" {
			t.Errorf("FIM.Prefix = %q, want %q", p.FIM.Prefix, "<|fim_prefix|>")
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
