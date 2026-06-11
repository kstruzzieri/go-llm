package main

import (
	"reflect"
	"testing"
)

func TestClassifyTrace_AllStates(t *testing.T) {
	top := []string{"gemma", "coder", "qwen36", "glm"}

	cases := []struct {
		name  string
		q     map[string]float64
		floor float64
		fok   bool
		want  discriminationState
	}{
		{
			name: "saturated — every top model 1.0",
			q:    map[string]float64{"gemma": 1.0, "coder": 1.0, "qwen36": 1.0, "glm": 1.0},
			floor: 0.0, fok: true, want: stateSaturated,
		},
		{
			name: "valid discriminator — top splits and one solved it",
			q:    map[string]float64{"gemma": 1.0, "coder": 0.5, "qwen36": 0.5, "glm": 0.5},
			floor: 0.0, fok: true, want: stateValidDiscriminator,
		},
		{
			name: "unsolved — top splits but none reached 1.0",
			q:    map[string]float64{"gemma": 0.5, "coder": 0.0, "qwen36": 0.5, "glm": 0.0},
			floor: 0.0, fok: true, want: stateUnsolved,
		},
		{
			name: "floor-only — top tied below 1.0, floor differs",
			q:    map[string]float64{"gemma": 0.5, "coder": 0.5, "qwen36": 0.5, "glm": 0.5},
			floor: 0.0, fok: true, want: stateFloorOnly,
		},
		{
			name: "no-signal — all five tied below 1.0",
			q:    map[string]float64{"gemma": 0.5, "coder": 0.5, "qwen36": 0.5, "glm": 0.5},
			floor: 0.5, fok: true, want: stateNoSignal,
		},
		{
			name: "unpaired — a top model has no label",
			q:    map[string]float64{"gemma": 1.0, "coder": 1.0, "qwen36": 1.0},
			floor: 0.0, fok: true, want: stateUnpaired,
		},
		{
			name: "unpaired — floor missing at step 4 (cannot separate floor-only from no-signal)",
			q:    map[string]float64{"gemma": 0.5, "coder": 0.5, "qwen36": 0.5, "glm": 0.5},
			floor: 0.0, fok: false, want: stateUnpaired,
		},
		{
			name: "precedence — saturated takes priority over any floor reading",
			q:    map[string]float64{"gemma": 1.0, "coder": 1.0, "qwen36": 1.0, "glm": 1.0},
			floor: 0.5, fok: true, want: stateSaturated,
		},
		{
			name: "valid discriminator — split with two at 1.0",
			q:    map[string]float64{"gemma": 1.0, "coder": 1.0, "qwen36": 0.5, "glm": 0.0},
			floor: 0.0, fok: true, want: stateValidDiscriminator,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyTrace(c.q, top, c.floor, c.fok)
			if got != c.want {
				t.Fatalf("classifyTrace = %q; want %q", got, c.want)
			}
		})
	}
}

func TestDiscriminationStates_Enumerated(t *testing.T) {
	// Lock the state set so a renamed/added state is a deliberate change.
	want := []discriminationState{
		stateValidDiscriminator, stateSaturated, stateUnsolved,
		stateFloorOnly, stateNoSignal, stateUnpaired,
	}
	if !reflect.DeepEqual(allDiscriminationStates(), want) {
		t.Fatalf("allDiscriminationStates() = %v; want %v", allDiscriminationStates(), want)
	}
}
