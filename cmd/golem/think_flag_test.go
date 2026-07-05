package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestParseThinkFlag(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{name: "unset", args: nil, want: ""},
		{name: "off", args: []string{"-think", "off"}, want: "off"},
		{name: "on", args: []string{"-think", "on"}, want: "on"},
		{name: "low", args: []string{"-think", "low"}, want: "low"},
		{name: "medium", args: []string{"-think", "medium"}, want: "medium"},
		{name: "high", args: []string{"-think", "high"}, want: "high"},
		{name: "uppercase normalized", args: []string{"-think", "HIGH"}, want: "high"},
		{name: "invalid", args: []string{"-think", "max"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := parseFlags(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatal("parseFlags: want error, got nil")
				}
				for _, v := range []string{"off", "on", "low", "medium", "high"} {
					if !strings.Contains(err.Error(), v) {
						t.Errorf("error %q does not name valid value %q", err, v)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFlags: %v", err)
			}
			if f.think != tt.want {
				t.Errorf("think = %q, want %q", f.think, tt.want)
			}
		})
	}
}

func TestThinkOptionsMapping(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	tests := []struct {
		in   string
		want provider.ModelOptions
	}{
		{"", provider.ModelOptions{}},
		{"off", provider.ModelOptions{Think: boolPtr(false)}},
		{"on", provider.ModelOptions{Think: boolPtr(true)}},
		{"low", provider.ModelOptions{Think: boolPtr(true), ThinkEffort: "low"}},
		{"medium", provider.ModelOptions{Think: boolPtr(true), ThinkEffort: "medium"}},
		{"high", provider.ModelOptions{Think: boolPtr(true), ThinkEffort: "high"}},
	}
	for _, tt := range tests {
		t.Run("value="+tt.in, func(t *testing.T) {
			got := thinkModelOptions(tt.in)
			if (got.Think == nil) != (tt.want.Think == nil) {
				t.Fatalf("Think nil-ness = %v, want %v", got.Think == nil, tt.want.Think == nil)
			}
			if got.Think != nil && *got.Think != *tt.want.Think {
				t.Errorf("*Think = %v, want %v", *got.Think, *tt.want.Think)
			}
			if got.ThinkEffort != tt.want.ThinkEffort {
				t.Errorf("ThinkEffort = %q, want %q", got.ThinkEffort, tt.want.ThinkEffort)
			}
		})
	}
}

// thinkFakeReg is a capChecker fake that records which lookup method was
// called; profiles carry ThinkMode (the field the gate keys off).
type thinkFakeReg struct {
	byKey      map[provider.ModelKey]provider.ThinkMode
	byModel    map[string][]provider.ThinkMode
	errByKey   map[provider.ModelKey]error
	errByModel map[string]error
	nilByKey   map[provider.ModelKey]bool // Lookup returns (nil, nil)

	lookupCalls    []provider.ModelKey
	lookupAnyCalls []string
}

func (f *thinkFakeReg) Lookup(ctx context.Context, key provider.ModelKey) (*provider.ModelProfile, error) {
	f.lookupCalls = append(f.lookupCalls, key)
	if err := f.errByKey[key]; err != nil {
		return nil, err
	}
	if f.nilByKey[key] {
		return nil, nil
	}
	tm, ok := f.byKey[key]
	if !ok {
		return nil, context.Canceled // not-found: any non-nil error
	}
	return &provider.ModelProfile{Key: key, ThinkMode: tm}, nil
}

func (f *thinkFakeReg) LookupAny(ctx context.Context, model string) ([]*provider.ModelProfile, error) {
	f.lookupAnyCalls = append(f.lookupAnyCalls, model)
	if err := f.errByModel[model]; err != nil {
		return nil, err
	}
	var out []*provider.ModelProfile
	for _, tm := range f.byModel[model] {
		out = append(out, &provider.ModelProfile{Key: provider.ModelKey{Provider: "p", Model: model}, ThinkMode: tm})
	}
	return out, nil
}

func (f *thinkFakeReg) Recommend(ctx context.Context, opts provider.RecommendOpts) ([]*provider.ModelProfile, error) {
	return nil, nil
}

