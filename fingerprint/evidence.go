package fingerprint

import (
	"maps"
	"slices"
	"time"
)

// EvidenceKeyVersion is the endpoint canonicalization version written to
// capability_evidence.key_version. Rows of any other version are not read.
const EvidenceKeyVersion = 1

// Adapters: the API an answer came through. Ollama serves its own API and an
// OpenAI-compatible one at one address, so the endpoint alone cannot say which.
const (
	AdapterOllama       = "ollama"
	AdapterOpenAICompat = "openai-compat"
)

// CapProbeFlagged is the state of a flag row, which reports a problem seen with
// a capability rather than a verdict on it.
const CapProbeFlagged CapProbeState = "flagged"

// EvidenceSource says where a row came from.
type EvidenceSource string

const (
	// EvidenceSourceProbe is a check made to test the capability.
	EvidenceSourceProbe EvidenceSource = "probe"
	// EvidenceSourceObserved is a report from real use.
	EvidenceSourceObserved EvidenceSource = "observed"
	// EvidenceSourceProvider is what the provider itself reported.
	EvidenceSourceProvider EvidenceSource = "provider"
)

// EvidenceKind says what a row claims.
type EvidenceKind string

const (
	// EvidenceKindAnswer is a definitive yes or no.
	EvidenceKindAnswer EvidenceKind = "answer"
	// EvidenceKindAttempt is a check that settled nothing, state inconclusive.
	EvidenceKindAttempt EvidenceKind = "attempt"
	// EvidenceKindFlag is a problem seen in use, state flagged.
	EvidenceKindFlag EvidenceKind = "flag"
)

const (
	// maxClockSkew is how far ahead of a reader's clock a row may be dated and
	// still count. Anything further is ignored until the clock catches up.
	maxClockSkew = time.Hour
	// nameOnlyFreshWindow is how long a yes with no content digest stays fresh.
	nameOnlyFreshWindow = 30 * 24 * time.Hour
	// evidenceRetention is the history Prune keeps unconditionally.
	evidenceRetention = 90 * 24 * time.Hour
)

// EvidenceRow is one row of the append-only capability evidence log.
type EvidenceRow struct {
	ID            int64  // insertion order; only ever the final tie-breaker
	KeyVersion    int    // EvidenceKeyVersion when written; other versions are not read
	Endpoint      string // destination canonical form
	Adapter       string // the API the row came through
	Model         string
	Digest        string // content digest; empty when name-only
	Capability    string
	Source        EvidenceSource
	Kind          EvidenceKind
	State         CapProbeState // yes or no (answer), inconclusive (attempt), flagged (flag)
	Method        string
	MethodVersion int
	Detail        string // bounded code; a flag's cause
	HTTPStatus    int    // run_refused flags only; 0 when absent
	ObservedAt    time.Time
	FreshUntil    time.Time // zero when not set
	ExpiresAt     time.Time // zero when valid indefinitely
}

// CapabilityReading is what the log says about one capability on one route.
type CapabilityReading struct {
	Capability string

	// Probe, Observed and Provider hold the newest valid definitive answer from
	// each source. An expired answer does not count, so an older one still valid
	// can return when a newer one expires.
	Probe, Observed, Provider *EvidenceRow

	// Attempt is the newest attempt, past its expiry or not. Honoring a cooldown
	// means reading its ExpiresAt.
	Attempt *EvidenceRow

	// Flag is the newest unexpired flag newer than Probe and Observed. A provider
	// answer does not clear it, and neither does a direct answer that has expired.
	Flag *EvidenceRow

	// OtherAdapter is the newest valid answer through the other API, whatever its
	// digest: digests from different APIs are not comparable. It never counts.
	OtherAdapter *EvidenceRow
}

// ExpiryFor returns the fresh_until and expires_at a row should be written with,
// derived from its ObservedAt. A zero time means the column is not set. A yes
// with a content digest does not expire, because a new digest invalidates it. A
// yes with none is valid indefinitely but fresh for 30 days. A no with none
// expires after 7 days; a no with a digest after proberTTL, or never when that
// is zero. An inconclusive expires after 24 hours.
func ExpiryFor(row EvidenceRow, proberTTL time.Duration) (freshUntil, expiresAt time.Time) {
	switch row.State {
	case CapProbeYes:
		if row.Digest == "" {
			freshUntil = row.ObservedAt.Add(nameOnlyFreshWindow)
		}
	case CapProbeNo:
		if row.Digest == "" {
			expiresAt = row.ObservedAt.Add(CapProbeDigestlessNoTTL)
		} else if proberTTL > 0 {
			expiresAt = row.ObservedAt.Add(proberTTL)
		}
	case CapProbeInconclusive:
		expiresAt = row.ObservedAt.Add(CapProbeInconclusiveTTL)
	}
	return freshUntil, expiresAt
}

