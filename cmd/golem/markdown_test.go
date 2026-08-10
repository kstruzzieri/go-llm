package main

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
)

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return 1, nil
}

var ansiSGR = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func sgrActive(s string) bool {
	active := false
	for _, sgr := range ansiSGR.FindAllString(s, -1) {
		active = sgr != markdownReset
	}
	return active
}

func TestMarkdownWriterStylesStructureWithoutChangingMarkdown(t *testing.T) {
	input := "# Heading\n- item\n12. ordered\nplain *emphasis*\n````go\nfmt.Println(\"x\")\n```\nstill code\n`````\nplain\n"
	want := "\x1b[1m# Heading\x1b[0m\n" +
		"\x1b[36m- \x1b[0mitem\n" +
		"\x1b[36m12. \x1b[0mordered\n" +
		"plain \x1b[3m*emphasis*\x1b[0m\n" +
		"\x1b[2;36m````go\x1b[0m\n" +
		"\x1b[36mfmt.Println(\"x\")\x1b[0m\n" +
		"\x1b[36m```\x1b[0m\n" +
		"\x1b[36mstill code\x1b[0m\n" +
		"\x1b[2;36m`````\x1b[0m\n" +
		"plain\n"

	var out bytes.Buffer
	w := newMarkdownWriter(&out, true)
	if _, err := io.WriteString(w, input); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if err := w.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if got := out.String(); got != want {
		t.Fatalf("rendered output mismatch:\n got: %q\nwant: %q", got, want)
	}
	if got := ansiSGR.ReplaceAllString(out.String(), ""); got != input {
		t.Fatalf("stripping ANSI changed Markdown:\n got: %q\nwant: %q", got, input)
	}
}

func renderMarkdown(t *testing.T, input string, splits ...int) string {
	t.Helper()
	var out bytes.Buffer
	w := newMarkdownWriter(&out, true)
	start := 0
	for _, end := range append(splits, len(input)) {
		if _, err := io.WriteString(w, input[start:end]); err != nil {
			t.Fatalf("WriteString(%d:%d): %v", start, end, err)
		}
		start = end
	}
	if err := w.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return out.String()
}

func TestMarkdownWriterOutputDoesNotDependOnTokenSplits(t *testing.T) {
	input := "## Heading\n1. item with *emphasis*\nmath 2 * 3 * 4\n````go\n```\nfmt.Println(\"λ🙂\")\n`````\nafter\n"
	want := renderMarkdown(t, input)
	for split := 0; split <= len(input); split++ {
		if got := renderMarkdown(t, input, split); got != want {
			t.Fatalf("split at byte %d changed output:\n got: %q\nwant: %q", split, got, want)
		}
	}

	byteSplits := make([]int, 0, len(input)-1)
	for i := 1; i < len(input); i++ {
		byteSplits = append(byteSplits, i)
	}
	if got := renderMarkdown(t, input, byteSplits...); got != want {
		t.Fatalf("byte-at-a-time output changed:\n got: %q\nwant: %q", got, want)
	}
}

func TestMarkdownWriterBoundsAmbiguousPrefixAndResetsBeforeNewline(t *testing.T) {
	digits := strings.Repeat("1", markdownPrefixLimit+32)
	input := digits + "\n````go\nunterminated"
	var out bytes.Buffer
	w := newMarkdownWriter(&out, true)
	// Measured before any newline: an uncapped writer would still be holding
	// every digit here, so this bound fails if the prefix cap is removed.
	if _, err := io.WriteString(w, digits); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	emitted := len(ansiSGR.ReplaceAllString(out.String(), ""))
	if emitted < len(digits)-(markdownPrefixLimit+1) {
		t.Fatalf("writer buffered too much: emitted %d of %d source bytes before any newline", emitted, len(digits))
	}
	if _, err := io.WriteString(w, input[len(digits):]); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if err := w.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	got := out.String()
	if plain := ansiSGR.ReplaceAllString(got, ""); plain != input {
		t.Fatalf("stripping ANSI changed Markdown:\n got: %q\nwant: %q", plain, input)
	}
	for _, line := range strings.SplitAfter(got, "\n") {
		if strings.HasSuffix(line, "\n") && strings.Contains(line, "\x1b[") && !strings.HasSuffix(strings.TrimSuffix(line, "\n"), markdownReset) {
			t.Fatalf("SGR remained active across newline: %q", line)
		}
	}
	if strings.Contains(got, "\x1b[") && !strings.HasSuffix(got, markdownReset) {
		t.Fatalf("Finish left SGR active: %q", got)
	}
}

func TestMarkdownWriterDisabledIsByteForBytePlain(t *testing.T) {
	input := "# Heading\n- item\n```go\ncode\n```\n"
	var out bytes.Buffer
	w := newMarkdownWriter(&out, false)
	if _, err := io.WriteString(w, input); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if err := w.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got := out.String(); got != input {
		t.Fatalf("disabled writer changed bytes: got %q, want %q", got, input)
	}
}

