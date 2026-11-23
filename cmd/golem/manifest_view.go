package main

import (
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
// it is a metadata purpose. An unknown purpose is shown as "other: <purpose>"
// so a raw "chat" stays distinct from the "agent" label "chat".
func purposeLabel(purpose string) (label string, metadata bool) {
	if p, ok := purposeTable[purpose]; ok {
		return p.label, p.metadata
	}
	return "other: " + displayField(purpose), false
}

// destinationName is the one identity formatter for a provider name (row,
// question and pin receipt). A name of only [A-Za-z0-9._-] stays bare; any
// other is quoted with strconv.QuoteToASCII so it cannot run into its
// neighbours or carry a control or look-alike character.
func destinationName(name string) string {
	bare := name != ""
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			bare = false
			break
		}
	}
	if bare {
		return name
	}
	return strconv.QuoteToASCII(name)
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
		return "not allowed (cleared; the next goal asks again)"
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
	// renderListing is a plain listing (for example /model servers).
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
		switch {
		case flags.Permits(d):
			out[d] = remoteAllowedByFlag
		case baseline.Permits(d):
			out[d] = remoteAllowedEarlier
		case mode == renderAsking:
			out[d] = remoteNeedsOK
		case mode == renderRefusing:
			out[d] = remoteRefused
		default:
			out[d] = remoteNotAllowed
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
