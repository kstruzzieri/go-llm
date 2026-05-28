package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseCaptureSample(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		want    captureSampleSpec
		wantErr string
	}{
		{
			name: "n-only",
			spec: "n=20",
			want: captureSampleSpec{N: 20},
		},
		{
			name: "stratify-and-filters",
			spec: "n=50,stratify=token-length:turn-count,recency=last-30d,has-tool-calls=true",
			want: captureSampleSpec{
				N:            50,
				Stratify:     []string{"token-length", "turn-count"},
				Recency:      "last-30d",
				HasToolCalls: trueBool(),
			},
		},
		{
			name:    "empty-rejected",
			spec:    "",
			wantErr: "empty",
		},
		{
			name:    "unknown-key",
			spec:    "n=10,wat=1",
			wantErr: "unknown key wat",
		},
		{
			name:    "bad-n",
			spec:    "n=zero",
			wantErr: "n=",
		},
		{
			name:    "bad-stratify-dimension",
			spec:    "n=10,stratify=wat",
			wantErr: "unknown stratify dimension wat",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCaptureSample(tc.spec)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v; want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v; want %+v", got, tc.want)
			}
		})
	}
}

func trueBool() *bool {
	b := true
	return &b
}

func TestTokenLengthBucket(t *testing.T) {
	cases := []struct {
		runes int
		want  string
	}{
		{0, "small"},
		{3500, "small"},
		{4000, "small"}, // ~1k tokens at 4 bytes/token
		{4001, "medium"},
		{32000, "medium"},
		{32001, "large"},
	}
	for _, tc := range cases {
		if got := tokenLengthBucket(tc.runes); got != tc.want {
			t.Errorf("tokenLengthBucket(%d)=%q; want %q", tc.runes, got, tc.want)
		}
	}
}

func TestTurnCountBucket(t *testing.T) {
	if got := turnCountBucket(2); got != "short" {
		t.Errorf("turnCountBucket(2)=%q; want short", got)
	}
	if got := turnCountBucket(5); got != "medium" {
		t.Errorf("turnCountBucket(5)=%q; want medium", got)
	}
	if got := turnCountBucket(20); got != "long" {
		t.Errorf("turnCountBucket(20)=%q; want long", got)
	}
}

func TestRecencyBucket(t *testing.T) {
	now := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		captured time.Time
		want     string
	}{
		{now.Add(-3 * 24 * time.Hour), "last-7d"},
		{now.Add(-15 * 24 * time.Hour), "last-30d"},
		{now.Add(-60 * 24 * time.Hour), "older"},
	}
	for _, tc := range cases {
		if got := recencyBucket(tc.captured, now); got != tc.want {
			t.Errorf("recencyBucket=%q; want %q", got, tc.want)
		}
	}
}
