package main

import (
	"context"
	"fmt"
	"os"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

// retrieveOpts selects how retrieve is enabled. Exactly one of the modes
// applies: noRag, explicit ragDB, or auto-discovery (autoDBPath+sidecar).
type retrieveOpts struct {
	noRag           bool
	ragDB           string // explicit -rag-db path
	autoDBPath      string // per-workspace index DB path
	autoSidecarPath string // per-workspace sidecar path
	workspaceID     string // workspace:<sha16> for sidecar validation
}

// retrieveResult is the startup outcome. line is the positive disclosure to show
// when retrieve is registered; warns are problems to surface; suppressNotice
// silences the generic "no index" line whenever a more specific outcome already
// explains the situation (-no-rag, explicit -rag-db, or an existing-but-disabled
// auto index). It stays false only when there genuinely is no usable index.
type retrieveResult struct {
	tool           agent.Tool
	line           string
	warns          []string
	suppressNotice bool
}

// enableRetrieve resolves the retrieve tool per spec §6.
func enableRetrieve(ctx context.Context, cfg *config.Config, router *provider.Router, opts retrieveOpts) retrieveResult {
	expected := expectedVectorSpaces(cfg)

	if opts.noRag {
		return retrieveResult{suppressNotice: true}
	}

	if opts.ragDB != "" {
		tool, dec, _, err := buildGatedRetriever(ctx, cfg, router, opts.ragDB, expected)
		if err != nil {
			return retrieveResult{warns: []string{"retrieve disabled: " + err.Error()}, suppressNotice: true}
		}
		if tool == nil {
			return retrieveResult{warns: []string{explicitMismatchWarning(opts.ragDB, dec, expected)}, suppressNotice: true}
		}
		return retrieveResult{tool: tool, line: "retrieve: rag-db " + opts.ragDB, suppressNotice: true,
			warns: legacyWarnIfAny(dec)}
	}

	// Auto-discovery: require both the DB and a valid sidecar.
	if _, err := os.Stat(opts.autoDBPath); err != nil {
		return retrieveResult{} // no index; generic notice applies
	}
	sc, err := readSidecar(opts.autoSidecarPath)
	if err != nil {
		return retrieveResult{} // missing/corrupt sidecar => not trusted; generic notice
	}
	if verr := validateSidecar(sc, opts.workspaceID); verr != nil {
		return retrieveResult{}
	}
	tool, dec, stats, err := buildGatedRetriever(ctx, cfg, router, opts.autoDBPath, expected)
	if err != nil {
		// An index exists but could not be opened/probed: a specific warning
		// already explains why, so suppress the contradictory generic "no index"
		// notice (mirrors the explicit -rag-db branches above).
		return retrieveResult{warns: []string{"retrieve disabled: " + err.Error()}, suppressNotice: true}
	}
	if tool == nil {
		// Index exists but the vector-space gate disabled it: the mismatch warning
		// stands alone; suppress the generic "no index" notice.
		return retrieveResult{warns: []string{autoMismatchWarning(dec, expected)}, suppressNotice: true}
	}
	return retrieveResult{tool: tool, line: autoLine(sc, stats), warns: legacyWarnIfAny(dec)}
}

// expectedVectorSpaces returns the provider-qualified vsid set the current
// embedding chain could produce (empty when none is configured).
func expectedVectorSpaces(cfg *config.Config) []string {
	chain, err := embeddingChain(cfg)
	if err != nil {
		return nil
	}
	return chain
}

func legacyWarnIfAny(dec vsDecision) []string {
	if dec.kind == vsLegacy {
		return []string{"retrieve: index has no recorded vector space (legacy corpus); cannot verify it matches the current embedding config"}
	}
	return nil
}

func autoLine(sc indexSidecar, stats rag.StoreStats) string {
	if sc.Status == "partial" {
		return fmt.Sprintf("retrieve: auto index is partial, %d sources, %d errors from last run; rerun \"golem index\"", stats.TotalSources, sc.ErrorCount)
	}
	return fmt.Sprintf("retrieve: auto index, %d sources, %s, updated %s", stats.TotalSources, sc.VectorSpaceID, sc.IndexedAt)
}

func autoMismatchWarning(dec vsDecision, expected []string) string {
	if dec.kind == vsInconsistent {
		return "retrieve disabled: auto index has inconsistent vector spaces; run \"golem index -full\" to rebuild"
	}
	return fmt.Sprintf("retrieve disabled: auto index uses vector space %s, current embedding chain is %v; run \"golem index -full\" to rebuild", dec.stored, expected)
}

func explicitMismatchWarning(dbPath string, dec vsDecision, expected []string) string {
	if dec.kind == vsInconsistent {
		return fmt.Sprintf("retrieve disabled: rag-db %q has inconsistent vector spaces; rebuild it with the current embedding config or remove -rag-db", dbPath)
	}
	return fmt.Sprintf("retrieve disabled: rag-db %q uses vector space %s, current embedding chain is %v; rebuild that DB with the current embedding config or remove -rag-db to use the auto index", dbPath, dec.stored, expected)
}
