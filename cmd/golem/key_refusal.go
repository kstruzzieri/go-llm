package main

import (
	"fmt"

	"github.com/kstruzzieri/go-llm/config"
)

// keyRefusalRemedy is the Golem-side way out of config.CodeKeyReferenceRefused:
// the library names the refusal, the host names its own flag.
const keyRefusalRemedy = "pass -config ./models.json to use this file's key references, or set GO_LLM_CONFIG"

// withKeyRefusalRemedy appends keyRefusalRemedy to a working-directory key
// refusal and returns every other error unchanged. The wrapper keeps
// errors.Is and config.DiagnosticOf working through %w.
func withKeyRefusalRemedy(err error) error {
	if d, ok := config.DiagnosticOf(err); ok && d.Code == config.CodeKeyReferenceRefused {
		return fmt.Errorf("%w\n%s", err, keyRefusalRemedy)
	}
	return err
}
