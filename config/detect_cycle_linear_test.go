package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// denseDiamondConfig builds `levels` layers of two roles (aN, bN) where both
// roles of layer i fall back to both roles of layer i+1. Path count from the
// top is 2^levels; without done-memoization in detectCycle, validate walks
// every path and goes exponential.
func denseDiamondConfig(levels int, close bool) *Config {
	cfg := &Config{
		Providers: map[string]ProviderConfig{"p": {BaseURL: "http://h"}},
		Models:    map[string]ModelConfig{},
		Defaults:  map[string]string{},
	}
	for i := 0; i < levels; i++ {
		var fb []string
		if i < levels-1 {
			fb = []string{fmt.Sprintf("a%d", i+1), fmt.Sprintf("b%d", i+1)}
		} else if close {
			fb = []string{"a0"} // deep back-edge: cycle through every layer
		}
		for _, side := range []string{"a", "b"} {
			cfg.Models[fmt.Sprintf("%s%d", side, i)] = ModelConfig{
				Name: "m", Type: "dense", Provider: "p", Fallbacks: fb,
			}
		}
	}
	return cfg
}

// TestDetectCycleLinearOnDenseDAG pins the three-color DFS: a 40-level dense
// diamond DAG (2^40 paths) must validate in linear time. Before the done
// memo, 26 levels already ran for minutes; the deadline fails loudly if the
// exponential walk ever comes back.
func TestDetectCycleLinearOnDenseDAG(t *testing.T) {
	cfg := denseDiamondConfig(40, false)
	start := time.Now()
	if err := cfg.validate(); err != nil {
		t.Fatalf("dense DAG must validate: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("validate took %v on a dense DAG — cycle detection is exponential again", elapsed)
	}
}

// TestDetectCycleStillFindsDeepCycle proves the memoization did not blind
// the detector: a back-edge from the deepest layer to the top is a cycle
// through every layer and must still be rejected.
func TestDetectCycleStillFindsDeepCycle(t *testing.T) {
	cfg := denseDiamondConfig(40, true)
	err := cfg.validate()
	assertDiag(t, err, CodeModelInvalid, SubjectRole, "a0")
}

// TestSchemaWalkerLeafRuleCoversAllConfigStructTypes pins the
// schemaNodeFor leaf rule against every named struct type reachable from
// Config: a struct is a leaf if and only if it (or its pointer) implements
// json.Marshaler. A future custom-marshal type that implements only
// encoding.TextMarshaler (or nothing) would be walked as a struct and
// silently mis-merged — this sweep forces that mismatch to fail here
// instead (Gemini review, PR #423).
func TestSchemaWalkerLeafRuleCoversAllConfigStructTypes(t *testing.T) {
	marshaler := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	seen := map[reflect.Type]bool{}
	var sweep func(t reflect.Type)
	sweep = func(rt reflect.Type) {
		for rt.Kind() == reflect.Ptr || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Map {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct || seen[rt] {
			return
		}
		seen[rt] = true
		node := schemaNodeFor(rt)
		hasMarshaler := reflect.PointerTo(rt).Implements(marshaler)
		if hasMarshaler && node != nil {
			t.Fatalf("%v implements json.Marshaler but the walker treats it as a struct", rt)
		}
		if !hasMarshaler && node == nil {
			t.Fatalf("%v has no custom marshaler but the walker treats it as a leaf — its fields would be mis-merged", rt)
		}
		for i := 0; i < rt.NumField(); i++ {
			sweep(rt.Field(i).Type)
		}
	}
	sweep(reflect.TypeOf(Config{}))
	// The sweep must have covered the known custom-marshal leaf and the
	// known walked structs, or it silently proved nothing.
	for _, want := range []reflect.Type{
		reflect.TypeOf(Duration{}),
		reflect.TypeOf(SamplingOptions{}),
		reflect.TypeOf(ThinkTagsConfig{}),
	} {
		if !seen[want] {
			t.Fatalf("sweep never reached %v", want)
		}
	}
}
