package golem

import (
	"fmt"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// TestProviderRejected pins the predicate on its own: failureCode's earlier
// arms mask 429 and 500 in a full run, and the provenance rule must hold even
// when the run error carries a status that is not this run's model error.
func TestProviderRejected(t *testing.T) {
	status := func(code int) error { return &provider.HTTPStatusError{StatusCode: code} }
	for _, code := range []int{400, 404, 408, 413, 422, 499} {
		s := status(code)
		if !providerRejected(s, s) {
			t.Errorf("%d: want rejected", code)
		}
		if wrapped := fmt.Errorf("run: %w", s); !providerRejected(wrapped, s) {
			t.Errorf("%d wrapped: want rejected", code)
		}
	}
	for _, code := range []int{399, 401, 403, 429, 500, 503} {
		s := status(code)
		if providerRejected(s, s) {
			t.Errorf("%d: want not rejected", code)
		}
	}
	s := status(400)
	if providerRejected(s, nil) {
		t.Error("no model error: want not rejected")
	}
	if providerRejected(s, status(400)) {
		t.Error("run error is not this run's model error: want not rejected")
	}
	if providerRejected(fmt.Errorf("plain"), fmt.Errorf("plain")) {
		t.Error("no status: want not rejected")
	}
}
