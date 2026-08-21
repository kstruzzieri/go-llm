package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

var errTestVeto = errors.New("test veto")

func canonicalOf(t *testing.T, d *Document) []byte {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	out, err := d.canonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Failure at the FINALIZE stage (not argument validation) must roll back
// everything: authored, effective, revision, origin.
func TestMutateRollsBackOnFinalizeFailure(t *testing.T) {
	d := loadTestDoc(t, docNestedConfig)
	beforeAuthored := d.authored.clone()
	beforeEffective := d.effective.clone()
	beforeRev, beforeOrigin := d.Revision(), d.Origin()

	err := d.mutate(func(a *Config) error {
		a.Defaults["chat"] = "ghost"
		return nil
	}, nil)
	if err == nil {
		t.Fatal("want finalize failure")
	}
	if !reflect.DeepEqual(d.authored, beforeAuthored) || !reflect.DeepEqual(d.effective, beforeEffective) {
		t.Fatal("finalize failure leaked state")
	}
	if d.Revision() != beforeRev || d.Origin() != beforeOrigin {
		t.Fatal("finalize failure changed revision/origin")
	}
}

// The post-finalize hook sees the FINALIZED effective candidate and can veto.
func TestMutatePostHookVetoRollsBack(t *testing.T) {
	d := loadTestDoc(t, docNestedConfig)
	before := d.authored.clone()
	err := d.mutate(
		func(a *Config) error { a.Defaults["agent"] = "fast"; return nil },
		func(effective *Config) error { return errTestVeto },
	)
	if err == nil || !strings.Contains(err.Error(), "veto") {
		t.Fatalf("err = %v, want veto", err)
	}
	if !reflect.DeepEqual(d.authored, before) {
		t.Fatal("vetoed mutation leaked")
	}
}

func TestBindUseCaseRederivesEffective(t *testing.T) {
	d := loadTestDoc(t, docNestedConfig)
	if err := d.BindUseCase("agent", "fast"); err != nil {
		t.Fatal(err)
	}
	if got := d.Config().Defaults["agent"]; got != "fast" {
		t.Fatalf("effective defaults[agent] = %q, want fast", got)
	}
	if !strings.Contains(string(canonicalOf(t, d)), `"agent": "fast"`) {
		t.Fatal("canonical bytes missing the bound role")
	}
}

func TestBindUseCaseMissingRole(t *testing.T) {
	d := loadTestDoc(t, docNestedConfig)
	before := d.authored.clone()
	if err := d.BindUseCase("agent", "ghost"); err == nil {
		t.Fatal("want error")
	}
	if !reflect.DeepEqual(d.authored, before) {
		t.Fatal("failed bind mutated authored")
	}
}
