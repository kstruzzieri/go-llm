package agent_test

import (
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
)

func TestHardeningContracts(t *testing.T) {
	started := time.Now()
	// Child cleanup completes before the sole elapsed assertion, including the
	// fixture loading and setup performed inside the bridge and active groups.
	t.Run("Active", func(t *testing.T) {
		agent.RunHardeningBoundaryContracts(t)
	})
	for _, boundary := range []string{
		"ZT-602_#431_project_trust", "ZT-603_#432_MCP_description_catalog_trust",
		"ZT-604_#433_terminal_output", "ZT-605_#434_quarantine",
		"ZT-606_#435_retrieval_screening",
	} {
		t.Run(boundary, func(t *testing.T) { t.Skip("deferred boundary coverage; tracked separately") })
	}
	elapsed := time.Since(started)
	t.Logf("hardening contracts elapsed: %s", elapsed)
	if elapsed >= 500*time.Millisecond {
		t.Errorf("hardening contracts elapsed = %s, want < 500ms", elapsed)
	}
}
