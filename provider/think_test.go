package provider

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// collect is a test helper that creates onThinking and onContent callbacks
// capturing all emitted text into two slices.
func collect() (onThinking, onContent func(string) error, thinking, content *[]string) {
	var th, ct []string
	onTh := func(s string) error { th = append(th, s); return nil }
	onCt := func(s string) error { ct = append(ct, s); return nil }
	return onTh, onCt, &th, &ct
}

func joined(parts *[]string) string {
	if parts == nil {
		return ""
	}
	return strings.Join(*parts, "")
}

func TestThinkParser_BasicThinking(t *testing.T) {
	onTh, onCt, thinking, content := collect()
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	chunks := []string{"<think>", "reasoning here", "</think>", "the answer"}
	for _, c := range chunks {
		if err := p.Process(c); err != nil {
			t.Fatalf("Process(%q) error: %v", c, err)
		}
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	if got := joined(thinking); got != "reasoning here" {
		t.Errorf("thinking = %q, want %q", got, "reasoning here")
	}
	if got := joined(content); got != "the answer" {
		t.Errorf("content = %q, want %q", got, "the answer")
	}
}

func TestThinkParser_FragmentedOpenTag(t *testing.T) {
	tests := []struct {
		name    string
		chunks  []string
		wantTh  string
		wantCt  string
	}{
		{
			name:   "split after <",
			chunks: []string{"<", "think>reasoning</think>answer"},
			wantTh: "reasoning",
			wantCt: "answer",
		},
		{
			name:   "split after <th",
			chunks: []string{"<th", "ink>reasoning</think>answer"},
			wantTh: "reasoning",
			wantCt: "answer",
		},
		{
			name:   "split after <think",
			chunks: []string{"<think", ">reasoning</think>answer"},
			wantTh: "reasoning",
			wantCt: "answer",
		},
		{
			name:   "char by char open tag",
			chunks: []string{"<", "t", "h", "i", "n", "k", ">", "r", "</think>a"},
			wantTh: "r",
			wantCt: "a",
		},
		{
			name:   "split in middle of open tag with content before",
			chunks: []string{"hello<thi", "nk>thinking</think>done"},
			wantTh: "thinking",
			wantCt: "hellodone",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			onTh, onCt, thinking, content := collect()
			p := NewThinkParser(ThinkParserConfig{
				Mode:       ThinkAlways,
				Tags:       DefaultThinkTags(),
				OnThinking: onTh,
				OnContent:  onCt,
			})
			for _, c := range tt.chunks {
				if err := p.Process(c); err != nil {
					t.Fatalf("Process(%q): %v", c, err)
				}
			}
			if err := p.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			if got := joined(thinking); got != tt.wantTh {
				t.Errorf("thinking = %q, want %q", got, tt.wantTh)
			}
			if got := joined(content); got != tt.wantCt {
				t.Errorf("content = %q, want %q", got, tt.wantCt)
			}
		})
	}
}

func TestThinkParser_FragmentedCloseTag(t *testing.T) {
	tests := []struct {
		name    string
		chunks  []string
		wantTh  string
		wantCt  string
	}{
		{
			name:   "split after </",
			chunks: []string{"<think>reasoning</", "think>answer"},
			wantTh: "reasoning",
			wantCt: "answer",
		},
		{
			name:   "split after </thi",
			chunks: []string{"<think>reasoning</thi", "nk>answer"},
			wantTh: "reasoning",
			wantCt: "answer",
		},
		{
			name:   "char by char close tag",
			chunks: []string{"<think>r<", "/", "t", "h", "i", "n", "k", ">", "a"},
			wantTh: "r",
			wantCt: "a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			onTh, onCt, thinking, content := collect()
			p := NewThinkParser(ThinkParserConfig{
				Mode:       ThinkAlways,
				Tags:       DefaultThinkTags(),
				OnThinking: onTh,
				OnContent:  onCt,
			})
			for _, c := range tt.chunks {
				if err := p.Process(c); err != nil {
					t.Fatalf("Process(%q): %v", c, err)
				}
			}
			if err := p.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			if got := joined(thinking); got != tt.wantTh {
				t.Errorf("thinking = %q, want %q", got, tt.wantTh)
			}
			if got := joined(content); got != tt.wantCt {
				t.Errorf("content = %q, want %q", got, tt.wantCt)
			}
		})
	}
}

