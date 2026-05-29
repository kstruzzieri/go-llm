package transcript

import (
	"time"

	"github.com/kstruzzieri/go-llm/conversation"
)

// stitch_status values recorded on a canonical conversation row.
const (
	statusCreated    = "created"
	statusExtended   = "extended"
	statusForked     = "forked"
	statusIdempotent = "idempotent"
)

// Lease-guard defaults (§7): a prefix candidate that is both stale and short is
// treated as a coincidental common opener and forked rather than extended.
const (
	defaultLeaseWindow    = 12 * time.Hour
	defaultShortThreshold = 2
)

// candidate is one canonical row participating in a stitch decision.
type candidate struct {
	id           string
	messages     []conversation.Message
	messageCount int   // len(messages); recomputed at load, never trusted from the column
	updatedAt    int64 // unix millis
}

// stitchDecision is the outcome of decideStitch.
type stitchDecision struct {
	targetID string
	status   string
}

// forkID returns the content-addressed id of a forked sibling of key. Because
// it is content-addressed, re-sending the same forked history resolves back to
// the same sibling (no unbounded fork growth).
func forkID(key string, incoming []conversation.Message) string {
	return key + ":" + sha256Hex(canonicalMessagesJSON(incoming))[:12]
}

// decideStitch resolves how an incoming full history relates to the candidate
// rows that share its base key (§7). candidates must already be the full set of
// rows sharing the key (base + every forked sibling).
func decideStitch(key string, incoming []conversation.Message, candidates []candidate, now time.Time, leaseWindow time.Duration, shortThreshold int) stitchDecision {
	if len(candidates) == 0 {
		return stitchDecision{targetID: key, status: statusCreated}
	}

	// 2. Exact re-record.
	for _, c := range candidates {
		if messagesEqual(c.messages, incoming) {
			return stitchDecision{targetID: c.id, status: statusIdempotent}
		}
	}

	// 3. Genuine extension: a candidate is a strict prefix of incoming. Pick the
	// longest (most specific continuation).
	var ext *candidate
	for i := range candidates {
		if isStrictPrefix(candidates[i].messages, incoming) {
			if ext == nil || candidates[i].messageCount > ext.messageCount {
				ext = &candidates[i]
			}
		}
	}
	if ext != nil {
		stale := now.Sub(time.UnixMilli(ext.updatedAt)) > leaseWindow
		short := ext.messageCount <= shortThreshold
		if stale && short {
			return stitchDecision{targetID: forkID(key, incoming), status: statusForked}
		}
		return stitchDecision{targetID: ext.id, status: statusExtended}
	}

	// 4. Re-send of an earlier state: incoming is a strict prefix of a candidate.
	// Keep the longer stored history (no shrink).
	var longer *candidate
	for i := range candidates {
		if isStrictPrefix(incoming, candidates[i].messages) {
			if longer == nil || candidates[i].messageCount > longer.messageCount {
				longer = &candidates[i]
			}
		}
	}
	if longer != nil {
		return stitchDecision{targetID: longer.id, status: statusIdempotent}
	}

	// 5. All diverge.
	return stitchDecision{targetID: forkID(key, incoming), status: statusForked}
}