// Fresh reports whether the row is a definitive answer that, at now, is
// unexpired and within FreshUntil when set. A valid answer can be stale.
func (r EvidenceRow) Fresh(now time.Time) bool {
	return r.definitive() && r.live(now) && (r.FreshUntil.IsZero() || !now.After(r.FreshUntil))
}

func (r EvidenceRow) definitive() bool {
	return r.Kind == EvidenceKindAnswer && (r.State == CapProbeYes || r.State == CapProbeNo)
}

// understood reports whether this binary knows the row's key version, method,
// method version, source and shape. Anything else belongs to another binary: it
// is never read and never pruned.
func (r EvidenceRow) understood() bool {
	recipe, ok := probeRecipes[r.Method]
	if !ok || r.KeyVersion != EvidenceKeyVersion || r.MethodVersion != recipe.Version ||
		r.Adapter != recipe.Adapter || !slices.Contains(recipe.Capabilities, r.Capability) || r.ObservedAt.IsZero() {
		return false
	}
	switch r.Source {
	case EvidenceSourceProbe, EvidenceSourceObserved, EvidenceSourceProvider:
	default:
		return false
	}
	switch r.Kind {
	case EvidenceKindAnswer:
		return r.definitive()
	case EvidenceKindAttempt:
		return r.State == CapProbeInconclusive
	case EvidenceKindFlag:
		return r.State == CapProbeFlagged
	}
	return false
}

// admitted reports whether the row's date is within the skew allowance at now.
func (r EvidenceRow) admitted(now time.Time) bool {
	return !r.ObservedAt.After(now.Add(maxClockSkew))
}

// live reports whether the row is admitted and unexpired at now.
func (r EvidenceRow) live(now time.Time) bool {
	return r.admitted(now) && (r.ExpiresAt.IsZero() || !now.After(r.ExpiresAt))
}

// newerThan orders rows by observation time, so a slow write of an older
// observation changes nothing. The ID only breaks an exact tie.
func (r EvidenceRow) newerThan(other EvidenceRow) bool {
	return r.ObservedAt.After(other.ObservedAt) || (r.ObservedAt.Equal(other.ObservedAt) && r.ID > other.ID)
}

// ReadCapabilities reduces one model's rows to a reading per capability for a
// route through adapter at digest, at time now. Row order is irrelevant. A
// capability with nothing to report has no entry. The rows in a reading are
// copies.
func ReadCapabilities(rows []EvidenceRow, adapter, digest string, now time.Time) map[string]CapabilityReading {
	readings := make(map[string]CapabilityReading)
	for _, r := range rows {
		if !r.understood() || !r.admitted(now) {
			continue
		}
		reading := readings[r.Capability]
		slot := reading.slotFor(r, adapter, digest, now)
		if slot == nil {
			continue
		}
		if *slot == nil || r.newerThan(**slot) {
			*slot = &r
		}
		reading.Capability = r.Capability
		readings[r.Capability] = reading
	}
	for capability, reading := range readings {
		if f := reading.Flag; f != nil && (reading.Probe != nil && !f.newerThan(*reading.Probe) ||
			reading.Observed != nil && !f.newerThan(*reading.Observed)) {
			reading.Flag = nil
			readings[capability] = reading
		}
	}
	return readings
}

// slotFor returns the field an understood, admitted row competes for, or nil
// when the row does not count for this route.
func (c *CapabilityReading) slotFor(r EvidenceRow, adapter, digest string, now time.Time) **EvidenceRow {
	if r.Adapter != adapter {
		if r.Kind == EvidenceKindAnswer && r.live(now) {
			return &c.OtherAdapter
		}
		return nil
	}
	if r.Digest != digest {
		return nil
	}
	switch {
	case r.Kind == EvidenceKindAttempt:
		return &c.Attempt
	case !r.live(now):
		return nil
	case r.Kind == EvidenceKindFlag:
		return &c.Flag
	case r.Source == EvidenceSourceProbe:
		return &c.Probe
	case r.Source == EvidenceSourceObserved:
		return &c.Observed
	}
	return &c.Provider
}