func TestThinkParser_NoThinkTags(t *testing.T) {
	onTh, onCt, thinking, content := collect()
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	for _, c := range []string{"just ", "plain ", "text"} {
		if err := p.Process(c); err != nil {
			t.Fatalf("Process(%q): %v", c, err)
		}
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if got := joined(thinking); got != "" {
		t.Errorf("thinking = %q, want empty", got)
	}
	if got := joined(content); got != "just plain text" {
		t.Errorf("content = %q, want %q", got, "just plain text")
	}
}

func TestThinkParser_ThinkNone_Passthrough(t *testing.T) {
	onTh, onCt, thinking, content := collect()
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkNone,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	input := "<think>reasoning</think>answer"
	if err := p.Process(input); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	if got := joined(thinking); got != "" {
		t.Errorf("thinking = %q, want empty (ThinkNone)", got)
	}
	// In ThinkNone mode, everything including tags passes through as content.
	if got := joined(content); got != input {
		t.Errorf("content = %q, want %q", got, input)
	}
}

func TestThinkParser_ThinkAuto_Detects(t *testing.T) {
	t.Run("detects tags in first 64 chars", func(t *testing.T) {
		onTh, onCt, thinking, content := collect()
		p := NewThinkParser(ThinkParserConfig{
			Mode:       ThinkAuto,
			Tags:       DefaultThinkTags(),
			OnThinking: onTh,
			OnContent:  onCt,
		})

		if err := p.Process("<think>auto reasoning</think>answer"); err != nil {
			t.Fatalf("Process error: %v", err)
		}
		if err := p.Flush(); err != nil {
			t.Fatalf("Flush error: %v", err)
		}

		if got := joined(thinking); got != "auto reasoning" {
			t.Errorf("thinking = %q, want %q", got, "auto reasoning")
		}
		if got := joined(content); got != "answer" {
			t.Errorf("content = %q, want %q", got, "answer")
		}
	})

	t.Run("drops to passthrough when no tags found", func(t *testing.T) {
		onTh, onCt, thinking, content := collect()
		p := NewThinkParser(ThinkParserConfig{
			Mode:       ThinkAuto,
			Tags:       DefaultThinkTags(),
			OnThinking: onTh,
			OnContent:  onCt,
		})

		// Send more than 64 chars with no think tags at all.
		long := strings.Repeat("x", 70)
		if err := p.Process(long); err != nil {
			t.Fatalf("Process error: %v", err)
		}
		if err := p.Flush(); err != nil {
			t.Fatalf("Flush error: %v", err)
		}

		if got := joined(thinking); got != "" {
			t.Errorf("thinking = %q, want empty", got)
		}
		if got := joined(content); got != long {
			t.Errorf("content len = %d, want %d", len(got), len(long))
		}
	})

	t.Run("detects tags across multiple chunks within 64 chars", func(t *testing.T) {
		onTh, onCt, thinking, content := collect()
		p := NewThinkParser(ThinkParserConfig{
			Mode:       ThinkAuto,
			Tags:       DefaultThinkTags(),
			OnThinking: onTh,
			OnContent:  onCt,
		})

		// Feed small chunks that together form a think tag.
		if err := p.Process("<thi"); err != nil {
			t.Fatalf("Process error: %v", err)
		}
		if err := p.Process("nk>auto</think>done"); err != nil {
			t.Fatalf("Process error: %v", err)
		}
		if err := p.Flush(); err != nil {
			t.Fatalf("Flush error: %v", err)
		}

		if got := joined(thinking); got != "auto" {
			t.Errorf("thinking = %q, want %q", got, "auto")
		}
		if got := joined(content); got != "done" {
			t.Errorf("content = %q, want %q", got, "done")
		}
	})
}

func TestThinkParser_MultipleThinkBlocks(t *testing.T) {
	onTh, onCt, thinking, content := collect()
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	input := "<think>A</think>middle<think>B</think>end"
	if err := p.Process(input); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	if got := joined(thinking); got != "AB" {
		t.Errorf("thinking = %q, want %q", got, "AB")
	}
	if got := joined(content); got != "middleend" {
		t.Errorf("content = %q, want %q", got, "middleend")
	}
}

func TestThinkParser_EmptyThinkBlock(t *testing.T) {
	onTh, onCt, thinking, content := collect()
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	if err := p.Process("<think></think>answer"); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	if got := joined(thinking); got != "" {
		t.Errorf("thinking = %q, want empty", got)
	}
	if got := joined(content); got != "answer" {
		t.Errorf("content = %q, want %q", got, "answer")
	}
}

func TestThinkParser_StreamEndsMidOpenTag(t *testing.T) {
	// Stream ends while we're trying to match an open tag (stateTagOpen).
	// The partial tag buffer should be emitted as content on Flush.
	onTh, onCt, thinking, content := collect()
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	if err := p.Process("hello<thi"); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	if got := joined(thinking); got != "" {
		t.Errorf("thinking = %q, want empty", got)
	}
	if got := joined(content); got != "hello<thi" {
		t.Errorf("content = %q, want %q", got, "hello<thi")
	}
}

func TestThinkParser_StreamEndsMidCloseTag(t *testing.T) {
	// Stream ends while we're trying to match a close tag (stateTagClose).
	// We're inside a thinking block, so the partial tag buffer should be
	// emitted as thinking on Flush.
	onTh, onCt, thinking, content := collect()
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	if err := p.Process("<think>reasoning</thi"); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	if got := joined(thinking); got != "reasoning</thi" {
		t.Errorf("thinking = %q, want %q", got, "reasoning</thi")
	}
	if got := joined(content); got != "" {
		t.Errorf("content = %q, want empty", got)
	}
}

func TestThinkParser_CustomTags(t *testing.T) {
	onTh, onCt, thinking, content := collect()
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       ThinkTags{Open: "<reasoning>", Close: "</reasoning>"},
		OnThinking: onTh,
		OnContent:  onCt,
	})

	input := "<reasoning>custom thinking</reasoning>result"
	if err := p.Process(input); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	if got := joined(thinking); got != "custom thinking" {
		t.Errorf("thinking = %q, want %q", got, "custom thinking")
	}
	if got := joined(content); got != "result" {
		t.Errorf("content = %q, want %q", got, "result")
	}
}

