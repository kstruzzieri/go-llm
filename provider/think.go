package provider

import (
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Parse state
// ---------------------------------------------------------------------------

// parseState tracks the current position in the think-tag state machine.
type parseState int

const (
	// stateContent means we are outside any think block, emitting content.
	stateContent parseState = iota
	// stateTagOpen means we saw '<' and are accumulating bytes that may
	// match the open tag (e.g. "<think>").
	stateTagOpen
	// stateThinking means we are inside a think block, emitting thinking.
	stateThinking
	// stateTagClose means we saw '<' inside a think block and are
	// accumulating bytes that may match the close tag (e.g. "</think>").
	stateTagClose
)

// sniffLimit is the number of characters ThinkAuto inspects before deciding
// whether the stream contains think tags.
const sniffLimit = 64

// ---------------------------------------------------------------------------
// ThinkParserConfig
// ---------------------------------------------------------------------------

// ThinkParserConfig holds the parameters for constructing a ThinkParser.
type ThinkParserConfig struct {
	// Mode controls whether thinking is parsed. ThinkNone disables parsing
	// entirely (passthrough). ThinkAuto sniffs the first sniffLimit chars.
	Mode ThinkMode

	// Tags defines the open/close delimiters for thinking blocks.
	Tags ThinkTags

	// OnThinking is called with thinking content extracted from within tags.
	OnThinking func(string) error

	// OnContent is called with non-thinking content.
	OnContent func(string) error

	// Budget optionally constrains thinking resources. May be nil.
	Budget *ThinkBudget
}

// ---------------------------------------------------------------------------
// ThinkParser
// ---------------------------------------------------------------------------

// ThinkParser is a streaming state machine that separates model output into
// thinking and content streams by detecting configurable XML-like tags.
// It is designed for single-goroutine use within a streaming response handler.
//
// Usage:
//
//	p := NewThinkParser(cfg)
//	for chunk := range stream {
//	    if err := p.Process(chunk); err != nil { ... }
//	}
//	if err := p.Flush(); err != nil { ... }
//	metrics := p.Metrics()
type ThinkParser struct {
	// OnThinking is the callback for thinking content. It may be replaced
	// between calls to Reset to redirect output.
	OnThinking func(string) error

	// OnContent is the callback for non-thinking content. It may be replaced
	// between calls to Reset to redirect output.
	OnContent func(string) error

	origMode ThinkMode // original mode from config, preserved across Reset
	mode     ThinkMode
	tags     ThinkTags
	budget   *ThinkBudget

	state  parseState
	tagBuf strings.Builder // accumulates partial tag matches

	// Auto-detection state.
	autoResolved bool   // true once ThinkAuto has decided
	sniffBuf     string // accumulates up to sniffLimit chars for auto mode

	// Metrics tracking.
	thinkingTokens int
	contentTokens  int
	thinkStart     time.Time     // when the current thinking block began
	thinkDuration  time.Duration // accumulated thinking duration

	// Budget enforcement.
	budgetExceeded bool
}

// NewThinkParser creates a ThinkParser with the given configuration.
func NewThinkParser(cfg ThinkParserConfig) *ThinkParser {
	return &ThinkParser{
		OnThinking: cfg.OnThinking,
		OnContent:  cfg.OnContent,
		origMode:   cfg.Mode,
		mode:       cfg.Mode,
		tags:       cfg.Tags,
		budget:     cfg.Budget,
		state:      stateContent,
	}
}

// Process feeds a streaming chunk into the parser. It returns an error if a
// callback returns an error or if a budget cancel is triggered.
func (p *ThinkParser) Process(chunk string) error {
	if len(chunk) == 0 {
		return nil
	}

	// ThinkNone: complete passthrough, no parsing at all.
	if p.mode == ThinkNone {
		p.contentTokens += estimateTokens(chunk)
		return p.OnContent(chunk)
	}

	// ThinkAuto: sniff the first sniffLimit chars to decide.
	if p.mode == ThinkAuto && !p.autoResolved {
		return p.processAutoSniff(chunk)
	}

	return p.processChunk(chunk)
}

// processAutoSniff handles the initial sniffing phase in ThinkAuto mode.
func (p *ThinkParser) processAutoSniff(chunk string) error {
	p.sniffBuf += chunk
	if len(p.sniffBuf) >= sniffLimit {
		return p.resolveAuto()
	}
	// Check if the open tag is already present in what we have so far.
	if strings.Contains(p.sniffBuf, p.tags.Open) {
		return p.resolveAuto()
	}
	// Check if a prefix of the open tag is present at the end of sniffBuf,
	// meaning the tag might span the next chunk. Keep sniffing.
	if p.hasPartialTagPrefix(p.sniffBuf, p.tags.Open) {
		return nil
	}
	return nil
}

// hasPartialTagPrefix reports whether s ends with a non-empty prefix of tag.
func (p *ThinkParser) hasPartialTagPrefix(s, tag string) bool {
	for i := 1; i < len(tag) && i <= len(s); i++ {
		if s[len(s)-i:] == tag[:i] {
			return true
		}
	}
	return false
}

// resolveAuto decides whether to activate parsing or passthrough.
func (p *ThinkParser) resolveAuto() error {
	p.autoResolved = true
	if strings.Contains(p.sniffBuf, p.tags.Open) {
		// Tags detected: switch to always mode and replay buffer.
		p.mode = ThinkAlways
		buf := p.sniffBuf
		p.sniffBuf = ""
		return p.processChunk(buf)
	}
	// No tags: switch to passthrough for the rest of the stream.
	p.mode = ThinkNone
	buf := p.sniffBuf
	p.sniffBuf = ""
	p.contentTokens += estimateTokens(buf)
	return p.OnContent(buf)
}

// processChunk runs the state machine over a chunk of text.
func (p *ThinkParser) processChunk(chunk string) error {
	// Fast path: no '<' in chunk and we're in a stable state.
	if !strings.Contains(chunk, "<") {
		switch p.state {
		case stateContent:
			p.contentTokens += estimateTokens(chunk)
			return p.OnContent(chunk)
		case stateThinking:
			return p.emitThinking(chunk)
		default:
			// stateTagOpen or stateTagClose: we need char-by-char
		}
	}

	for i := 0; i < len(chunk); i++ {
		ch := chunk[i]

		switch p.state {
		case stateContent:
			if ch == '<' {
				p.state = stateTagOpen
				p.tagBuf.Reset()
				p.tagBuf.WriteByte(ch)
			} else {
				// Accumulate content. Find the next '<' for batch emit.
				end := strings.IndexByte(chunk[i:], '<')
				if end == -1 {
					seg := chunk[i:]
					p.contentTokens += estimateTokens(seg)
					if err := p.OnContent(seg); err != nil {
						return err
					}
					return nil
				}
				seg := chunk[i : i+end]
				p.contentTokens += estimateTokens(seg)
				if err := p.OnContent(seg); err != nil {
					return err
				}
				i += end - 1 // loop will increment
			}

		case stateTagOpen:
			p.tagBuf.WriteByte(ch)
			tag := p.tagBuf.String()
			if tag == p.tags.Open {
				// Matched the open tag: enter thinking.
				p.state = stateThinking
				p.tagBuf.Reset()
				p.thinkStart = time.Now()
			} else if len(tag) <= len(p.tags.Open) && p.tags.Open[:len(tag)] == tag {
				// Still a valid prefix of the open tag: keep accumulating.
			} else {
				// Mismatch: emit accumulated tagBuf as content.
				buf := tag
				p.tagBuf.Reset()
				p.state = stateContent
				p.contentTokens += estimateTokens(buf)
				if err := p.OnContent(buf); err != nil {
					return err
				}
				// Re-process current character in stateContent.
				// The character might be '<' starting a new potential tag.
				if ch == '<' {
					p.state = stateTagOpen
					p.tagBuf.WriteByte(ch)
				}
			}

		case stateThinking:
			if ch == '<' {
				p.state = stateTagClose
				p.tagBuf.Reset()
				p.tagBuf.WriteByte(ch)
			} else {
				// Accumulate thinking. Find the next '<' for batch emit.
				end := strings.IndexByte(chunk[i:], '<')
				if end == -1 {
					seg := chunk[i:]
					if err := p.emitThinking(seg); err != nil {
						return err
					}
					return nil
				}
				seg := chunk[i : i+end]
				if err := p.emitThinking(seg); err != nil {
					return err
				}
				i += end - 1 // loop will increment
			}

		case stateTagClose:
			p.tagBuf.WriteByte(ch)
			tag := p.tagBuf.String()
			if tag == p.tags.Close {
				// Matched the close tag: exit thinking.
				if !p.thinkStart.IsZero() {
					p.thinkDuration += time.Since(p.thinkStart)
					p.thinkStart = time.Time{}
				}
				p.state = stateContent
				p.tagBuf.Reset()
			} else if len(tag) <= len(p.tags.Close) && p.tags.Close[:len(tag)] == tag {
				// Still a valid prefix of the close tag: keep accumulating.
			} else {
				// Mismatch: emit accumulated tagBuf as thinking.
				buf := tag
				p.tagBuf.Reset()
				p.state = stateThinking
				if err := p.emitThinking(buf); err != nil {
					return err
				}
				// Re-process current character in stateThinking.
				if ch == '<' {
					p.state = stateTagClose
					p.tagBuf.WriteByte(ch)
				}
			}
		}
	}
	return nil
}

// emitThinking sends thinking content via the callback, applying budget constraints.
func (p *ThinkParser) emitThinking(s string) error {
	tokens := estimateTokens(s)
	p.thinkingTokens += tokens

	if err := p.checkBudget(); err != nil {
		return err
	}

	if p.budgetExceeded {
		return nil // skip the callback but still track tokens
	}

	return p.OnThinking(s)
}

// checkBudget evaluates whether the thinking budget has been exceeded.
func (p *ThinkParser) checkBudget() error {
	if p.budget == nil || p.budgetExceeded {
		return nil
	}

	exceeded := p.budget.MaxTokens > 0 && p.thinkingTokens > p.budget.MaxTokens
	if p.budget.MaxTime > 0 && !p.thinkStart.IsZero() && time.Since(p.thinkStart) > p.budget.MaxTime {
		exceeded = true
	}

	if exceeded {
		p.budgetExceeded = true
		if p.budget.OnExceeded == ThinkBudgetCancel {
			return fmt.Errorf("provider: thinking budget exceeded")
		}
	}
	return nil
}

// Flush emits any remaining buffered content after the stream ends. It must
// be called after the last Process call to ensure all data is emitted.
func (p *ThinkParser) Flush() error {
	// Handle ThinkAuto sniff buffer that never resolved.
	if p.mode == ThinkAuto && !p.autoResolved {
		p.autoResolved = true
		if p.sniffBuf != "" {
			// Check if open tag is in the buffer.
			if strings.Contains(p.sniffBuf, p.tags.Open) {
				p.mode = ThinkAlways
				buf := p.sniffBuf
				p.sniffBuf = ""
				if err := p.processChunk(buf); err != nil {
					return err
				}
				return p.flushTagBuf()
			}
			buf := p.sniffBuf
			p.sniffBuf = ""
			p.contentTokens += estimateTokens(buf)
			return p.OnContent(buf)
		}
		return nil
	}

	return p.flushTagBuf()
}

// flushTagBuf emits any remaining partial tag buffer.
func (p *ThinkParser) flushTagBuf() error {
	// Close any open think duration.
	if !p.thinkStart.IsZero() {
		p.thinkDuration += time.Since(p.thinkStart)
		p.thinkStart = time.Time{}
	}

	if p.tagBuf.Len() == 0 {
		return nil
	}
	buf := p.tagBuf.String()
	p.tagBuf.Reset()

	switch p.state {
	case stateTagOpen:
		// Was trying to match an open tag in content — emit as content.
		p.state = stateContent
		p.contentTokens += estimateTokens(buf)
		return p.OnContent(buf)
	case stateTagClose:
		// Was trying to match a close tag in thinking — emit as thinking.
		p.state = stateThinking
		p.thinkingTokens += estimateTokens(buf)
		if p.budgetExceeded {
			return nil
		}
		return p.OnThinking(buf)
	default:
		// Shouldn't happen, but emit as content to be safe.
		p.contentTokens += estimateTokens(buf)
		return p.OnContent(buf)
	}
}

// Reset prepares the parser for a new response, clearing all accumulated state
// and metrics. The Mode, Tags, and Budget configuration are preserved. The
// OnThinking and OnContent callbacks may be replaced after calling Reset.
func (p *ThinkParser) Reset() {
	p.mode = p.origMode
	p.state = stateContent
	p.tagBuf.Reset()
	p.autoResolved = false
	p.sniffBuf = ""
	p.thinkingTokens = 0
	p.contentTokens = 0
	p.thinkStart = time.Time{}
	p.thinkDuration = 0
	p.budgetExceeded = false
}

// SetActive controls whether a ThinkToggle parser actively parses think tags.
// When active is true, the parser behaves like ThinkAlways (parses tags).
// When active is false, the parser behaves like ThinkNone (passthrough).
//
// This method is only meaningful when the parser's mode is ThinkToggle.
// For other modes it is a no-op. The Provider calls SetActive based on
// whether it injected /think or /no_think into the prompt for this request.
//
// Must be called before Process, typically after Reset for a new request.
func (p *ThinkParser) SetActive(active bool) {
	if p.origMode != ThinkToggle {
		return
	}
	if active {
		p.mode = ThinkAlways
	} else {
		p.mode = ThinkNone
	}
}

// Metrics returns the accumulated thinking and content metrics for the current
// (or most recent) response. Call after Flush for complete results.
func (p *ThinkParser) Metrics() ThinkMetrics {
	total := p.thinkingTokens + p.contentTokens
	var ratio float64
	if total > 0 {
		ratio = float64(p.thinkingTokens) / float64(total)
	}
	return ThinkMetrics{
		ThinkingTokens: p.thinkingTokens,
		ContentTokens:  p.contentTokens,
		ThinkRatio:     ratio,
		ThinkDuration:  p.thinkDuration,
	}
}
