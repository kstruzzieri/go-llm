package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kstruzzieri/go-llm/internal/opsview"
	"github.com/kstruzzieri/go-llm/internal/promptfence"
)

// opsCell neutralizes untrusted text for one table cell: controls are quoted
// (sanitizeApprovalPreview) and line breaks flattened (FlattenLine), so a
// hostile model name can neither drive the terminal nor forge a row.
func opsCell(s string) string {
	return promptfence.FlattenLine(sanitizeApprovalPreview(s))
}

func partText(p opsPart) string {
	s := p.State
	if p.Detail != "" {
		s += ": " + p.Detail
	}
	if p.AgeMs != nil {
		s += " (" + fmtAge(*p.AgeMs) + ")"
	}
	return s
}

// renderOpsTable writes the human view. elapsed advances ages since the
// snapshot was built; width > 0 clips every line to that many columns.
func renderOpsTable(w io.Writer, s opsview.Snapshot, elapsed time.Duration, width int) error {
	generatedAt, _ := time.Parse(time.RFC3339, s.GeneratedAt)
	withLoads := s.Mode != opsview.ModeOnce
	var buf bytes.Buffer
	rev := "-"
	if s.Config.Revision != nil {
		rev = *s.Config.Revision
		if len(rev) > 8 {
			rev = rev[:8]
		}
	}
	_, _ = fmt.Fprintf(&buf, "GOLEM OPS  mode %s  config %s rev %s  attention %d\n", opsCell(string(s.Mode)), opsCell(s.Config.Source), opsCell(rev), len(s.Attention))

	tw := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "\nBACKENDS\t\t")
	for _, b := range s.Backends {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", opsCell(b.ID), opsCell(b.Hosting), opsCell(backendText(b, elapsed)))
	}
	header := "\nMODELS\tRESIDENCY\tACTIVITY\tUSED BY\tLAST 1H"
	if withLoads {
		header += "\tLOADS"
	}
	_, _ = fmt.Fprintln(tw, header)
	for _, m := range s.Models {
		cells := []string{
			opsCell(m.ID), opsCell(partText(residencyPart(m, generatedAt, elapsed))),
			opsCell(partText(activityPart(m, elapsed))), opsCell(usedByText(m)), opsCell(statsText(m, "1h")),
		}
		if withLoads {
			cells = append(cells, opsCell(loadsText(m)))
		}
		_, _ = fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(&buf, "\nATTENTION")
	if len(s.Attention) == 0 {
		_, _ = fmt.Fprintln(&buf, "  none")
	}
	for _, a := range s.Attention {
		_, _ = fmt.Fprintf(&buf, "  [%s] %s\n", opsCell(a.Severity), opsCell(a.Text))
	}
	_, _ = fmt.Fprintf(&buf, "\nCOVERAGE\n  %s\n", opsCoverageNote)

	text := buf.String()
	if width > 0 {
		text = clipToColumns(text, width)
	}
	_, err := io.WriteString(w, text)
	return err
}

// runeCols counts terminal columns conservatively: runes from U+1100 up count
// as two, so wide scripts can only make a clipped line shorter than width.
// Zero-width runes that survive opsCell (combining marks) count as one, which
// also errs short.
// ponytail: conservative, not exact East Asian Width; exact widths need
// golang.org/x/text, which this module does not allow.
func runeCols(r rune) int {
	if r >= 0x1100 {
		return 2
	}
	return 1
}

func displayCols(s string) int {
	n := 0
	for _, r := range s {
		n += runeCols(r)
	}
	return n
}

// clipToColumns truncates every line to width columns, ending a cut line
// with "»" (below U+1100, so it measures and renders as one column).
func clipToColumns(text string, width int) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if displayCols(line) <= width {
			continue
		}
		var b strings.Builder
		cols := 0
		for _, r := range line {
			c := runeCols(r)
			if cols+c > width-1 {
				break
			}
			b.WriteRune(r)
			cols += c
		}
		lines[i] = b.String() + "»" // U+00BB: one column everywhere
	}
	return strings.Join(lines, "\n")
}