func TestThinkSupportGate(t *testing.T) {
	ctx := context.Background()
	k1 := provider.ModelKey{Provider: "ollama", Model: "m1"}
	k2 := provider.ModelKey{Provider: "ollama", Model: "m2"}

	t.Run("unset flag: zero options, no lookups, no notice", func(t *testing.T) {
		reg := &thinkFakeReg{byKey: map[provider.ModelKey]provider.ThinkMode{k1: provider.ThinkNone}}
		opts, notice := resolveThinkOptions(ctx, reg, []string{"ollama/m1"}, "")
		if opts.Think != nil || opts.ThinkEffort != "" {
			t.Errorf("options = %+v, want zero", opts)
		}
		if notice != "" {
			t.Errorf("notice = %q, want empty", notice)
		}
		if len(reg.lookupCalls) != 0 || len(reg.lookupAnyCalls) != 0 {
			t.Errorf("lookups made with unset flag: Lookup=%v LookupAny=%v", reg.lookupCalls, reg.lookupAnyCalls)
		}
	})

	t.Run("empty chain (recommend mode): options applied, no notice", func(t *testing.T) {
		reg := &thinkFakeReg{}
		opts, notice := resolveThinkOptions(ctx, reg, nil, "on")
		if opts.Think == nil || !*opts.Think {
			t.Errorf("options = %+v, want Think=true", opts)
		}
		if notice != "" {
			t.Errorf("notice = %q, want empty", notice)
		}
	})

	t.Run("all configured candidates ThinkNone: zero options plus notice", func(t *testing.T) {
		reg := &thinkFakeReg{
			byKey:   map[provider.ModelKey]provider.ThinkMode{k1: provider.ThinkNone},
			byModel: map[string][]provider.ThinkMode{"m2": {provider.ThinkNone}},
		}
		opts, notice := resolveThinkOptions(ctx, reg, []string{"ollama/m1", "m2"}, "high")
		if opts.Think != nil || opts.ThinkEffort != "" {
			t.Errorf("options = %+v, want zero", opts)
		}
		if !strings.Contains(notice, "-think ignored") {
			t.Errorf("notice = %q, want it to contain %q", notice, "-think ignored")
		}
		if !strings.Contains(notice, "ollama/m1") {
			t.Errorf("notice = %q, want it to name a ThinkNone model", notice)
		}
	})

	t.Run("mixed chain: options applied, no notice", func(t *testing.T) {
		reg := &thinkFakeReg{byKey: map[provider.ModelKey]provider.ThinkMode{
			k1: provider.ThinkNone,
			k2: provider.ThinkToggle,
		}}
		opts, notice := resolveThinkOptions(ctx, reg, []string{"ollama/m1", "ollama/m2"}, "on")
		if opts.Think == nil || !*opts.Think {
			t.Errorf("options = %+v, want Think=true", opts)
		}
		if notice != "" {
			t.Errorf("notice = %q, want empty", notice)
		}
	})

	t.Run("provider/model selector uses Lookup", func(t *testing.T) {
		reg := &thinkFakeReg{byKey: map[provider.ModelKey]provider.ThinkMode{k1: provider.ThinkToggle}}
		_, _ = resolveThinkOptions(ctx, reg, []string{"ollama/m1"}, "on")
		if len(reg.lookupCalls) != 1 || reg.lookupCalls[0] != k1 {
			t.Errorf("Lookup calls = %v, want exactly [%v]", reg.lookupCalls, k1)
		}
		if len(reg.lookupAnyCalls) != 0 {
			t.Errorf("LookupAny calls = %v, want none", reg.lookupAnyCalls)
		}
	})

	t.Run("bare selector uses LookupAny", func(t *testing.T) {
		reg := &thinkFakeReg{byModel: map[string][]provider.ThinkMode{"m2": {provider.ThinkToggle}}}
		_, _ = resolveThinkOptions(ctx, reg, []string{"m2"}, "on")
		if len(reg.lookupAnyCalls) != 1 || reg.lookupAnyCalls[0] != "m2" {
			t.Errorf("LookupAny calls = %v, want exactly [m2]", reg.lookupAnyCalls)
		}
		if len(reg.lookupCalls) != 0 {
			t.Errorf("Lookup calls = %v, want none", reg.lookupCalls)
		}
	})

	t.Run("lookup error fails open: options applied, no notice", func(t *testing.T) {
		reg := &thinkFakeReg{errByKey: map[provider.ModelKey]error{k1: context.DeadlineExceeded}}
		opts, notice := resolveThinkOptions(ctx, reg, []string{"ollama/m1"}, "on")
		if opts.Think == nil || !*opts.Think {
			t.Errorf("options = %+v, want Think=true (fail open)", opts)
		}
		if notice != "" {
			t.Errorf("notice = %q, want empty", notice)
		}
	})

	t.Run("bare selector multi-match with any non-ThinkNone profile fails open", func(t *testing.T) {
		reg := &thinkFakeReg{byModel: map[string][]provider.ThinkMode{
			"m2": {provider.ThinkNone, provider.ThinkToggle},
		}}
		opts, notice := resolveThinkOptions(ctx, reg, []string{"m2"}, "on")
		if opts.Think == nil || !*opts.Think {
			t.Errorf("options = %+v, want Think=true (fail open)", opts)
		}
		if notice != "" {
			t.Errorf("notice = %q, want empty", notice)
		}
	})

	t.Run("nil profile with nil error fails open", func(t *testing.T) {
		reg := &thinkFakeReg{nilByKey: map[provider.ModelKey]bool{k1: true}}
		opts, notice := resolveThinkOptions(ctx, reg, []string{"ollama/m1"}, "on")
		if opts.Think == nil || !*opts.Think {
			t.Errorf("options = %+v, want Think=true (fail open)", opts)
		}
		if notice != "" {
			t.Errorf("notice = %q, want empty", notice)
		}
	})

	t.Run("bare selector with no matching provider fails open", func(t *testing.T) {
		reg := &thinkFakeReg{}
		opts, notice := resolveThinkOptions(ctx, reg, []string{"ghost"}, "on")
		if opts.Think == nil || !*opts.Think {
			t.Errorf("options = %+v, want Think=true (fail open)", opts)
		}
		if notice != "" {
			t.Errorf("notice = %q, want empty", notice)
		}
	})
}