func TestThinkParser_FalsePositiveOpenTag(t *testing.T) {
	// "x < y" contains a '<' but doesn't start a think tag.
	onTh, onCt, thinking, content := collect()
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	if err := p.Process("x < y is true"); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	if got := joined(thinking); got != "" {
		t.Errorf("thinking = %q, want empty", got)
	}
	if got := joined(content); got != "x < y is true" {
		t.Errorf("content = %q, want %q", got, "x < y is true")
	}
}

func TestThinkParser_FalsePositiveCloseTag(t *testing.T) {
	// "</div>" inside a thinking block is not the close tag.
	onTh, onCt, thinking, content := collect()
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	input := "<think>some </div> html</think>answer"
	if err := p.Process(input); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	if got := joined(thinking); got != "some </div> html" {
		t.Errorf("thinking = %q, want %q", got, "some </div> html")
	}
	if got := joined(content); got != "answer" {
		t.Errorf("content = %q, want %q", got, "answer")
	}
}

func TestThinkParser_CallbackErrorPropagation(t *testing.T) {
	t.Run("onContent error", func(t *testing.T) {
		errTest := errors.New("content callback error")
		p := NewThinkParser(ThinkParserConfig{
			Mode:       ThinkAlways,
			Tags:       DefaultThinkTags(),
			OnThinking: func(string) error { return nil },
			OnContent:  func(string) error { return errTest },
		})

		err := p.Process("hello")
		if !errors.Is(err, errTest) {
			t.Errorf("got error %v, want %v", err, errTest)
		}
	})

	t.Run("onThinking error", func(t *testing.T) {
		errTest := errors.New("thinking callback error")
		p := NewThinkParser(ThinkParserConfig{
			Mode:       ThinkAlways,
			Tags:       DefaultThinkTags(),
			OnThinking: func(string) error { return errTest },
			OnContent:  func(string) error { return nil },
		})

		err := p.Process("<think>reasoning</think>")
		if !errors.Is(err, errTest) {
			t.Errorf("got error %v, want %v", err, errTest)
		}
	})
}

