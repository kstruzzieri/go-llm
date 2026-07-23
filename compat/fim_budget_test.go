package compat

import "testing"

func TestFIMBudget_QwenPythonPrefixReduced(t *testing.T) {
	split := baseSplit(fimFamily("qwen3"))
	adj := languageAdjust("python")
	got := clampPct(split + adj)
	if got != 65 {
		t.Errorf("Qwen+Python prefix pct = %d, want 65", got)
	}
}

func TestFIMBudget_UnknownFamilyConservative(t *testing.T) {
	if baseSplit("unknown") != 70 {
		t.Errorf("unknown family default = %d, want 70", baseSplit("unknown"))
	}
}

func TestFIMBudget_Clamped(t *testing.T) {
	if clampPct(5) != 40 {
		t.Errorf("clamp low = %d, want 40", clampPct(5))
	}
	if clampPct(99) != 90 {
		t.Errorf("clamp high = %d, want 90", clampPct(99))
	}
}

func TestAdaptiveSplit_ShortPrefixTransfersBudgetToSuffix(t *testing.T) {
	result := adaptiveSplit(100, 1000, 70, 1000)
	if result.Prefix != 100 {
		t.Errorf("prefix = %d, want 100 (short prefix)", result.Prefix)
	}
	if result.Suffix < 600 {
		t.Errorf("suffix = %d, want >= 600 (surplus transferred)", result.Suffix)
	}
}

func TestAdaptiveSplit_BothFitWithinBudget(t *testing.T) {
	result := adaptiveSplit(200, 100, 70, 1000)
	if result.Prefix != 200 || result.Suffix != 100 {
		t.Errorf("both-fit: got prefix=%d suffix=%d, want 200/100", result.Prefix, result.Suffix)
	}
}

func TestAdaptiveSplit_BothExceedBudget(t *testing.T) {
	result := adaptiveSplit(5000, 5000, 70, 1000)
	if result.Prefix != 700 || result.Suffix != 300 {
		t.Errorf("both-exceed: got prefix=%d suffix=%d, want 700/300", result.Prefix, result.Suffix)
	}
}

func TestBudgetForProfile_EndToEnd(t *testing.T) {
	// Qwen3 (75) + Python (-10) = 65 — below family default, reasonable for a
	// language where suffix context tends to matter more.
	result := budgetForProfile("qwen3-coder", "python", 0, 200, 200, 1000)
	// pct = 65, so prefixBudget = 650, suffixBudget = 350.
	// Both fit; surplus from prefix (650-200=450) goes to suffix, giving suffix
	// budget 800. Both shorter than their budgets: prefix=200, suffix=200.
	if result.Prefix != 200 || result.Suffix != 200 {
		t.Errorf("end-to-end short: got %+v, want {200,200}", result)
	}
}

func TestBudgetForProfile_OverrideBeatsFamilyDefault(t *testing.T) {
	// Override of 50 replaces the family default (75) before language adjust.
	// Go delta = +5, so final pct = clamp(50+5) = 55.
	// Max 1000 -> prefixBudget 550, suffixBudget 450.
	result := budgetForProfile("qwen3", "go", 50, 5000, 5000, 1000)
	if result.Prefix != 550 || result.Suffix != 450 {
		t.Errorf("override: got %+v, want {550,450}", result)
	}
}

func TestBudgetForProfile_OverrideClampedHigh(t *testing.T) {
	// Override of 95 would exceed the 90 ceiling; clampPct should enforce it.
	// Language=rust (+5) would push it to 100; final clamp = 90.
	result := budgetForProfile("deepseek", "rust", 95, 5000, 5000, 1000)
	if result.Prefix != 900 || result.Suffix != 100 {
		t.Errorf("override clamped high: got %+v, want {900,100}", result)
	}
}

func TestBudgetForProfile_UnknownFamilyAndLanguage(t *testing.T) {
	// Unknown family -> 70 base, unknown language -> 0 delta, so pct = 70.
	result := budgetForProfile("made-up-model", "klingon", 0, 5000, 5000, 1000)
	if result.Prefix != 700 || result.Suffix != 300 {
		t.Errorf("unknown: got %+v, want {700,300}", result)
	}
}
