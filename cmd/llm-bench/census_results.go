package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
)

const censusResultsSchema = "llm-bench.census-results/v1"

// censusResults is the committed census output (census-results.json): the
// per-(stratum, carrier, arm) aggregates and one row per observation with any
// finding. It holds findings and counts, never observation content.
// ManifestSHA256 binds the manifest bytes; FullSHA256 binds the gitignored
// full/observations.jsonl. A verifier row's Bytes in Fired remains 0 for the
// v1 committed format; the full output keeps its now root-independent count.
type censusResults struct {
	Schema         string              `json:"schema"`
	ManifestSHA256 string              `json:"manifest_sha256"`
	FullSHA256     string              `json:"full_sha256"`
	Aggregates     []censusAggregate   `json:"aggregates"`
	Fired          []censusObservation `json:"fired"`
}

// censusAggregate is one arm's outcome counts on one carrier of one stratum.
// Each observation counts in exactly one of Refused (a tool refusal, never a
// denominator), AdversarialFixture (a file classed adversarial-fixture, kept
// out of every benign count, BC5), Unobserved (armUnobserved) or
// Observations. Observations splits into Trailer, Blocked, CallBlocked and
// the identical remainder. Files counts the files behind Observations, which
// is Observations for a file carrier and 0 for search (an observation per
// query). Suppressed counts the Blocked observations whose block removed at
// least one tag (F6), and TrailerBytes sums the trailer bytes added.
type censusAggregate struct {
	Stratum            string `json:"stratum"`
	Carrier            string `json:"carrier"`
	Arm                string `json:"arm"`
	Observations       int    `json:"observations"`
	Files              int    `json:"files"`
	Trailer            int    `json:"trailer"`
	Blocked            int    `json:"blocked"`
	CallBlocked        int    `json:"call_blocked"`
	Suppressed         int    `json:"suppressed"`
	TrailerBytes       int    `json:"trailer_bytes"`
	AdversarialFixture int    `json:"adversarial_fixture"`
	Refused            int    `json:"refused"`
	Unobserved         int    `json:"unobserved"`
}

// aggregateCensus derives every arm's outcome on every observation and
// aggregates them, sorted by (stratum, carrier, arm); Fired is sorted by
// (stratum, carrier, path). The digests are left for the caller to bind.
func aggregateCensus(obs []censusObservation, m censusManifest) censusResults {
	class := map[[2]string]string{}
	for _, s := range m.Strata {
		for _, f := range s.Files {
			class[[2]string{s.Name, f.Path}] = f.Class
		}
	}
	arms := interceptorArms()
	cells := map[[3]string]*censusAggregate{}
	r := censusResults{Schema: censusResultsSchema, Aggregates: []censusAggregate{}, Fired: []censusObservation{}}
	for _, o := range obs {
		if len(o.Findings) > 0 {
			fired := o
			if fired.Carrier == "verifier" {
				fired.Bytes = 0 // preserve the v1 committed format
			}
			r.Fired = append(r.Fired, fired)
		}
		fixture := o.Carrier != "search" && class[[2]string{o.Stratum, o.Path}] == censusAdversarial
		for _, arm := range arms {
			k := [3]string{o.Stratum, o.Carrier, arm.Name}
			a := cells[k]
			if a == nil {
				a = &censusAggregate{Stratum: o.Stratum, Carrier: o.Carrier, Arm: arm.Name}
				cells[k] = a
			}
			out, trailerBytes, suppressed := deriveArm(o, arm)
			switch {
			case o.Refused != "":
				a.Refused++
				continue
			case fixture:
				a.AdversarialFixture++
				continue
			case out == armUnobserved:
				a.Unobserved++
				continue
			}
			a.Observations++
			if o.Carrier != "search" {
				a.Files++
			}
			switch out {
			case armTrailer:
				a.Trailer++
				a.TrailerBytes += trailerBytes
			case armBlocked:
				a.Blocked++
				if suppressed > 0 {
					a.Suppressed++
				}
			case armCallBlocked:
				a.CallBlocked++
			}
		}
	}
	for _, a := range cells {
		r.Aggregates = append(r.Aggregates, *a)
	}
	slices.SortFunc(r.Aggregates, func(x, y censusAggregate) int {
		return cmp.Or(cmp.Compare(x.Stratum, y.Stratum), cmp.Compare(x.Carrier, y.Carrier), cmp.Compare(x.Arm, y.Arm))
	})
	slices.SortFunc(r.Fired, func(x, y censusObservation) int {
		return cmp.Or(cmp.Compare(x.Stratum, y.Stratum), cmp.Compare(x.Carrier, y.Carrier), cmp.Compare(x.Path, y.Path))
	})
	return r
}

// marshalCensusResults is the committed byte form: indented JSON plus a
// trailing newline.
func marshalCensusResults(r censusResults) ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	return append(b, '\n'), err
}

// loadCensusResults strictly decodes a census-results file.
func loadCensusResults(path string) (censusResults, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return censusResults{}, fmt.Errorf("census results: %w", err)
	}
	var r censusResults
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return censusResults{}, fmt.Errorf("census results %s: %w", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return censusResults{}, fmt.Errorf("census results %s: trailing data after the results", path)
	}
	if r.Schema != censusResultsSchema {
		return censusResults{}, fmt.Errorf("census results %s: schema %q, want %q", path, r.Schema, censusResultsSchema)
	}
	return r, nil
}
