package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/agent"
)

// renderer is an append-only agent.Observer for a terminal. It streams tokens,
// prints one-line tool-call and tool-result notices, a dim per-step footer, and
// (via finalFooter) a dim summary after Run.
type renderer struct {
	markdown     *markdownWriter
	color        bool
	terminal     bool
	maxSteps     int
	now          func() time.Time
	lastMark     time.Time
	runStart     time.Time
	warnPressure bool   // print a one-line context-pressure warning
	warned       bool   // ensures at most one pressure line per run
	thinkOpen    bool   // "[thinking]" header printed for the current step
	pending      []byte // incomplete UTF-8 rune from terminal-bound model content
	pendingThink bool
	// mixed mirrors ContextManager.Mixed for the run this renderer observes. It
	// is a constructor PARAMETER rather than a settable field like warnPressure
	// because forgetting it at a new call site would not fail loudly — it would
	// silently restore the wrong "(truncated)" marker resultSummary exists to
	// suppress.
	mixed bool
}

func newRenderer(out io.Writer, color bool, maxSteps int, now func() time.Time, mixed bool) *renderer {
	if now == nil {
		now = time.Now
	}
	start := now()
	return &renderer{markdown: newMarkdownWriter(out, color), color: color, terminal: isTerminalOutput(out), maxSteps: maxSteps, now: now, lastMark: start, runStart: start, mixed: mixed}
}

func isTerminalOutput(out io.Writer) bool {
	switch w := out.(type) {
	case *os.File:
		return realTermOps{}.IsTerminal(int(w.Fd()))
	case *synchronizedWriter:
		return w.terminal
	default:
		return false
	}
}

func (r *renderer) OnToken(_ context.Context, e agent.TokenEvent) error {
	if r.thinkOpen {
		r.thinkOpen = false
		if err := r.breakLine(); err != nil {
			return err
		}
	}
	return r.streamContent(e.Content, false)
}

func (r *renderer) OnToolCall(_ context.Context, e agent.ToolCallEvent) error {
	// Some tools take no arguments; omit the trailing space when args are empty
	// so the line reads "> name" rather than "> name ".
	line := "\n> " + e.Call.Function.Name
	if args := string(e.Call.Function.Arguments); args != "" {
		line += " " + args
	}
	line += "\n"
	if r.terminal {
		line = sanitizeApprovalPreview(line)
	}
	return r.writeRaw(line)
}

// OnThinking streams reasoning deltas dim, under a one-per-step "[thinking]"
// header, keeping them visually distinct from the answer. The header is plain
// text so non-TTY logs stay parseable.
func (r *renderer) OnThinking(_ context.Context, e agent.ThinkingEvent) error {
	if !r.thinkOpen {
		if err := r.breakLine(); err != nil {
			return err
		}
		if err := r.writeRaw(r.dim("[thinking]") + "\n"); err != nil {
			return err
		}
		r.thinkOpen = true
	}
	return r.streamContent(e.Content, true)
}

func (r *renderer) OnStep(_ context.Context, e agent.StepEvent) error {
	r.thinkOpen = false
	t := r.now()
	lat := t.Sub(r.lastMark).Seconds()
	r.lastMark = t
	model := "?"
	if e.RouteOutcome != nil {
		model = e.RouteOutcome.ActualModel.String()
	}
	line := fmt.Sprintf("%s · %.1fs · ctx %.0f%% · step %d/%d",
		model, lat, e.Pressure.UsedPct*100, e.Index+1, r.maxSteps)
	return r.writeDim(line)
}

func (r *renderer) finalFooter(res agent.Result, elapsed time.Duration) {
	_ = r.flushPending()
	_ = r.markdown.Finish()
	line := fmt.Sprintf("done · %s · %.1fs · %d tok",
		plural(len(res.Steps), "step", "steps"), elapsed.Seconds(), res.Usage.TotalTokens)
	if res.Risk != nil {
		// #514 D7: successful REPL and -p turns expose their cumulative score.
		line += fmt.Sprintf(" · risk %d", res.Risk.Score)
	}
	if res.StopReason != agent.Completed {
		// Double space (not " · ") deliberately sets the stop reason apart from
		// the metric fields above it — it is a status suffix, not another metric.
		line += "  stopped: " + res.StopReason.String()
	}
	_ = r.writeDim(line)
}

func (r *renderer) writeDim(line string) error {
	if err := r.breakLine(); err != nil {
		return err
	}
	if r.terminal {
		line = sanitizeApprovalPreview(line)
	}
	return r.writeRaw(r.dim(line) + "\n")
}

func (r *renderer) writeRaw(s string) error {
	if err := r.flushPending(); err != nil {
		return err
	}
	_, err := r.markdown.WriteRaw([]byte(s))
	return err
}

func (r *renderer) writeDimContent(s string) error {
	if !r.color {
		_, err := r.markdown.WriteRaw([]byte(s))
		return err
	}
	_, err := r.markdown.WriteStyled("\x1b[2m", []byte(s))
	return err
}

func (r *renderer) breakLine() error {
	if err := r.flushPending(); err != nil {
		return err
	}
	return r.markdown.BreakLine()
}

func (r *renderer) rawWriter() io.Writer { return markdownRawWriter{markdown: r.markdown} }

func (r *renderer) finish() error {
	r.thinkOpen = false
	if err := r.flushPending(); err != nil {
		return err
	}
	return r.markdown.Finish()
}