func TestMarkdownWriterReportsRawShortWrites(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		write   func(*markdownWriter) (int, error)
	}{
		{name: "disabled token", write: func(w *markdownWriter) (int, error) {
			return w.Write([]byte("plain"))
		}},
		{name: "terminal chrome", enabled: true, write: func(w *markdownWriter) (int, error) {
			return w.WriteRaw([]byte("chrome"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newMarkdownWriter(shortWriter{}, tc.enabled)
			n, err := tc.write(w)
			if n != 1 || !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("short write = (%d, %v), want (1, io.ErrShortWrite)", n, err)
			}
		})
	}
}

func TestMarkdownWriterPrefixCapAllowsOneDecidingByte(t *testing.T) {
	fence := strings.Repeat("`", markdownPrefixLimit)
	input := fence + "go\ncode\n" + fence + "\nafter\n"
	got := renderMarkdown(t, input)
	if !strings.HasPrefix(got, markdownFence) || !strings.Contains(got, markdownFence+"go"+markdownReset+"\n") {
		t.Fatalf("valid fence at prefix cap was not recognized: %q", got)
	}
	if !strings.Contains(got, markdownAccent+"code"+markdownReset+"\n") {
		t.Fatalf("fenced body was not styled: %q", got)
	}
	if !strings.HasSuffix(got, "after\n") {
		t.Fatalf("cap-sized closing fence did not close: %q", got)
	}
}

func TestMarkdownWriterNeverReturnsWithActiveSGR(t *testing.T) {
	var out bytes.Buffer
	w := newMarkdownWriter(&out, true)
	chunks := []string{"# ", "heading", "\n```", "go\n", "λ", "🙂\n", "```", "\n"}
	for _, chunk := range chunks {
		if _, err := io.WriteString(w, chunk); err != nil {
			t.Fatalf("WriteString(%q): %v", chunk, err)
		}
		if sgrActive(out.String()) {
			t.Fatalf("WriteString(%q) returned with active SGR: %q", chunk, out.String())
		}
	}
	if err := w.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if sgrActive(out.String()) {
		t.Fatalf("Finish returned with active SGR: %q", out.String())
	}
}

func TestMarkdownWriterStylesCompleteInlineEmphasis(t *testing.T) {
	input := "before *emphasis* after\n*start* and unmatched *span\n"
	want := "before \x1b[3m*emphasis*\x1b[0m after\n" +
		"\x1b[3m*start*\x1b[0m and unmatched *span\n"
	if got := renderMarkdown(t, input); got != want {
		t.Fatalf("complete emphasis was not styled line-locally: got %q, want %q", got, want)
	}
}

func TestMarkdownWriterBoundsUnclosedInlineEmphasis(t *testing.T) {
	input := "*" + strings.Repeat("x", 144)
	var out bytes.Buffer
	w := newMarkdownWriter(&out, true)
	if _, err := io.WriteString(w, input); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if emitted := len(ansiSGR.ReplaceAllString(out.String(), "")); emitted < len(input)-128 {
		t.Fatalf("unclosed emphasis buffered too much: emitted %d of %d bytes", emitted, len(input))
	}
	if err := w.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got := out.String(); got != input {
		t.Fatalf("unclosed emphasis must degrade verbatim: got %q, want %q", got, input)
	}
}

func TestMarkdownWriterLeavesOutOfScopeSyntaxPlain(t *testing.T) {
	input := "~~~go\nplain\n~~~\n_emphasis_\n"
	if got := renderMarkdown(t, input); got != input {
		t.Fatalf("unsupported Markdown gained styling: got %q, want %q", got, input)
	}
}

func TestMarkdownWriterFenceCloserMustBeLongEnoughAndHaveNoInfo(t *testing.T) {
	input := "````go\nA\n```\nB\n```` nope\nC\n`````   \nD\n"
	got := renderMarkdown(t, input)
	for _, codeLine := range []string{"A", "```", "B", "```` nope", "C"} {
		if !strings.Contains(got, markdownAccent+codeLine+markdownReset+"\n") {
			t.Fatalf("line %q escaped the longer fence: %q", codeLine, got)
		}
	}
	if !strings.Contains(got, markdownFence+"`````   "+markdownReset+"\nD\n") {
		t.Fatalf("whitespace-only longer closer did not close fence: %q", got)
	}

	got = renderMarkdown(t, "```go\ncode\n```")
	if !strings.HasSuffix(got, markdownFence+"```"+markdownReset) {
		t.Fatalf("closing fence at EOF was not recognized: %q", got)
	}
}

func TestMarkdownWriterFenceCloserAcceptsCRLF(t *testing.T) {
	input := "```go\r\ncode\r\n```\r\nafter\r\n"
	got := renderMarkdown(t, input)
	want := markdownFence + "```\r" + markdownReset + "\n" + "after\r\n"
	if !strings.Contains(got, want) {
		t.Fatalf("CRLF closing fence did not return to prose: got %q, want substring %q", got, want)
	}
}