func TestThinkParser_ResetAndReuse(t *testing.T) {
	onTh, onCt, thinking, content := collect()
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	// First response: use longer strings so token estimates are distinct.
	// "first reasoning block" = 21 chars => ~5 tokens
	// "the first content" = 17 chars => ~4 tokens
	if err := p.Process("<think>first reasoning block</think>the first content"); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	m1 := p.Metrics()
	if m1.ThinkingTokens == 0 {
		t.Error("first response should have thinking tokens")
	}

	// Reset and collect new buffers.
	onTh2, onCt2, thinking2, content2 := collect()
	p.Reset()

	// Verify metrics are zeroed after reset.
	mReset := p.Metrics()
	if mReset.ThinkingTokens != 0 || mReset.ContentTokens != 0 {
		t.Errorf("after Reset: ThinkingTokens=%d, ContentTokens=%d; want both 0",
			mReset.ThinkingTokens, mReset.ContentTokens)
	}

	// Reconfigure callbacks for second response.
	p.OnThinking = onTh2
	p.OnContent = onCt2

	// "second" = 6 chars => ~1 token; "two" = 3 chars => ~1 token
	if err := p.Process("<think>second</think>two"); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	// Verify first response was correct.
	if got := joined(thinking); got != "first reasoning block" {
		t.Errorf("first thinking = %q, want %q", got, "first reasoning block")
	}
	if got := joined(content); got != "the first content" {
		t.Errorf("first content = %q, want %q", got, "the first content")
	}

	// Verify second response is independent.
	if got := joined(thinking2); got != "second" {
		t.Errorf("second thinking = %q, want %q", got, "second")
	}
	if got := joined(content2); got != "two" {
		t.Errorf("second content = %q, want %q", got, "two")
	}

	// Verify metrics reflect only the second response.
	m2 := p.Metrics()
	if m2.ThinkingTokens == m1.ThinkingTokens {
		t.Errorf("second ThinkingTokens (%d) should differ from first (%d)",
			m2.ThinkingTokens, m1.ThinkingTokens)
	}
}

func TestThinkParser_Metrics(t *testing.T) {
	onTh, onCt := func(string) error { return nil }, func(string) error { return nil }
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	// "reasoning text" = 14 chars, ~3 tokens (14/4)
	// "the answer" = 10 chars, ~2 tokens (10/4)
	if err := p.Process("<think>reasoning text</think>the answer"); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	m := p.Metrics()
	if m.ThinkingTokens != estimateTokens("reasoning text") {
		t.Errorf("ThinkingTokens = %d, want %d", m.ThinkingTokens, estimateTokens("reasoning text"))
	}
	if m.ContentTokens != estimateTokens("the answer") {
		t.Errorf("ContentTokens = %d, want %d", m.ContentTokens, estimateTokens("the answer"))
	}

	wantRatio := float64(m.ThinkingTokens) / float64(m.ThinkingTokens+m.ContentTokens)
	if m.ThinkRatio != wantRatio {
		t.Errorf("ThinkRatio = %v, want %v", m.ThinkRatio, wantRatio)
	}
}

func TestThinkParser_BudgetSkip_MaxTokens(t *testing.T) {
	var thinkCalls int
	onTh := func(s string) error { thinkCalls++; return nil }
	onCt := func(string) error { return nil }
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
		Budget: &ThinkBudget{
			MaxTokens:  1, // very low budget
			OnExceeded: ThinkBudgetSkip,
		},
	})

	// The thinking content exceeds 1 token budget.
	// "lots of thinking content here" is ~7 tokens.
	if err := p.Process("<think>lots of thinking content here</think>answer"); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	// After budget exceeded, onThinking should stop being called for
	// subsequent think text, but the state machine should still transition properly.
	m := p.Metrics()
	if m.ThinkingTokens == 0 {
		t.Error("ThinkingTokens should still track even when budget exceeded")
	}
}

func TestThinkParser_BudgetMaxTime(t *testing.T) {
	var thinkChunks []string
	onTh := func(s string) error { thinkChunks = append(thinkChunks, s); return nil }
	onCt := func(string) error { return nil }
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
		Budget: &ThinkBudget{
			MaxTime:    1 * time.Nanosecond, // effectively immediate expiry
			OnExceeded: ThinkBudgetSkip,
		},
	})

	// Give the time budget a chance to expire.
	time.Sleep(2 * time.Nanosecond)

	if err := p.Process("<think>thinking after budget</think>done"); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	// Thinking tokens should still be tracked in metrics.
	m := p.Metrics()
	if m.ThinkingTokens == 0 {
		t.Error("ThinkingTokens should track even when time budget exceeded")
	}
}

