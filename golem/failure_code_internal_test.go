package golem

import (
	"errors"
	"fmt"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// TestProviderRejected covers the status boundaries independently of
// failureCode, whose earlier cases handle 429 and 5xx.
func TestProviderRejected(t *testing.T) {
	status := func(code int) error { return &provider.HTTPStatusError{StatusCode: code} }
	for _, code := range []int{400, 404, 408, 413, 422, 499} {
		s := status(code)
		if !providerRejected(s) {
			t.Errorf("%d: want rejected", code)
		}
		if wrapped := fmt.Errorf("model: %w", s); !providerRejected(wrapped) {
			t.Errorf("%d wrapped: want rejected", code)
		}
	}
	for _, code := range []int{399, 401, 403, 429, 500, 503} {
		s := status(code)
		if providerRejected(s) {
			t.Errorf("%d: want not rejected", code)
		}
	}
	if providerRejected(nil) {
		t.Error("no model error: want not rejected")
	}
	if providerRejected(errors.New("plain")) {
		t.Error("no status: want not rejected")
	}
}