func TestMarkdownWriterOverCapCloserStillClosesFence(t *testing.T) {
	input := "```go\ncode\n```" + strings.Repeat(" ", 14) + "\nafter\n"
	got := renderMarkdown(t, input)
	if plain := ansiSGR.ReplaceAllString(got, ""); plain != input {
		t.Fatalf("over-cap closer changed source: got %q, want %q", plain, input)
	}
	if !strings.HasSuffix(got, "after\n") {
		t.Fatalf("over-cap closer left following prose in code style: %q", got)
	}
}

func TestMarkdownWriterEmphasisRespectsFlanking(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"space-flanked stars stay plain", "2 * 3 = 6 and 4 * 5 = 20\n", "2 * 3 = 6 and 4 * 5 = 20\n"},
		{"double star stays plain", "**bold** stays plain\n", "**bold** stays plain\n"},
		{"inner spaced star is content", "*a * b* c\n", "\x1b[3m*a * b*\x1b[0m c\n"},
		{"tight pair still styles", "see *this* now\n", "see \x1b[3m*this*\x1b[0m now\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderMarkdown(t, tc.input); got != tc.want {
				t.Fatalf("flanking render mismatch:\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

func TestMarkdownWriterUndersizedOverCapCloserStaysInFence(t *testing.T) {
	fence := strings.Repeat("`", markdownPrefixLimit)
	input := fence + "go\ncode\n" + "   " + strings.Repeat("`", markdownPrefixLimit-2) + "\nstill code\n" + fence + "\nprose\n"
	got := renderMarkdown(t, input)
	if plain := ansiSGR.ReplaceAllString(got, ""); plain != input {
		t.Fatalf("undersized over-cap closer changed source: got %q, want %q", plain, input)
	}
	if !strings.Contains(got, markdownAccent+"still code"+markdownReset+"\n") {
		t.Fatalf("undersized over-cap closer escaped the fence: %q", got)
	}
	if !strings.HasSuffix(got, "prose\n") {
		t.Fatalf("real closer after undersized closer left fence state inverted: %q", got)
	}
}

func TestMarkdownWriterInfoStringBacktickDoesNotOpenFence(t *testing.T) {
	input := "```foo``` is inline code\nplain prose\n"
	got := renderMarkdown(t, input)
	if plain := ansiSGR.ReplaceAllString(got, ""); plain != input {
		t.Fatalf("info-string backtick changed source: got %q, want %q", plain, input)
	}
	if !strings.Contains(got, "\nplain prose\n") {
		t.Fatalf("info-string backtick line opened a fence and styled following prose: %q", got)
	}
}

func TestMarkdownWriterInterleavedStyledWritesKeepByteOrder(t *testing.T) {
	var out bytes.Buffer
	w := newMarkdownWriter(&out, true)
	if _, err := w.Write([]byte("##")); err != nil {
		t.Fatalf("Write prefix: %v", err)
	}
	if _, err := w.WriteStyled("\x1b[2m", []byte("T")); err != nil {
		t.Fatalf("WriteStyled: %v", err)
	}
	if _, err := w.WriteStyled("\x1b[2m", []byte{0xf0, 0x9f}); err != nil {
		t.Fatalf("WriteStyled fragment: %v", err)
	}
	if _, err := w.Write([]byte("x\n")); err != nil {
		t.Fatalf("Write model bytes: %v", err)
	}
	if err := w.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	want := "##T\xf0\x9fx\n"
	if plain := ansiSGR.ReplaceAllString(out.String(), ""); plain != want {
		t.Fatalf("interleaved styled writes reordered or dropped bytes: got %q, want %q", plain, want)
	}
}

func TestMarkdownWriterRawChromeMidLineKeepsFenceBody(t *testing.T) {
	var out bytes.Buffer
	w := newMarkdownWriter(&out, true)
	if _, err := io.WriteString(w, "```go\n"); err != nil {
		t.Fatalf("WriteString fence: %v", err)
	}
	if _, err := w.WriteRaw([]byte("chrome")); err != nil {
		t.Fatalf("WriteRaw: %v", err)
	}
	if _, err := io.WriteString(w, "tail\n```\nafter\n"); err != nil {
		t.Fatalf("WriteString body: %v", err)
	}
	if err := w.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "chrome"+markdownAccent+"tail"+markdownReset+"\n") {
		t.Fatalf("code bytes after mid-line chrome lost fence styling: %q", got)
	}
	if !strings.HasSuffix(got, "after\n") {
		t.Fatalf("fence did not close after mid-line chrome: %q", got)
	}
}

func TestMarkdownWriterOverCapInvalidCloserStaysInFence(t *testing.T) {
	input := "```go\ncode\n```" + strings.Repeat(" ", 14) + "nope\nstill code\n```\nafter\n"
	got := renderMarkdown(t, input)
	if plain := ansiSGR.ReplaceAllString(got, ""); plain != input {
		t.Fatalf("over-cap invalid closer changed source: got %q, want %q", plain, input)
	}
	if !strings.Contains(got, markdownAccent+"still code"+markdownReset+"\n") {
		t.Fatalf("invalid over-cap closer escaped the fence: %q", got)
	}
	if !strings.HasSuffix(got, "after\n") {
		t.Fatalf("later valid closer did not return to prose: %q", got)
	}
}
