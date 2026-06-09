package main

// corpusFilter pairs a loaded manifest with a selection so the offline scorers
// (-manual-report, -paired-report) can drop judge-validation and
// non-model-evidence rows exactly as the live-replay path does. A nil
// *corpusFilter means "no manifest" — the scorers behave exactly as before.
type corpusFilter struct {
	Manifest  Manifest
	Selection corpusSelection
}

// tracesFromArtifacts returns the distinct embedded traces from an artifact
// set, in first-seen order, deduped by trace ID. The offline scorers carry the
// original Trace inside every Artifact, so the manifest can be applied to the
// frozen artifacts without re-reading the trace files.
func tracesFromArtifacts(arts []Artifact) []Trace {
	seen := make(map[string]struct{}, len(arts))
	out := make([]Trace, 0, len(arts))
	for _, a := range arts {
		if _, ok := seen[a.Trace.ID]; ok {
			continue
		}
		seen[a.Trace.ID] = struct{}{}
		out = append(out, a.Trace)
	}
	return out
}

// corpusEvidenceFilter resolves which trace IDs survive a manifest selection as
// model evidence. It reuses the exact replay-path machinery — buildCorpusRun
// then modelEvidenceResults — so judge-validation, non-evidence, and
// unselected rows drop identically in the offline scorers. keep holds the
// surviving trace IDs; data preserves selected-but-missing and
// loaded-without-manifest diagnostics; excl reports what was dropped.
func corpusEvidenceFilter(m Manifest, sel corpusSelection, traces []Trace) (keep map[string]struct{}, data *corpusReportData, excl corpusResultExclusions) {
	run, data, _ := buildCorpusRun(m, sel, traces)
	results := make([]Result, 0, len(run))
	for _, tr := range run {
		results = append(results, Result{TraceID: tr.ID})
	}
	kept, excl := modelEvidenceResults(results, data)
	keep = make(map[string]struct{}, len(kept))
	for _, r := range kept {
		keep[r.TraceID] = struct{}{}
	}
	return keep, data, excl
}
