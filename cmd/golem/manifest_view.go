package main

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

// embeddingUseCase is the routing use case for embedding (indexing and
// retrieval) calls.
const embeddingUseCase = "embedding"

// purposeInfo is one row of the purpose table.
type purposeInfo struct {
	label    string
	metadata bool
}

// purposeTable maps a manifest purpose to its plain-language label and
// whether it is a metadata purpose (no user content is sent).
var purposeTable = map[string]purposeInfo{
	modelSetUseCase:                            {"chat", false},
	delegateUseCase:                            {"code delegation", false},
	config.UseCasePlanning:                     {"planning", false},
	config.UseCaseSummarize:                    {"summaries", false},
	embeddingUseCase:                           {"embeddings", false},
	config.UseCaseRoute:                        {"routing", false},
	config.UseCaseRerank:                       {"reranking", false},
	config.UseCaseVerify:                       {"verification", false},
	config.UseCaseExtract:                      {"extraction", false},
	config.UseCaseApproval:                     {"approval checks", false},
	config.UseCaseVision:                       {"vision", false},
	"fim":                                      {"code completion", false},
	provider.DestinationPurposeModelRefresh:    {"model list", true},
	provider.DestinationPurposeCapabilityProbe: {"capabilities", true},
	provider.DestinationPurposeSlotProbe:       {"slot status", true},
	provider.DestinationPurposeDiscovery:       {"server discovery", true},
	provider.DestinationPurposeHealth:          {"health checks", true},
	provider.DestinationPurposeWarmthPoll:      {"warm-up status", true},
}

// purposeLabel returns the display label for a manifest purpose and whether
// it is a metadata purpose. An unknown purpose is shown as "other: " and the
// id through destinationName, so a raw "chat" stays distinct from the "agent"
// label "chat", and an id that is not bare is quoted: the renderer's
// " (fallback)" lands outside the quotes, so a primary "custom (fallback)"
// cannot read as a fallback "custom".
func purposeLabel(purpose string) (label string, metadata bool) {
	if p, ok := purposeTable[purpose]; ok {
		return p.label, p.metadata
	}
	return "other: " + destinationName(purpose), false
}

// destinationName is the one identity formatter for a provider name (row,
// question and pin receipt) and an unknown purpose id. A name of only
// [A-Za-z0-9._-] stays bare; any other is quoted with strconv.QuoteToASCII so
// it cannot run into its neighbours or carry a control or look-alike
// character, and each ASCII space in it is then written \x20. QuoteToASCII
// keeps spaces, so a long quoted name padded with them could soft-wrap a
// forged state label (say "allowed by -allow-destination") onto a physical
// line of its own. The escape is unambiguous: a literal backslash is already
// \\, and nothing else in the quoted form is a space.
func destinationName(name string) string {
	bare := name != ""
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			bare = false
			break
		}
	}
	if bare {
		return name
	}
	return strings.ReplaceAll(strconv.QuoteToASCII(name), " ", `\x20`)
}

// destinationURL formats a canonical Destination URL (row and pin receipt); it
// does no redaction, so it is for canonical destination URLs only, never a raw
// base_url. The URL is an identity, like the provider name, so it is shown as exact code
// points: every non-ASCII rune is escaped with strconv.QuoteToASCII's
// convention, minus the quotes. Escaping only blank-rendering runes would mean
// enumerating them (U+3000 and U+00A0 are graphic, and so is U+3164), and a
// missed one lets a hostile base_url draw a fake state on its own row.
func destinationURL(d provider.Destination) string {
	q := strconv.QuoteToASCII(d.BaseURL())
	return q[1 : len(q)-1]
}

// remoteState is the consent state of one remote destination.
type remoteState int

const (
	// remoteNeedsOK is a remote the user is about to be asked about.
	remoteNeedsOK remoteState = iota
	// remoteAllowedByFlag is a remote granted by -allow-destination.
	remoteAllowedByFlag
	// remoteAllowedEarlier is a remote granted earlier this session.
	remoteAllowedEarlier
	// remoteNotAllowed is a remote not granted, listed outside a refusal.
	remoteNotAllowed
	// remoteRefused is a remote refused because it cannot be asked about.
	remoteRefused
)

