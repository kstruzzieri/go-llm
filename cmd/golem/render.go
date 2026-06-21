package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
)

// renderer is an append-only agent.Observer for a terminal. It streams tokens,
// prints a one-line tool-call notice, a dim per-step footer, and (via
// finalFooter) a dim summary after Run. It deliberately omits a tool-RESULT
// line: that requires the agent.ToolResultObserver seam, added in B3.
type renderer struct {
	out      io.Writer
	color    bool
	maxSteps int
	now      func() time.Time
	lastMark time.Time
	runStart time.Time
	lastNL   bool
}

func newRenderer(out io.Writer, color bool, maxSteps int, now func() time.Time) *renderer {
	if now == nil {
		now = time.Now
	}
	start := now()
	return &renderer{out: out, color: color, maxSteps: maxSteps, now: now, lastMark: start, runStart: start, lastNL: true}
}

func (r *renderer) OnToken(_ context.Context, e agent.TokenEvent) error {
	_, err := io.WriteString(r.out, e.Content)
	if err == nil && e.Content != "" {
		r.lastNL = strings.HasSuffix(e.Content, "\n")
	}
	return err
}

func (r *renderer) OnToolCall(_ context.Context, e agent.ToolCallEvent) error {
	// Some tools take no arguments; omit the trailing space when args are empty
	// so the line reads "> name" rather than "> name ".
	if args := string(e.Call.Function.Arguments); args != "" {
		_, err := fmt.Fprintf(r.out, "\n> %s %s\n", e.Call.Function.Name, args)
		if err == nil {
			r.lastNL = true
		}
		return err
	}
	_, err := fmt.Fprintf(r.out, "\n> %s\n", e.Call.Function.Name)
	if err == nil {
		r.lastNL = true
	}
	return err
}

func (r *renderer) OnStep(_ context.Context, e agent.StepEvent) error {
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

func (r *renderer) finalFooter(res agent.Result) {
	total := r.now().Sub(r.runStart).Seconds()
	line := fmt.Sprintf("done · %d steps · %.1fs · %d tok",
		len(res.Steps), total, res.Usage.TotalTokens)
	if res.StopReason != agent.Completed {
		// Double space (not " · ") deliberately sets the stop reason apart from
		// the metric fields above it — it is a status suffix, not another metric.
		line += "  stopped: " + res.StopReason.String()
	}
	_ = r.writeDim(line)
}

func (r *renderer) writeDim(line string) error {
	if !r.lastNL {
		if _, err := io.WriteString(r.out, "\n"); err != nil {
			return err
		}
	}
	if r.color {
		line = "\x1b[2m" + line + "\x1b[0m"
	}
	_, err := io.WriteString(r.out, line+"\n")
	if err == nil {
		r.lastNL = true
	}
	return err
}

// OnToolResult prints a quiet, display-only "< summary" line that pairs with the
// "> call" line from OnToolCall. Pre-invocation synthetic failures (unknown
// tool, malformed args, denied) may appear result-only with no preceding call.
// The summary is never persisted and never fed back to the model.
func (r *renderer) OnToolResult(_ context.Context, e agent.ToolResultEvent) error {
	if !r.lastNL {
		if _, err := io.WriteString(r.out, "\n"); err != nil {
			return err
		}
	}
	_, err := io.WriteString(r.out, "< "+resultSummary(e)+"\n")
	if err == nil {
		r.lastNL = true
	}
	return err
}

// resultSummary derives a terse one-line summary from the result. A tool-set
// Preview wins; otherwise the summary is keyed on the tool name. Display-only.
func resultSummary(e agent.ToolResultEvent) string {
	res := e.Result
	if res.Preview != "" {
		return res.Preview
	}
	if res.IsError {
		return "error: " + firstLine(res.Content)
	}
	suffix := ""
	if res.Truncated {
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