func (r *renderer) streamContent(s string, thinking bool) error {
	if !r.terminal {
		return r.writeContent(s, thinking)
	}
	data := append(r.pending, s...)
	complete := 0
	for complete < len(data) && utf8.FullRune(data[complete:]) {
		_, size := utf8.DecodeRune(data[complete:])
		complete += size
	}
	safe := sanitizeApprovalPreview(string(data[:complete]))
	r.pending = append(r.pending[:0], data[complete:]...)
	r.pendingThink = thinking
	if complete == 0 {
		return nil
	}
	return r.writeContent(safe, thinking)
}

func (r *renderer) flushPending() error {
	if len(r.pending) == 0 {
		return nil
	}
	text := sanitizeApprovalPreview(string(r.pending))
	r.pending = nil
	return r.writeContent(text, r.pendingThink)
}

func (r *renderer) writeContent(s string, thinking bool) error {
	if thinking {
		return r.writeDimContent(s)
	}
	_, err := io.WriteString(r.markdown, s)
	return err
}

// dim wraps s in the SGR faint sequence when color is on; otherwise s as-is.
func (r *renderer) dim(s string) string {
	if r.color {
		return "\x1b[2m" + s + "\x1b[0m"
	}
	return s
}

// OnToolResult prints a quiet, display-only "< summary" line that pairs with the
// "> call" line from OnToolCall. Pre-invocation synthetic failures (unknown
// tool, malformed args, plan failure, denied) may appear result-only with no preceding call.
// The summary is never persisted and never fed back to the model.
func (r *renderer) OnToolResult(_ context.Context, e agent.ToolResultEvent) error {
	if err := r.breakLine(); err != nil {
		return err
	}
	line := "< " + r.resultSummary(e) + "\n"
	if r.terminal {
		line = sanitizeApprovalPreview(line)
	}
	return r.writeRaw(line)
}

// OnPressure prints a single dim context-pressure warning per run when warnings
// are enabled and the level reaches warn. Telemetry records every event; the
// terminal is deduped to avoid spam.
func (r *renderer) OnPressure(_ context.Context, e agent.PressureEvent) error {
	if !r.warnPressure || r.warned || e.Pressure.Level < agent.LevelWarn {
		return nil
	}
	r.warned = true
	return r.writeDim(fmt.Sprintf("context pressure: %s · ctx %.0f%% · cause %s",
		e.Pressure.Level, e.Pressure.UsedPct*100, e.Pressure.Cause))
}

// resultSummary derives a terse one-line summary from the result. A tool-set
// Preview wins; otherwise the summary is keyed on the tool name. Display-only.
//
// ToolResult.Truncated describes ToolResult.Content, which capOutput trimmed to
// the tool's output cap. Under MIXED assembly that string is not what the model
// reads: the assembler replaces the anchor's content with the alternatives it
// selected from ToolResult.Context, so a "(truncated)" marker would describe a
// rendering that was discarded. It cannot be recomputed here either — assembly
// runs before the NEXT step's model call, against a global budget this event
// predates. So it is suppressed, and only for results that actually carry a
// structured set: a plain tool under mixed keeps its flat content verbatim and
// its truncation is real. What replaces the signal under mixed is
// Pressure.AnchorOmissions and the telemetry context_assembly span, both of
// which describe what the model was actually given.
func (r *renderer) resultSummary(e agent.ToolResultEvent) string {
	res := e.Result
	if res.Preview != "" {
		return res.Preview
	}
	if res.IsError {
		return "error: " + firstLine(res.Content)
	}
	flatDiscarded := r.mixed && res.Context != nil
	suffix := ""
	if res.Truncated && !flatDiscarded {
		suffix = " (truncated)"
	}
	switch e.Call.Function.Name {
	case "search":
		return searchSummary(res.Content) + suffix
	case "glob", "list":
		return entriesSummary(res.Content) + suffix
	case "retrieve":
		if res.Attrib != nil {
			return plural(len(res.Attrib.Sources), "source", "sources") + suffix
		}
		return plural(countLines(res.Content), "line", "lines") + suffix
	default: // read_file and any unknown tool
		return plural(countLines(res.Content), "line", "lines") + suffix
	}
}

// searchSummary parses the "path:line: text" output of the search tool. The
// trailing "[truncated ...]" marker and the "no matches" sentinel are handled
// without counting them as matches.
func searchSummary(content string) string {
	if content == "" || content == "no matches" {
		return "no matches"
	}
	matches := 0
	files := map[string]struct{}{}
	for _, line := range strings.Split(content, "\n") {
		if line == "" || strings.HasPrefix(line, "[") {
			continue
		}
		matches++
		if i := strings.IndexByte(line, ':'); i >= 0 {
			files[line[:i]] = struct{}{}
		}
	}
	if matches == 0 {
		return "no matches"
	}
	return plural(matches, "match", "matches") + " in " + plural(len(files), "file", "files")
}

// entriesSummary counts the glob/list entry lines, skipping the sentinel and the
// truncation marker.
func entriesSummary(content string) string {
	if content == "" || content == "no entries" {
		return "no entries"
	}
	n := 0
	for _, line := range strings.Split(content, "\n") {
		if line == "" || strings.HasPrefix(line, "[") {
			continue
		}
		n++
	}
	if n == 0 {
		return "no entries"
	}
	return plural(n, "entry", "entries")
}

// countLines counts the lines in content, ignoring trailing newlines.
func countLines(content string) int {
	if content == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(content, "\n"), "\n") + 1
}

// firstLine returns the first line of s, capped to 100 runes for the result line.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	rs := []rune(s)
	if len(rs) > 100 {
		return string(rs[:100]) + "…"
	}
	return s
}

// plural renders "<n> <singular|plural>" choosing the form by count.
func plural(n int, singular, pluralForm string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, pluralForm)
}
