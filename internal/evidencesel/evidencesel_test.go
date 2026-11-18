package evidencesel

import (
	"reflect"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestEvidenceSelect_DigestAndRows(t *testing.T) {
	ol := provider.EvidenceBinding{Endpoint: "http://127.0.0.1:11434", Adapter: fingerprint.AdapterOllama}
	oc := provider.EvidenceBinding{Endpoint: "http://127.0.0.1:8080", Adapter: fingerprint.AdapterOpenAICompat}
	listing := func(members ...fingerprint.ListingMember) *fingerprint.ListingSnapshot {
		return &fingerprint.ListingSnapshot{Members: members}
	}
	cases := []struct {
		name       string
		binding    provider.EvidenceBinding
		listing    *fingerprint.ListingSnapshot
		model      string
		resolved   bool
		wantDigest string
	}{
		{"exact spelling", ol, listing(fingerprint.ListingMember{Model: "q", Digest: "sha256:exact"}), "q", true, "sha256:exact"},
		{"exact beats an earlier other spelling", ol, listing(
			fingerprint.ListingMember{Model: "q:latest", Digest: "sha256:alias"},
			fingerprint.ListingMember{Model: "q", Digest: "sha256:exact"}), "q", true, "sha256:exact"},
		{"other ollama spelling", ol, listing(fingerprint.ListingMember{Model: "q:latest", Digest: "sha256:alias"}), "q", true, "sha256:alias"},
		{"first other spelling wins", ol, listing(
			fingerprint.ListingMember{Model: "q:latest", Digest: "sha256:one"},
			fingerprint.ListingMember{Model: "q:latest", Digest: "sha256:two"}), "q", true, "sha256:one"},
		{"no member", ol, listing(fingerprint.ListingMember{Model: "other", Digest: "sha256:x"}), "q", true, ""},
		{"no listing", ol, nil, "q", true, ""},
		{"unresolved credential", ol, listing(fingerprint.ListingMember{Model: "q", Digest: "sha256:exact"}), "q", false, ""},
		{"openai-compat has no other spelling", oc, listing(fingerprint.ListingMember{Model: "m:latest", Digest: "sha256:alias"}), "m", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			digest, _ := Select(fingerprint.EvidenceSnapshot{Listing: tc.listing}, tc.binding, tc.model, tc.resolved)
			if digest != tc.wantDigest {
				t.Fatalf("digest = %q; want %q", digest, tc.wantDigest)
			}
		})
	}

	t.Run("rows", func(t *testing.T) {
		at := time.Now().Add(-time.Hour)
		row := func(endpoint, model string) fingerprint.EvidenceRow {
			return fingerprint.EvidenceRow{Endpoint: endpoint, Model: model, Capability: "chat", ObservedAt: at}
		}
		snap := fingerprint.EvidenceSnapshot{Rows: []fingerprint.EvidenceRow{
			row(ol.Endpoint, "q"),
			row(oc.Endpoint, "q"), // another endpoint
			row(ol.Endpoint, "q:latest"),
			row(ol.Endpoint, "other"), // another model
		}}
		_, got := Select(snap, ol, "q", true)
		want := []fingerprint.EvidenceRow{row(ol.Endpoint, "q"), row(ol.Endpoint, "q:latest")}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("rows = %+v; want %+v", got, want)
		}
		// The credential gates the digest only, never the rows.
		if _, got := Select(snap, ol, "q", false); !reflect.DeepEqual(got, want) {
			t.Fatalf("unresolved rows = %+v; want %+v", got, want)
		}
		// Through openai-compat only the exact spelling counts.
		if _, got := Select(fingerprint.EvidenceSnapshot{Rows: []fingerprint.EvidenceRow{row(oc.Endpoint, "q"), row(oc.Endpoint, "q:latest")}}, oc, "q", true); !reflect.DeepEqual(got, []fingerprint.EvidenceRow{row(oc.Endpoint, "q")}) {
			t.Fatalf("openai-compat rows = %+v; want only q", got)
		}
	})
}
