package main

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/kstruzzieri/go-llm/internal/opsview"
	"github.com/kstruzzieri/go-llm/internal/promptfence"
)

// opsCellCols bounds every cell. tabwriter pads each column to its longest
// cell, so one unbounded name would otherwise be copied into every row.
const opsCellCols = 256

// opsCell neutralizes untrusted text for one table cell: combining-mark
// floods and controls are quoted (sanitizeApprovalPreview), line breaks
// flattened (FlattenLine), and the result clipped to opsCellCols, so a
// hostile model name can neither drive the terminal, forge a row, nor
// inflate the table.
func opsCell(s string) string {
	return clipCols(promptfence.FlattenLine(sanitizeApprovalPreview(quoteMarkFloods(s))), opsCellCols)
}

// quoteMarkFloods quotes combining marks past the second on one base. Marks
// are graphic, so sanitizeApprovalPreview keeps them, and a stack of them
// draws over the rows above and below.
func quoteMarkFloods(s string) string {
	var b strings.Builder
	run := 0
	for _, r := range s {
		run++
		if !unicode.In(r, unicode.Mn, unicode.Me) {
			run = 0
		}
		if run > 2 {
			q := strconv.QuoteToASCII(string(r))
			b.WriteString(q[1 : len(q)-1])
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
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
	// On a terminal one long ID would widen its column until every row's
	// facts fall past the clip, so IDs get a third of the width.
	idCell := opsCell
	if width > 0 {
		idCell = func(id string) string { return clipCols(opsCell(id), max(24, width/3)) }
	}
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
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", idCell(b.ID), opsCell(b.Hosting), opsCell(backendText(b, elapsed, withLoads)))
	}
	header := "\nMODELS\tRESIDENCY\tACTIVITY\tUSED BY\tLAST 1H"
	if withLoads {
		header += "\tLOADS"
	}
	_, _ = fmt.Fprintln(tw, header)
	for _, m := range s.Models {
		cells := []string{
			idCell(m.ID), opsCell(partText(residencyPart(m, generatedAt, elapsed))),
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

// runeCols counts terminal columns conservatively: every non-ASCII rune but
// the clip marker counts as two, so wide scripts and East Asian Ambiguous
// runes (Cyrillic, Greek, accented Latin, drawn wide by CJK-configured
// terminals) can only make a clipped line shorter than width, never wrap it
// and start a visual line with untrusted text. Zero-width runes that survive
// opsCell (combining marks) also err short. "»" is East Asian Neutral: one
// column everywhere.
// ponytail: conservative, not exact East Asian Width; exact widths need
// golang.org/x/text, which this module does not allow.
func runeCols(r rune) int {
	if r >= 0x80 && r != '»' {
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

// clipCols truncates s to n columns, ending a cut with "»".
func clipCols(s string, n int) string {
	if displayCols(s) <= n {
		return s
	}
	var b strings.Builder
	cols := 0
	for _, r := range s {
		c := runeCols(r)
		if cols+c > n-1 {
			break
		}
		b.WriteRune(r)
		cols += c
	}
	return b.String() + "»" // U+00BB: one column everywhere
}

// clipToColumns truncates every line to width columns.
func clipToColumns(text string, width int) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = clipCols(line, width)
	}
	return strings.Join(lines, "\n")
}