// text is the user-facing copy for the state.
func (s remoteState) text() string {
	switch s {
	case remoteNeedsOK:
		return "needs your OK"
	case remoteAllowedByFlag:
		return "allowed by -allow-destination"
	case remoteAllowedEarlier:
		return "allowed earlier this session"
	case remoteNotAllowed:
		return "not allowed (cleared)"
	case remoteRefused:
		return "not allowed (needs -allow-destination)"
	default:
		// Unknown values read as the safe, refusing copy.
		return "not allowed (needs -allow-destination)"
	}
}

// renderMode says what the manifest rendering is for.
type renderMode int

const (
	// renderAsking precedes a consent question.
	renderAsking renderMode = iota
	// renderRefusing accompanies a refusal.
	renderRefusing
	// renderListing is a plain listing (for example /grants).
	renderListing
)

// remoteStates classifies every remote destination in the manifest. Local
// destinations are absent from the result.
func remoteStates(m *provider.DestinationManifest, flags, baseline provider.DestinationPolicy, mode renderMode) map[provider.Destination]remoteState {
	out := map[provider.Destination]remoteState{}
	for _, d := range m.Destinations() {
		if d.IsLocal() {
			continue
		}
		// Coverage is decided by the baseline alone, so the "needs your OK"
		// rows are exactly uncoveredAll(dests, baseline), the set the
		// question names, whatever the flags hold.
		switch {
		case !baseline.Permits(d):
			switch mode {
			case renderAsking:
				out[d] = remoteNeedsOK
			case renderRefusing:
				out[d] = remoteRefused
			default:
				out[d] = remoteNotAllowed
			}
		case flags.Permits(d):
			out[d] = remoteAllowedByFlag
		default:
			out[d] = remoteAllowedEarlier
		}
	}
	return out
}

// uncoveredAll returns every destination p does not permit, in dests order.
func uncoveredAll(dests []provider.Destination, p provider.DestinationPolicy) []provider.Destination {
	var out []provider.Destination
	for _, d := range dests {
		if !p.Permits(d) {
			out = append(out, d)
		}
	}
	return out
}

// manifestOpts frames one rendering of the model-server section.
type manifestOpts struct {
	// style is the color and width of the writer rendered to.
	style consoleStyle
	// operation is "" for the session view, or the subcommand ("models",
	// "index", "source") the section describes.
	operation string
	// cleared marks a listing after /grants clear.
	cleared bool
}

// clearedNote heads a /grants listing after /grants clear.
const clearedNote = "access cleared by /grants clear; the next admission asks about every remote -allow-destination does not cover"

// manifestNameMax caps the shared name column; a longer name sits alone on
// its header line.
const manifestNameMax = 16

