package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

// retrieveOpts selects how retrieve is enabled. Exactly one of the modes
// applies: noRag, explicit ragDB, or auto-discovery (autoDBPath+sidecar).
type retrieveOpts struct {
	noRag       bool
	ragDB       string // explicit -rag-db path
	autoDBPath  string // per-workspace index DB path
	workspaceID string // workspace:<sha16> for sidecar validation
	weighter    rag.BehavioralWeighter
	progressive bool // opt into the #189 progressive renderer
}

// retrieveResult is the startup outcome. reader owns the registered generation
// (nil when retrieve is disabled); it is deliberately the ONLY handle to the
// tool, so every caller must register through a wrapper that admits invokes via
// reader.inflight (readyRetrieve) -- exposing reader.tool raw would let
// shutdown close the store and feedback service under an active retrieval.
// line is the positive disclosure to show when retrieve is registered; warns
// are problems to surface; suppressNotice silences the generic "no index" line
// whenever a more specific outcome already explains the situation (-no-rag,
// explicit -rag-db, or an existing-but-disabled auto index). It stays false
// only when there genuinely is no usable index.
type retrieveResult struct {
	reader         *retrievalReader
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
		reader, dec, _, err := buildGatedRetriever(ctx, cfg, router, opts.ragDB, expected, opts.weighter, opts.progressive)
		if err != nil {
			return retrieveResult{warns: []string{"retrieve disabled: " + err.Error()}, suppressNotice: true}
		}
		if reader == nil {
			return retrieveResult{warns: []string{explicitMismatchWarning(opts.ragDB, dec, expected)}, suppressNotice: true}
		}
		return retrieveResult{reader: reader, line: "retrieve: rag-db " + opts.ragDB, suppressNotice: true,
			warns: legacyWarnIfAny(dec)}
	}

	// Auto-discovery resolves the atomic pointer first and falls back to the
	// immutable legacy DB/sidecar pair only when no pointer exists.
	gen, err := resolveActiveGeneration(ctx, opts.autoDBPath, opts.workspaceID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return retrieveResult{}
		}
		return retrieveResult{warns: []string{"retrieve disabled: " + err.Error()}, suppressNotice: true}
	}
	reader, dec, stats, err := buildGatedRetriever(ctx, cfg, router, gen.dbPath, expected, opts.weighter, opts.progressive)
	if err != nil {
		// An index exists but could not be opened/probed: a specific warning
		// already explains why, so suppress the contradictory generic "no index"
		// notice (mirrors the explicit -rag-db branches above).
		return retrieveResult{warns: []string{"retrieve disabled: " + err.Error()}, suppressNotice: true}
	}
	if reader == nil {
		// Index exists but the vector-space gate disabled it: the mismatch warning
		// stands alone; suppress the generic "no index" notice.
		return retrieveResult{warns: []string{autoMismatchWarning(dec, expected)}, suppressNotice: true}
	}
	return retrieveResult{reader: reader, line: autoGenerationLine(gen.metadata, stats), warns: legacyWarnIfAny(dec)}
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

func autoGenerationLine(metadata generationMetadata, stats rag.StoreStats) string {
	if metadata.Status == "partial" {
		return fmt.Sprintf("retrieve: auto index is partial, %d sources, %d errors from last run; rerun \"golem index\"", stats.TotalSources, metadata.ErrorCount)
	}
	return fmt.Sprintf("retrieve: auto index, %d sources, %s, updated %s", stats.TotalSources, metadata.VectorSpaceID, metadata.IndexedAt)
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
