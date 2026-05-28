// routing_feedback_logger.go provides the once-logged warning seam used by
// the routing-feedback read/write paths and by newRouteID. PR3 wires three
// independent sync.Once-guarded emitters: feedback read failures, feedback
// write failures, and crypto/rand failures in route-ID generation. Each
// fires at most once per Router instance, so a persistently broken store cannot
// flood logs while still surfacing the first occurrence to operators.
package provider

import (
	"log"
	"sync"
)

// feedbackLogger is the minimal logger seam used by the routing-feedback
// surface. Implementations must be safe for concurrent calls. PR3 ships
// a default implementation backed by log.Default(). The interface is
// intentionally unexported — the spec restricts PR3's public surface to
// the SQLite store, FeedbackScoringMode, and ScoreBreakdown. Tests in
// the same package inject capturing implementations by assigning
// router.feedbackLogger directly after setupTestRouter returns.
type feedbackLogger interface {
	Warnf(format string, args ...any)
}

// defaultFeedbackLoggerImpl forwards to log.Default(). Used when no
// override is assigned to router.feedbackLogger.
type defaultFeedbackLoggerImpl struct{}

func (defaultFeedbackLoggerImpl) Warnf(format string, args ...any) {
	log.Printf("WARN provider/routing_feedback: "+format, args...)
}

// defaultFeedbackLogger is the package-level default.
var defaultFeedbackLogger feedbackLogger = defaultFeedbackLoggerImpl{}

// feedbackWarningState holds the three sync.Once guards used to keep
// noisy operator warnings to one emission per Router instance. A Router
// owns exactly one feedbackWarningState; tests construct ad-hoc states to
// assert the once semantics in isolation.
type feedbackWarningState struct {
	readOnce       sync.Once
	writeOnce      sync.Once
	routeIDRndOnce sync.Once
}

// newFeedbackWarningState returns a zero-initialized state.
func newFeedbackWarningState() *feedbackWarningState {
	return &feedbackWarningState{}
}

// warnFeedbackReadOnce emits one warning the first time a feedback store
// Get/Score call returns an error during snapshot building. Subsequent
// calls are silent.
//
// The nil-logger guard runs BEFORE sync.Once.Do so a nil logger does not
// silently consume the once. If a caller wires the emitter before a real
// logger is attached (init order, test scaffolding), the once stays
// armed and the first real-logger call still emits.
func (s *feedbackWarningState) warnFeedbackReadOnce(l feedbackLogger, key FeedbackKey, err error) {
	if s == nil || l == nil {
		return
	}
	s.readOnce.Do(func() {
		l.Warnf("feedback store read failed for key %+v (further read failures silenced): %v", key, err)
	})
}

// warnFeedbackWriteOnce emits one warning the first time
// RoutingFeedback.RecordOutcome returns a non-nil error. Replaces the
// silent-swallow that recordOutcomeFeedback shipped in PR2. Same
// nil-logger-before-Do guard as warnFeedbackReadOnce.
func (s *feedbackWarningState) warnFeedbackWriteOnce(l feedbackLogger, err error) {
	if s == nil || l == nil {
		return
	}
	s.writeOnce.Do(func() {
		l.Warnf("feedback store RecordOutcome failed (further write failures silenced): %v", err)
	})
}

// warnRouteIDRandOnce emits one warning the first time crypto/rand
// produces an error inside newRouteID. Replaces the silent swallow that
// route_plan.go shipped in PR2. Same nil-logger-before-Do guard as
// warnFeedbackReadOnce.
func (s *feedbackWarningState) warnRouteIDRandOnce(l feedbackLogger, err error) {
	if s == nil || l == nil {
		return
	}
	s.routeIDRndOnce.Do(func() {
		l.Warnf("newRouteID crypto/rand failure (further failures silenced): %v", err)
	})
}