func TestThinkParser_NestedThinkTag(t *testing.T) {
	// Nested <think> inside thinking: the first </think> closes the block.
	onTh, onCt, thinking, content := collect()
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	input := "<think>outer <think>inner</think>after"
	if err := p.Process(input); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	// The inner <think> is just text inside the thinking block.
	// The first </think> closes the block.
	if got := joined(thinking); got != "outer <think>inner" {
		t.Errorf("thinking = %q, want %q", got, "outer <think>inner")
	}
	if got := joined(content); got != "after" {
		t.Errorf("content = %q, want %q", got, "after")
	}
}

func TestThinkParser_FastPath_NoAngleBracket(t *testing.T) {
	var contentCalls int
	onCt := func(string) error { contentCalls++; return nil }
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: func(string) error { return nil },
		OnContent:  onCt,
	})

	// A large chunk with no '<' should be handled in a single callback.
	big := strings.Repeat("hello world ", 100)
	if err := p.Process(big); err != nil {
		t.Fatalf("Process error: %v", err)
	}

	if contentCalls != 1 {
		t.Errorf("contentCalls = %d, want 1 (fast path)", contentCalls)
	}
}

func TestThinkParser_FastPath_NoAngleBracket_InThinking(t *testing.T) {
	var thinkCalls int
	onTh := func(string) error { thinkCalls++; return nil }
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  func(string) error { return nil },
	})

	// Enter thinking state first.
	if err := p.Process("<think>"); err != nil {
		t.Fatalf("Process error: %v", err)
	}

	// Now send a large chunk with no '<' — should be single callback as thinking.
	thinkCalls = 0
	big := strings.Repeat("reasoning step ", 100)
	if err := p.Process(big); err != nil {
		t.Fatalf("Process error: %v", err)
	}

	if thinkCalls != 1 {
		t.Errorf("thinkCalls = %d, want 1 (fast path in thinking)", thinkCalls)
	}
}

func TestThinkParser_ComplexFragmented(t *testing.T) {
	// A complex scenario where both open and close tags are fragmented
	// across multiple small chunks.
	onTh, onCt, thinking, content := collect()
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	chunks := []string{
		"pre",
		"<",
		"think",
		">",
		"mid",
		"dle",
		"<",
		"/think",
		">",
		"post",
	}
	for _, c := range chunks {
		if err := p.Process(c); err != nil {
			t.Fatalf("Process(%q): %v", c, err)
		}
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if got := joined(thinking); got != "middle" {
		t.Errorf("thinking = %q, want %q", got, "middle")
	}
	if got := joined(content); got != "prepost" {
		t.Errorf("content = %q, want %q", got, "prepost")
	}
}

func TestThinkParser_ThinkAuto_NoTagsEarlyFlush(t *testing.T) {
	// In auto mode, if we never reach 64 chars and no tags found, Flush
	// should emit the sniff buffer as content.
	onTh, onCt, thinking, content := collect()
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAuto,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	if err := p.Process("short"); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	if got := joined(thinking); got != "" {
		t.Errorf("thinking = %q, want empty", got)
	}
	if got := joined(content); got != "short" {
		t.Errorf("content = %q, want %q", got, "short")
	}
}

func TestThinkParser_ContentBeforeAndAfterThink(t *testing.T) {
	onTh, onCt, thinking, content := collect()
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	if err := p.Process("before<think>inside</think>after"); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	if got := joined(thinking); got != "inside" {
		t.Errorf("thinking = %q, want %q", got, "inside")
	}
	if got := joined(content); got != "beforeafter" {
		t.Errorf("content = %q, want %q", got, "beforeafter")
	}
}

func TestThinkParser_BudgetCancel(t *testing.T) {
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: func(string) error { return nil },
		OnContent:  func(string) error { return nil },
		Budget: &ThinkBudget{
			MaxTokens:  1,
			OnExceeded: ThinkBudgetCancel,
		},
	})

	err := p.Process("<think>lots of thinking content that exceeds budget</think>answer")
	if err == nil {
		t.Fatal("expected error when budget cancel is triggered")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("error %q should mention budget", err.Error())
	}
}

func TestThinkParser_MetricsThinkDuration(t *testing.T) {
	onTh := func(string) error { return nil }
	onCt := func(string) error { return nil }
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	if err := p.Process("<think>"); err != nil {
		t.Fatalf("Process error: %v", err)
	}

	// Small sleep to ensure measurable duration.
	time.Sleep(5 * time.Millisecond)

	if err := p.Process("thinking</think>done"); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	m := p.Metrics()
	if m.ThinkDuration < 5*time.Millisecond {
		t.Errorf("ThinkDuration = %v, want >= 5ms", m.ThinkDuration)
	}
}

