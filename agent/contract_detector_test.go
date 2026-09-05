package agent_test

import (
	"context"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agent/interceptor"
)

// TestToolTrustContractIsCleanUnderDefaultDetectors: a run with the default
// detectors on must not start with a finding against its own base contract.
func TestToolTrustContractIsCleanUnderDefaultDetectors(t *testing.T) {
	for _, ic := range interceptor.Defaults() {
		findings, err := ic.InspectInput(context.Background(), agent.InputInspection{Step: 0, System: agent.ToolTrustContract})
		if err != nil {
			t.Fatalf("%s: %v", ic.Name(), err)
		}
		if len(findings) != 0 {
			t.Errorf("%s: base contract triggers %+v", ic.Name(), findings)
		}
	}
}