// renderManifest writes the model-server section for m: a rule, then the
// servers on this machine and the remote ones, one block per destination with
// its name, URL, consent state (remotes only), the purposes it serves, the
// metadata checks it gets, and where its key comes from. Untrusted fields go
// through displayField, destinationName and destinationURL; layout is measured on
// the uncolored text. states must hold every remote: a missing entry panics
// rather than defaulting to a state. An empty manifest writes nothing. The
// section is written in one call, and its error is returned so a caller about
// to ask the consent question can refuse instead.
func renderManifest(w io.Writer, m *provider.DestinationManifest, keySources map[string]string, states map[provider.Destination]remoteState, opt manifestOpts) error {
	dests := m.Destinations()
	if len(dests) == 0 {
		return nil
	}
	sort.SliceStable(dests, func(i, j int) bool {
		if dests[i].IsLocal() != dests[j].IsLocal() {
			return dests[i].IsLocal()
		}
		return dests[i].String() < dests[j].String()
	})
	// Labels in manifest order; never deduplicated after translation, so a
	// raw "chat" purpose stays distinct from the "agent" label "chat".
	uses := map[provider.Destination][]string{}
	checks := map[provider.Destination][]string{}
	for _, e := range m.Edges() {
		label, metadata := purposeLabel(e.Purpose)
		if e.IsFallback {
			label += " (fallback)"
		}
		if metadata {
			checks[e.Destination] = append(checks[e.Destination], label)
		} else {
			uses[e.Destination] = append(uses[e.Destination], label)
		}
	}
	nameCol := 0
	for _, d := range dests {
		nameCol = max(nameCol, displayCols(destinationName(d.Provider())))
	}
	nameCol = min(manifestNameMax, nameCol)
	indent := strings.Repeat(" ", 2+nameCol+2)

	s := opt.style
	// Pull a row left only as far as needed to keep its content intact.
	fitIndent := func(cols int) string {
		return indent[:min(len(indent), max(0, s.width-cols))]
	}
	var b strings.Builder
	line := func(text string) {
		b.WriteString(text)
		b.WriteByte('\n')
	}
	// row wraps the uncolored items, then paints only the label.
	row := func(label string, items []string) {
		if len(items) == 0 {
			return
		}
		widest := 0
		for i, item := range items {
			cols := displayCols(item)
			if i+1 < len(items) {
				cols++ // comma
			}
			widest = max(widest, cols)
		}
		labelWidth := displayCols(label)
		pad := fitIndent(labelWidth + widest)
		if labelWidth+widest > s.width {
			line(indent + s.paint(sgrCyan, strings.TrimSpace(label)))
			for _, l := range wrapList(items, fitIndent(widest), fitIndent(widest), s.width) {
				line(l)
			}
			return
		}
		for i, l := range wrapList(items, pad+label, pad+strings.Repeat(" ", labelWidth), s.width) {
			if i == 0 {
				word := strings.TrimRight(label, " ")
				l = pad + s.paint(sgrCyan, word) + l[len(pad)+len(word):]
			}
			line(l)
		}
	}

	sub := "where this session's model requests can go"
	usesLabel := "used for  "
	switch opt.operation {
	case "":
	case "models":
		sub = "servers golem models lists and checks"
		usesLabel = "in route  "
	default:
		sub = "where golem " + opt.operation + " sends model requests"
	}
	line(s.rule("model servers", sub))
	if opt.cleared {
		// Word-wrapped, so a terminal never soft-wraps a fragment of it onto
		// a row of its own where it could read like a state.
		for _, l := range wrapWords(clearedNote, s.width) {
			line(s.paint(sgrDim, l))
		}
	}

	asking := false
	for i, d := range dests {
		if i == 0 && d.IsLocal() {
			line(s.paint(sgrGreen, "On this machine") + " " + s.paint(sgrDim, "(allowed automatically)"))
		}
		if !d.IsLocal() && (i == 0 || dests[i-1].IsLocal()) {
			line(s.paint(sgrYellowBold, "Remote"))
		}
		// The URL is never wrapped or cut: it is the identity being granted.
		name, url := destinationName(d.Provider()), destinationURL(d)
		head := "  " + name + strings.Repeat(" ", max(0, nameCol-displayCols(name))) + "  " + url
		if displayCols(name) > nameCol || displayCols(head) > s.width {
			line(strings.Repeat(" ", min(2, max(0, s.width-displayCols(name)))) + name)
			head = fitIndent(displayCols(url)) + url
		}
		if d.IsLocal() {
			line(head)
		} else {
			st, ok := states[d]
			if !ok {
				panic(fmt.Sprintf("golem: renderManifest: no consent state for remote %q", d.String()))
			}
			asking = asking || st == remoteNeedsOK
			sgr := sgrYellow
			if st == remoteAllowedByFlag || st == remoteAllowedEarlier {
				sgr = sgrDim
			}
			if text := st.text(); displayCols(head)+2+displayCols(text) > s.width {
				line(head)
				line(fitIndent(displayCols(text)) + s.paint(sgr, text))
			} else {
				line(head + "  " + s.paint(sgr, text))
			}
		}
		row(usesLabel, uses[d])
		row("checks    ", checks[d])
		if k, ok := keySources[d.Provider()]; ok {
			text := displayField(k)
			line(fitIndent(displayCols(text)) + s.paint(sgrDim, text))
		}
	}
	if asking {
		b.WriteByte('\n')
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// consentQuestion is the one-line session consent prompt for the uncovered
// remotes, sorted by String() (the row order). uncovered must be non-empty.
// It carries no SGR.
func consentQuestion(uncovered []provider.Destination) string {
	sorted := append([]provider.Destination(nil), uncovered...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].String() < sorted[j].String() })
	names := make([]string, len(sorted))
	for i, d := range sorted {
		names[i] = destinationName(d.Provider())
	}
	var list string
	if n := len(names); n > 1 {
		list = strings.Join(names[:n-1], ", ") + " and " + names[n-1]
	} else {
		list = names[0]
	}
	return "Send data to " + list + " (remote) during this session? [y/N] "
}
