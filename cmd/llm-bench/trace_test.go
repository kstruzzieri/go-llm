package main

import (
	"errors"
	"testing"
)

func TestValidateTrace(t *testing.T) {
	tests := []struct {
		name    string
		trace   Trace
		wantErr error
	}{
		{
			name:    "ok",
			trace:   Trace{ID: "t1", System: "sys", Turns: []Turn{{Role: "user", Content: "q"}}},
			wantErr: nil,
		},
		{
			name:    "missing id",
			trace:   Trace{System: "sys", Turns: []Turn{{Role: "user"}}},
			wantErr: nil, // validateTrace returns fmt.Errorf, not a sentinel, for missing id — covered by substring
		},
		{
			name:    "empty system",
			trace:   Trace{ID: "t2", Turns: []Turn{{Role: "user"}}},
			wantErr: errEmptySystem,
		},
		{
			name:    "no turns",
			trace:   Trace{ID: "t3", System: "sys"},
			wantErr: errNoTurns,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTrace(tt.trace)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			// nil wantErr cases: ok case must pass, missing-id case must error.
			if tt.name == "ok" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.name == "missing id" && err == nil {
				t.Fatal("expected error for missing id, got nil")
			}
		})
	}
}
