package main

// Corpus balance gate (#331 slice 3c, Task 12): the pre-label gate over the
// FROZEN mixed corpus. mixedBookkeeping computes these figures as reporting
// only; this test hardens them into rejection against the committed fixture's
// registered end state — 5 strata of 14 primaries each, exactly 6 controls
// capped at 2 per stratum, exactly 12 topline carriers equal to the
// registered selector's output, the scenario-family floors, the twin
// contract, and the conversation_only answer-position thirds. Several checks
// double what validateMixedFixture already rejects (defense in depth); the
// per-stratum primary count, the control and topline TOTALS, the family
// floors, and the thirds floors are enforced nowhere else — a corpus missing
// an entire stratum validates cleanly and only fails here.

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestMixedCorpusBalance(t *testing.T) {
	root := corpusRepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "docs", "llm", "assembly-corpus", "mixed", "mixed-cases.json"))
	if err != nil {
		t.Fatalf("read mixed fixture: %v", err)
	}
	f, err := parseMixedFixture(raw)
	if err != nil {
		t.Fatalf("parse mixed fixture: %v", err)
	}
	// The balance assertions below presume a fixture the -assembly-build path
	// accepts; run the registered validation first so a rejected corpus fails
	// here with the validator's message instead of a misleading count.
	if _, err := validateMixedFixture(f); err != nil {
		t.Fatalf("validate mixed fixture: %v", err)
	}

	primaries := map[string]int{}
	controls := map[string]int{}
	totalControls := 0
	for _, c := range f.Cases {
		if c.Control {
			controls[c.Stratum]++
			totalControls++
			continue
		}
		primaries[c.Stratum]++
	}

	t.Run("five strata of fourteen primaries", func(t *testing.T) {
		if got, want := len(primaries), len(assemblyMixedRegisteredStrata); got != want {
			t.Errorf("corpus populates %d strata with primary cases; the registered set has %d", got, want)
		}
		for _, s := range assemblyMixedRegisteredStrata {
			if got := primaries[s]; got != 14 {
				t.Errorf("stratum %q holds %d primary case(s); the frozen corpus registers 14 per stratum", s, got)
			}
		}
	})

	t.Run("six controls capped at two per stratum", func(t *testing.T) {
		if totalControls != 6 {
			t.Errorf("corpus holds %d control case(s); the frozen corpus registers 6", totalControls)
		}
		for _, s := range assemblyMixedRegisteredStrata {
			if got := controls[s]; got > 2 {
				t.Errorf("stratum %q holds %d control case(s); at most 2 are allowed", s, got)
			}
		}
	})

	t.Run("twelve topline carriers equal the registered selection", func(t *testing.T) {
		selected, _ := mixedToplineSelection(f.Cases)
		if got := len(selected); got != 12 {
			t.Errorf("registered topline selection covers %d case(s); the frozen corpus registers 12", got)
		}
		carriers := map[string]struct{}{}
		for _, c := range f.Cases {
			if c.ToplineFacts != "" {
				carriers[c.ID] = struct{}{}
			}
		}
		for _, id := range sortedIDs(selected) {
			if _, ok := carriers[id]; !ok {
				t.Errorf("case %q: the registered selection includes it but the fixture carries no topline_facts", id)
			}
		}
		for _, id := range sortedIDs(carriers) {
			if _, ok := selected[id]; !ok {
				t.Errorf("case %q carries topline_facts outside the registered selection", id)
			}
		}
	})

	t.Run("scenario families meet the per-stratum floors", func(t *testing.T) {
		famCases := map[string]map[string]int{}
		for _, c := range f.Cases {
			if c.Control {
				continue
			}
			if c.ScenarioFamily == "" {
				t.Errorf("case %q: primary case without a scenario_family", c.ID)
				continue
			}
			fams := famCases[c.Stratum]
			if fams == nil {
				fams = map[string]int{}
				famCases[c.Stratum] = fams
			}
			fams[c.ScenarioFamily]++
		}
		for _, s := range assemblyMixedRegisteredStrata {
			fams := famCases[s]
			if len(fams) < 6 {
				t.Errorf("stratum %q holds %d distinct scenario family(ies); the floor is 6", s, len(fams))
			}
			names := make([]string, 0, len(fams))
			for fam := range fams {
				names = append(names, fam)
			}
			sort.Strings(names)
			for _, fam := range names {
				if n := fams[fam]; n > 3 {
					t.Errorf("stratum %q scenario_family %q holds %d case(s); no family may exceed 3", s, fam, n)
				}
			}
		}
	})

	t.Run("twin groups stay within the contract", func(t *testing.T) {
		groupStrata := map[string]map[string]struct{}{}
		for _, c := range f.Cases {
			if c.TwinGroup == "" {
				continue
			}
			if _, ok := mixedTwinStrata[c.Stratum]; !ok {
				t.Errorf("twin_group %q: case %q sits in stratum %q outside the allowed twin strata", c.TwinGroup, c.ID, c.Stratum)
			}
			strata := groupStrata[c.TwinGroup]
			if strata == nil {
				strata = map[string]struct{}{}
				groupStrata[c.TwinGroup] = strata
			}
			if _, dup := strata[c.Stratum]; dup {
				t.Errorf("twin_group %q holds two members in stratum %q; twin members must sit in distinct strata", c.TwinGroup, c.Stratum)
			}
			strata[c.Stratum] = struct{}{}
		}
		if len(groupStrata) > 4 {
			t.Errorf("corpus declares %d twin group(s); at most 4 are allowed", len(groupStrata))
		}
		for _, name := range sortedIDs(groupStrata) {
			if n := len(groupStrata[name]); n < 2 || n > 3 {
				t.Errorf("twin_group %q has %d member(s); a twin group is 2-3 cases", name, n)
			}
		}
	})

	t.Run("conversation_only answer thirds balanced", func(t *testing.T) {
		thirds := map[string]int{}
		declared := 0
		for _, c := range f.Cases {
			if c.Control || c.Stratum != "conversation_only" {
				continue
			}
			if c.AnswerTurnIndex == nil {
				t.Errorf("case %q: conversation_only primary without answer_turn_index (the thirds bookkeeping needs every primary)", c.ID)
				continue
			}
			thirds[mixedAnswerThird(c)]++
			declared++
		}
		if declared != 14 {
			t.Errorf("%d conversation_only primary case(s) declare answer_turn_index; the frozen corpus registers 14", declared)
		}
		for _, third := range []string{"early", "middle", "late"} {
			if got := thirds[third]; got < 4 {
				t.Errorf("conversation_only answer third %q holds %d primary case(s); the floor is 4 of 14", third, got)
			}
		}
	})
}

// sortedIDs returns a map's string keys sorted, so balance failures report in
// a deterministic order.
func sortedIDs[V any](m map[string]V) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
