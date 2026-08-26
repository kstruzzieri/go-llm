package config

import "fmt"

// UnbindUseCase removes a use-case route from defaults. An unknown use
// case is refused loudly (use_case_not_found) — unbinding a route that is
// not bound is caller staleness, mirroring role_not_found. go-llm imposes
// no floor on which use cases must exist; consumer floors (e.g. Firn's
// agent route) stay host-side. Chain validity of the remaining defaults
// rides the finalize gate.
func (d *Document) UnbindUseCase(useCase string) error {
	return d.mutate(func(a *Config) error {
		// Inside the closure so the central read-only gate wins for
		// invalid requests too (established pattern).
		if useCase == "" {
			return diagWrap(CodeInvalidArgument, SubjectNone, "",
				fmt.Errorf("config: unbind use case: empty use case"))
		}
		if _, ok := a.Defaults[useCase]; !ok {
			return diagWrap(CodeUseCaseNotFound, SubjectUseCase, useCase,
				fmt.Errorf("config: unbind use case: %q not bound", useCase))
		}
		delete(a.Defaults, useCase)
		return nil
	})
}