func TestThinkParser_MetricsZeroTokens(t *testing.T) {
	// When there are zero tokens, ThinkRatio should be 0.
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: func(string) error { return nil },
		OnContent:  func(string) error { return nil },
	})

	if err := p.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	m := p.Metrics()
	if m.ThinkRatio != 0 {
		t.Errorf("ThinkRatio = %v, want 0", m.ThinkRatio)
	}
}

func TestThinkParser_AngleBracketInsideContent(t *testing.T) {
	// A '<' that starts to match but doesn't complete the open tag should
	// emit the partial match and continue as content.
	onTh, onCt, thinking, content := collect()
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	if err := p.Process("<div>not a think tag</div>"); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	if got := joined(thinking); got != "" {
		t.Errorf("thinking = %q, want empty", got)
	}
	if got := joined(content); got != "<div>not a think tag</div>" {
		t.Errorf("content = %q, want %q", got, "<div>not a think tag</div>")
	}
}

func TestThinkParser_ThinkToggle_SetActiveTrue(t *testing.T) {
	onTh, onCt, thinking, content := collect()
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkToggle,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	// Activate reasoning for this request.
	p.SetActive(true)

	for _, chunk := range []string{"<think>reasoning</think>", "answer"} {
		if err := p.Process(chunk); err != nil {
			t.Fatalf("Process(%q) error: %v", chunk, err)
		}
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush() error: %v", err)
	}

	if got := joined(thinking); got != "reasoning" {
		t.Errorf("thinking = %q, want %q", got, "reasoning")
	}
	if got := joined(content); got != "answer" {
		t.Errorf("content = %q, want %q", got, "answer")
	}
}

func TestThinkParser_ThinkToggle_SetActiveFalse(t *testing.T) {
	onTh, onCt, thinking, content := collect()
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkToggle,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	// Deactivate reasoning — passthrough mode.
	p.SetActive(false)

	input := "<think>this should pass through</think>as content"
	if err := p.Process(input); err != nil {
		t.Fatalf("Process() error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush() error: %v", err)
	}

	// Everything should be content, including the tags.
	if got := joined(thinking); got != "" {
		t.Errorf("thinking = %q, want empty", got)
	}
	if got := joined(content); got != input {
		t.Errorf("content = %q, want %q", got, input)
	}
}

func TestThinkParser_ThinkToggle_ResetAndToggle(t *testing.T) {
	onTh, onCt, thinking, content := collect()
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkToggle,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	// Request 1: reasoning active.
	p.SetActive(true)
	if err := p.Process("<think>thought1</think>answer1"); err != nil {
		t.Fatalf("Process() error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush() error: %v", err)
	}
	if got := joined(thinking); got != "thought1" {
		t.Errorf("req1 thinking = %q, want %q", got, "thought1")
	}
	if got := joined(content); got != "answer1" {
		t.Errorf("req1 content = %q, want %q", got, "answer1")
	}

	// Reset for request 2.
	p.Reset()
	*thinking = nil
	*content = nil

	// Request 2: reasoning disabled.
	p.SetActive(false)
	input := "no reasoning here"
	if err := p.Process(input); err != nil {
		t.Fatalf("Process() error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush() error: %v", err)
	}
	if got := joined(thinking); got != "" {
		t.Errorf("req2 thinking = %q, want empty", got)
	}
	if got := joined(content); got != input {
		t.Errorf("req2 content = %q, want %q", got, input)
	}
}

func TestThinkParser_SetActive_NoopForNonToggle(t *testing.T) {
	onTh, onCt, thinking, content := collect()
	p := NewThinkParser(ThinkParserConfig{
		Mode:       ThinkAlways,
		Tags:       DefaultThinkTags(),
		OnThinking: onTh,
		OnContent:  onCt,
	})

	// SetActive should be a no-op for ThinkAlways.
	p.SetActive(false)

	// Should still parse tags (not passthrough).
	if err := p.Process("<think>thought</think>answer"); err != nil {
		t.Fatalf("Process() error: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush() error: %v", err)
	}
	if got := joined(thinking); got != "thought" {
		t.Errorf("thinking = %q, want %q", got, "thought")
	}
	if got := joined(content); got != "answer" {
		t.Errorf("content = %q, want %q", got, "answer")
	}
}
