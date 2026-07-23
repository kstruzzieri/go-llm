package provider

import (
	"strings"
	"testing"
)

func TestFeedbackScoringModeString(t *testing.T) {
	cases := []struct {
		mode FeedbackScoringMode
		want string
	}{
		{FeedbackScoringOff, "off"},
		{FeedbackScoringShadow, "shadow"},
		{FeedbackScoringEnforce, "enforce"},
	}
	for _, tc := range cases {
		if got := tc.mode.String(); got != tc.want {
			t.Errorf("FeedbackScoringMode(%d).String() = %q, want %q", tc.mode, got, tc.want)
		}
	}
}

func TestFeedbackScoringModeUnknownString(t *testing.T) {
	got := FeedbackScoringMode(99).String()
	// Lock in the godoc contract: unknown values produce a *labeled*
	// fallback so log lines never contain a bare integer like "99".
	if !strings.Contains(got, "unknown") {
		t.Errorf("unknown mode label = %q; want it to contain 'unknown'", got)
	}
	if !strings.Contains(got, "99") {
		t.Errorf("unknown mode label = %q; want it to include the int value 99", got)
	}
}

func TestWithFeedbackScoringModeAppliesValue(t *testing.T) {
	router, _ := setupTestRouter(t, WithFeedbackScoringMode(FeedbackScoringShadow))
	if router.feedbackScoringMode != FeedbackScoringShadow {
		t.Errorf("feedbackScoringMode = %v, want FeedbackScoringShadow", router.feedbackScoringMode)
	}
}

func TestWithFeedbackScoringSugarMapsToEnforce(t *testing.T) {
	router, _ := setupTestRouter(t, WithFeedbackScoring(true))
	if router.feedbackScoringMode != FeedbackScoringEnforce {
		t.Errorf("WithFeedbackScoring(true) gave mode %v, want FeedbackScoringEnforce", router.feedbackScoringMode)
	}
}

func TestWithFeedbackScoringFalseMapsToOff(t *testing.T) {
	router, _ := setupTestRouter(t, WithFeedbackScoring(false))
	if router.feedbackScoringMode != FeedbackScoringOff {
		t.Errorf("WithFeedbackScoring(false) gave mode %v, want FeedbackScoringOff", router.feedbackScoringMode)
	}
}

func TestDefaultFeedbackScoringModeIsOff(t *testing.T) {
	router, _ := setupTestRouter(t)
	if router.feedbackScoringMode != FeedbackScoringOff {
		t.Errorf("default feedbackScoringMode = %v, want FeedbackScoringOff", router.feedbackScoringMode)
	}
}
