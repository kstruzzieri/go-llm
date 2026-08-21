package config

import "fmt"

// mutate applies fn to a clone of the authored config, re-derives the
// effective view through finalize (the same gate Load applies), runs the
// optional post hook against the FINALIZED effective candidate, and commits
// both views only if every stage succeeds — all under d.mu, so evaluation
// and commit are one atomic step and no save can interleave (saves also
// hold d.mu end-to-end; spec amendment 6). Any error leaves the document
// unchanged. Draft-only: rawBytes/revision/origin never change here.
func (d *Document) mutate(fn func(authored *Config) error, post func(effective *Config) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	authored := d.authored.clone()
	if err := fn(authored); err != nil {
		return err
	}
	effective := authored.clone()
	if err := effective.finalize(); err != nil {
		return err
	}
	if post != nil {
		if err := post(effective); err != nil {
			return err
		}
	}
	d.authored = authored
	d.effective = effective
	return nil
}

// BindUseCase points a use case at an EXISTING role (edits defaults). Role
// existence is checked explicitly; chain validity rides the finalize gate.
func (d *Document) BindUseCase(useCase, role string) error {
	if useCase == "" || role == "" {
		return fmt.Errorf("config: bind use case: empty use case or role")
	}
	return d.mutate(func(a *Config) error {
		if _, ok := a.Models[role]; !ok {
			return fmt.Errorf("config: bind use case %q: role %q not defined", useCase, role)
		}
		if a.Defaults == nil {
			a.Defaults = map[string]string{}
		}
		a.Defaults[useCase] = role
		return nil
	}, nil)
}