// NextReadingTransition returns the first instant after now at which what
// ReadCapabilities reports for a route through adapter at digest differs from
// what it reports at now, or zero when nothing in rows will ever change it. Such
// an instant comes from an expiry, a freshness end or the admission of a
// future-dated row (ObservedAt less the skew allowance). An end is inclusive, so
// its change is the instant after it. A row that loses to another changes
// nothing and is not named, and neither is the expiry of an attempt, which reads
// the same past it. OtherAdapter is left out: it never counts for this route.
// Rows are one model's, as for ReadCapabilities.
//
// ponytail: re-reads the rows at each future candidate, so quadratic in a model's
// rows at worst; it runs once per profile build, and most rows have no future
// instant.
func NextReadingTransition(rows []EvidenceRow, adapter, digest string, now time.Time) time.Time {
	var candidates []time.Time
	for _, r := range rows {
		candidates = append(candidates, r.ObservedAt.Add(-maxClockSkew))
		for _, end := range []time.Time{r.ExpiresAt, r.FreshUntil} {
			if !end.IsZero() {
				candidates = append(candidates, end.Add(time.Nanosecond))
			}
		}
	}
	slices.SortFunc(candidates, time.Time.Compare)
	current := routeStates(rows, adapter, digest, now)
	for _, t := range candidates {
		if t.After(now) && !maps.Equal(routeStates(rows, adapter, digest, t), current) {
			return t
		}
	}
	return time.Time{}
}

// routeState is a reading in comparable form: the row in each slot that counts
// for the route, and whether each is fresh.
type routeState struct {
	rows  [5]EvidenceRow
	fresh [5]bool
}

func routeStates(rows []EvidenceRow, adapter, digest string, at time.Time) map[string]routeState {
	states := make(map[string]routeState)
	for capability, c := range ReadCapabilities(rows, adapter, digest, at) {
		var s routeState
		for i, slot := range [...]*EvidenceRow{c.Probe, c.Observed, c.Provider, c.Attempt, c.Flag} {
			if slot != nil {
				s.rows[i], s.fresh[i] = *slot, slot.Fresh(at)
			}
		}
		if s != (routeState{}) { // an entry only for OtherAdapter says nothing here
			states[capability] = s
		}
	}
	return states
}

// evidencePartition identifies rows that compete for one reading under one
// method contract. Nothing is ever compacted across partitions.
type evidencePartition struct {
	keyVersion    int
	endpoint      string
	adapter       string
	model         string
	capability    string
	source        EvidenceSource
	method        string
	methodVersion int
	digest        string
	kind          EvidenceKind
}

func (r EvidenceRow) partition() evidencePartition {
	return evidencePartition{
		keyVersion: r.KeyVersion, endpoint: r.Endpoint, adapter: r.Adapter, model: r.Model,
		capability: r.Capability, source: r.Source, method: r.Method, methodVersion: r.MethodVersion,
		digest: r.Digest, kind: r.Kind,
	}
}

// dominates reports whether r wins over older whenever older could count, for a
// reader at now or later and for one behind now by up to maxClockSkew: r is newer
// and dated no later than now. A row dated inside the skew allowance does not
// qualify yet, because a reader behind now does not admit it. Answers and flags
// must also be valid now and for all of older's life; attempts stay readable
// regardless of expiry.
func (r EvidenceRow) dominates(older EvidenceRow, now time.Time) bool {
	if !r.newerThan(older) || r.ObservedAt.After(now) {
		return false
	}
	if r.Kind == EvidenceKindAttempt {
		return true
	}
	return r.live(now) &&
		(r.ExpiresAt.IsZero() || (!older.ExpiresAt.IsZero() && !r.ExpiresAt.Before(older.ExpiresAt)))
}

// prunableEvidenceIDs returns, sorted, the rows no reader can select at now or
// later: understood rows outside the retention window with a dominating sibling
// in their partition. Rows this binary does not understand are neither deleted
// nor allowed to authorize a deletion. A partition's newest row has no newer
// sibling, so it always stays.
//
// ponytail: quadratic per partition, which holds a handful of rows; sort each by
// observation time if a partition ever reaches thousands.
func prunableEvidenceIDs(rows []EvidenceRow, now time.Time) []int64 {
	partitions := make(map[evidencePartition][]EvidenceRow)
	for _, r := range rows {
		if r.understood() {
			partitions[r.partition()] = append(partitions[r.partition()], r)
		}
	}
	cutoff := now.Add(-evidenceRetention)
	var ids []int64
	for _, siblings := range partitions {
		for _, r := range siblings {
			if !r.ObservedAt.Before(cutoff) {
				continue
			}
			if slices.ContainsFunc(siblings, func(s EvidenceRow) bool { return s.dominates(r, now) }) {
				ids = append(ids, r.ID)
			}
		}
	}
	slices.Sort(ids)
	return ids
}
