package sqlitetest

import (
	"testing"
	"time"
)

func TestTrialTimeout(t *testing.T) {
	for _, tc := range []struct {
		remaining time.Duration
		ok        bool
		want      time.Duration
	}{
		{0, false, time.Minute},
		{10 * time.Minute, true, time.Minute},
		{4 * time.Second, true, 3 * time.Second},
	} {
		if got := trialTimeout(tc.remaining, tc.ok); got != tc.want {
			t.Errorf("trialTimeout(%v, %t) = %v, want %v", tc.remaining, tc.ok, got, tc.want)
		}
	}
}
