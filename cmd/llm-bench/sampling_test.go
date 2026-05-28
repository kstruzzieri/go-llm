package main

import (
	"reflect"
	"strings"
	"testing"
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
