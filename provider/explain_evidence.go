// provider/explain_evidence.go
//
// The pure capability precedence core (#736): one fact per canonical capability
// from explicit, direct, provider, catalog and type evidence, with what was
// checked, attempted, flagged, answered through the other API or remembered by
// the legacy table shown beside it. It takes values only: no context, store,
// clock or I/O.
package provider

import (
	"slices"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

// EvidenceInputs is everything ExplainEvidence ranks for one model through one
// binding.
type EvidenceInputs struct {
	Adapter string // binding adapter; rows of the other adapter only fill OtherAdapter
	Digest  string // identity the rows are read at; "" is name-only
	// Now is the instant expiry and freshness are judged at. It is required:
	// the core takes no clock, and at a zero Now every row looks future-dated
	// and is dropped, so a stored no would lose to a catalog yes.
	Now       time.Time
	Explicit  []string      // nonempty explicit list, nil when none
	TypeCaps  Capability    // type tier; 0 when none
	TypeNames []string      // contributing model types, in any order, duplicates allowed; nil allowed
	Catalog   *ModelProfile // nil when no family matches
	// Runtime holds live facts from the runtime's model listing
	// (OllamaListingFacts) by capability: empty when the runtime reports
	// neither capabilities nor a template, as openai-compat never does, and
	// always nil from configview. A yes or no answers only a capability no
	// stored provider row answers, and is counted with Method ollama-show.
	Runtime map[string]fingerprint.CapProbeState
	Rows    []fingerprint.EvidenceRow // one model's rows, both alias spellings
	// Legacy is the capability_probes row for tool_call, shown as history and
	// never counted. A row for any other capability is ignored. Its BackendID
	// must be the provider label, as ReadSnapshot and the registry supply it,
	// because History.Provider copies it.
	Legacy *fingerprint.CapProbe
}

// CheckedAnswer is a stored answer or attempt shown beside a fact's winner.
type CheckedAnswer struct {
	State   string    `json:"state"`
	Method  string    `json:"method,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	At      time.Time `json:"at,omitzero"`
	Unsaved bool      `json:"unsaved,omitempty"`
}

// CapabilityFlag is a problem seen with a capability in use. It never changes
// the fact's state.
type CapabilityFlag struct {
	Cause   string    `json:"cause"`
	At      time.Time `json:"at,omitzero"`
	Status  int       `json:"status,omitempty"`
	Unsaved bool      `json:"unsaved,omitempty"`
}

// OtherAdapterAnswer is an answer stored through the other API at the same
// endpoint. It never counts.
type OtherAdapterAnswer struct {
	Adapter string    `json:"adapter"`
	State   string    `json:"state"`
	Method  string    `json:"method,omitempty"`
	At      time.Time `json:"at,omitzero"`
	Unsaved bool      `json:"unsaved,omitempty"`
}

// HistoryAnswer is the legacy capability_probes verdict for tool_call. It never
// counts.
type HistoryAnswer struct {
	Provider string    `json:"provider"`
	State    string    `json:"state"`
	At       time.Time `json:"at,omitzero"`
}

// CapabilityFact explains one capability: its state, the tier that decided it,
// the stored row's details when a row decided it, and the records beside it.
type CapabilityFact struct {
	Capability   string              `json:"capability"`
	State        string              `json:"state"`  // yes | no | inconclusive | unknown
	Source       string              `json:"source"` // explicit | probe | observed | provider | catalog | type | history | none
	Method       string              `json:"method,omitempty"`
	Detail       string              `json:"detail,omitempty"`
	Identity     string              `json:"identity,omitempty"` // content-digest | name-only
	CheckedAt    time.Time           `json:"checked_at,omitzero"`
	FreshUntil   time.Time           `json:"fresh_until,omitzero"`
	ExpiresAt    time.Time           `json:"expires_at,omitzero"`
	Stale        bool                `json:"stale,omitempty"`
	Unsaved      bool                `json:"unsaved,omitempty"`
	LastCheck    *CheckedAnswer      `json:"last_check,omitempty"`
	LastAttempt  *CheckedAnswer      `json:"last_attempt,omitempty"`
	Flag         *CapabilityFlag     `json:"flag,omitempty"`
	OtherAdapter *OtherAdapterAnswer `json:"other_adapter,omitempty"`
	History      *HistoryAnswer      `json:"history,omitempty"`
}

const (
	factUnknown      = "unknown"
	sourceExplicit   = "explicit"
	sourceCatalog    = "catalog"
	sourceType       = "type"
	sourceHistory    = "history"
	sourceNone       = "none"
	identityDigest   = "content-digest"
	identityNameOnly = "name-only"
	detailOther      = "other"
)

// ExplainEvidence ranks one model's capability evidence and returns one fact per
// canonical capability, in canonical order. The first tier that answers wins: a
// parseable nonempty explicit list; the newer direct answer, probe or observed
// (an equal time goes to the probe); a stored provider row; a live provider fact
// (yes or no) when no stored provider row answers; the catalog; the model type;
// an unexpired attempt (inconclusive); legacy tool_call history (unknown, never
// counted); else unknown. Catalog and type only say yes. Rows of the other
// adapter only fill OtherAdapter. Row order and TypeNames order are
// irrelevant. Stored details and flag statuses are projected onto bounded
// codes. A name in CanonicalCapabilityNames with no canonical bit is skipped.
func ExplainEvidence(in EvidenceInputs) []CapabilityFact {
	readings := fingerprint.ReadCapabilities(in.Rows, in.Adapter, in.Digest, in.Now)
	explicit, err := ParseCapsStrict(in.Explicit)
	if err != nil {
		explicit = 0 // ignored, as merge ignores a list that fails to parse
	}
	typeMethod := typeMethodOf(in.TypeNames)
	facts := make([]CapabilityFact, 0, len(CanonicalCapabilityNames))
	for _, name := range CanonicalCapabilityNames {
		bit, ok := canonicalCapability(name)
		if !ok {
			continue // Has(0) is always true: an unknown name would read yes
		}
		reading := readings[name]
		attempt := reading.Attempt
		live := in.Runtime[name]
		history := historyAnswer(in.Legacy, name)
		f := CapabilityFact{Capability: name}
		attemptWon := false
		switch row := readingAnswer(reading); {
		case explicit != 0:
			f.State, f.Source = string(fingerprint.CapProbeNo), sourceExplicit
			if explicit.Has(bit) {
				f.State = string(fingerprint.CapProbeYes)
			}
		case row != nil:
			fillFromRow(&f, row, in.Now)
		case live == fingerprint.CapProbeYes || live == fingerprint.CapProbeNo:
			f.State, f.Source, f.Method = string(live), string(fingerprint.EvidenceSourceProvider), fingerprint.MethodOllamaShow
			f.Identity = identityOf(in.Digest)
		case in.Catalog != nil && in.Catalog.Caps.Has(bit):
			f.State, f.Source, f.Method = string(fingerprint.CapProbeYes), sourceCatalog, sourceCatalog+":"+in.Catalog.Family
		case in.TypeCaps.Has(bit):
			f.State, f.Source, f.Method = string(fingerprint.CapProbeYes), sourceType, typeMethod
		case attemptHolds(attempt, in.Now):
			fillFromRow(&f, attempt, in.Now)
			attemptWon = true
		case history != nil:
			f.State, f.Source = factUnknown, sourceHistory
		default:
			f.State, f.Source = factUnknown, sourceNone
		}
		if f.Source == sourceExplicit || f.Source == string(fingerprint.EvidenceSourceObserved) {
			f.LastCheck = checkedAnswer(reading.Probe)
		}
		if !attemptWon {
			f.LastAttempt = checkedAnswer(attempt)
		}
		f.Flag = capabilityFlag(reading.Flag)
		f.OtherAdapter = otherAdapterAnswer(reading.OtherAdapter)
		f.History = history
		facts = append(facts, f)
	}
	return facts
}

// CapabilityMasks reduces facts to the capabilities they say yes to, no to,
// and neither; inconclusive counts as unknown. Facts for names outside the
// canonical set are skipped.
func CapabilityMasks(facts []CapabilityFact) (yes, no, unknown Capability) {
	for _, f := range facts {
		bit, ok := canonicalCapability(f.Capability)
		if !ok {
			continue
		}
		switch f.State {
		case string(fingerprint.CapProbeYes):
			yes |= bit
		case string(fingerprint.CapProbeNo):
			no |= bit
		default:
			unknown |= bit
		}
	}
	return yes, no, unknown
}

// directAnswer is the newer of a reading's probe and observed answers, an
// equal time going to the probe; nil when it has neither.
func directAnswer(c fingerprint.CapabilityReading) *fingerprint.EvidenceRow {
	switch {
	case c.Probe != nil && (c.Observed == nil || !c.Observed.ObservedAt.After(c.Probe.ObservedAt)):
		return c.Probe
	case c.Observed != nil:
		return c.Observed
	}
	return nil
}

// readingAnswer is the answer a reading's stored log tiers give: the direct
// answer, else what the provider reported, else nil. It stays the one
// implementation of those tiers (#736): ExplainEvidence ranks with it and
// the resolver (toolCallFromLog) settles with it, so the two cannot disagree.
func readingAnswer(c fingerprint.CapabilityReading) *fingerprint.EvidenceRow {
	if d := directAnswer(c); d != nil {
		return d
	}
	return c.Provider
}

// attemptHolds reports whether an attempt still holds its cooldown at now: its
// expiry is inclusive, and an attempt with no ExpiresAt (ExpiryFor never writes
// one; another writer might) holds indefinitely. False for nil. The core, the
// tool_call explanation and the resolver all read attempts through it.
func attemptHolds(a *fingerprint.EvidenceRow, now time.Time) bool {
	return a != nil && (a.ExpiresAt.IsZero() || !now.After(a.ExpiresAt))
}

// fillFromRow makes a stored row f's winner.
func fillFromRow(f *CapabilityFact, row *fingerprint.EvidenceRow, now time.Time) {
	f.State, f.Source, f.Method = string(row.State), string(row.Source), row.Method
	f.Detail, f.Identity = projectDetail(row.Detail), identityOf(row.Digest)
	f.CheckedAt, f.FreshUntil, f.ExpiresAt = row.ObservedAt, row.FreshUntil, row.ExpiresAt
	f.Stale = row.State == fingerprint.CapProbeYes && !row.FreshUntil.IsZero() && row.FreshUntil.Before(now)
	f.Unsaved = row.ID < 0
}

func checkedAnswer(row *fingerprint.EvidenceRow) *CheckedAnswer {
	if row == nil {
		return nil
	}
	return &CheckedAnswer{
		State: string(row.State), Method: row.Method, Detail: projectDetail(row.Detail),
		At: row.ObservedAt, Unsaved: row.ID < 0,
	}
}

func capabilityFlag(row *fingerprint.EvidenceRow) *CapabilityFlag {
	if row == nil {
		return nil
	}
	status := 0
	if row.HTTPStatus == 400 || row.HTTPStatus == 422 { // the statuses a refused run reports
		status = row.HTTPStatus
	}
	return &CapabilityFlag{Cause: projectDetail(row.Detail), At: row.ObservedAt, Status: status, Unsaved: row.ID < 0}
}

func otherAdapterAnswer(row *fingerprint.EvidenceRow) *OtherAdapterAnswer {
	if row == nil {
		return nil
	}
	return &OtherAdapterAnswer{Adapter: row.Adapter, State: string(row.State), Method: row.Method, At: row.ObservedAt, Unsaved: row.ID < 0}
}

// historyAnswer is the legacy verdict shown for tool_call, nil for any other
// capability, a legacy row for another capability, or a state outside yes, no
// and inconclusive.
func historyAnswer(legacy *fingerprint.CapProbe, capability string) *HistoryAnswer {
	if legacy == nil || capability != capabilityToolCall || legacy.Capability != capabilityToolCall {
		return nil
	}
	switch legacy.State {
	case fingerprint.CapProbeYes, fingerprint.CapProbeNo, fingerprint.CapProbeInconclusive:
		return &HistoryAnswer{Provider: legacy.BackendID, State: string(legacy.State), At: legacy.TestedAt}
	}
	return nil
}

// projectDetail keeps a stored detail only when it is a bounded code, so a
// provider's raw text never leaves.
func projectDetail(detail string) string {
	switch detail {
	case "", fingerprint.DetailToolCalled, fingerprint.DetailToolIgnored, fingerprint.DetailToolNotCalled,
		fingerprint.DetailRequestRejected, fingerprint.DetailCutOff, fingerprint.DetailToolCallInvalid,
		fingerprint.DetailCheckFailed, fingerprint.DetailRunRefused:
		return detail
	}
	return detailOther
}

func identityOf(digest string) string {
	if digest == "" {
		return identityNameOnly
	}
	return identityDigest
}

// typeMethodOf is the type tier's Method: "type:" and the distinct names in
// sorted order joined by "+", or "type" when there are none.
func typeMethodOf(names []string) string {
	if len(names) == 0 {
		return sourceType
	}
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	return sourceType + ":" + strings.Join(slices.Compact(sorted), "+")
}
