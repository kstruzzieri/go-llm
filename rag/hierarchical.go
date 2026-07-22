package rag

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// HierarchicalRetrievalRequest configures an explicit bounded group-first retrieval.
type HierarchicalRetrievalRequest struct {
	Request                                        RetrievalRequest
	CandidateLimit, MaxDepth, MaxGroups, MaxTokens int
	Timeout                                        time.Duration
}

// HierarchicalRetrievalResponse embeds the canonical response and its selection trace.
type HierarchicalRetrievalResponse struct {
	RetrievalResponse
	Trace HierarchicalRetrievalTrace
}

// HierarchicalRetrievalTrace explains group selection and final result budgeting.
type HierarchicalRetrievalTrace struct {
	SearchMode    string
	Groups        []HierarchicalGroupTrace
	SelectedPaths [][]string
	FinalChunks   []HierarchicalChunkTrace
	Skipped       []HierarchicalSkipTrace
	Budget        HierarchicalBudgetTrace
	Policy        RetrievalPolicyOutcome
}

// HierarchicalGroupTrace records one ranked sibling group.
type HierarchicalGroupTrace struct {
	Path           []string
	Kind           string
	Name           string
	RankScore      float64
	CandidateCount int
	Selected       bool
	SkipReason     string
}

// HierarchicalChunkTrace records one final chunk and its trusted freshness.
type HierarchicalChunkTrace struct {
	Result         ScoredResult
	FreshnessKnown bool
	Freshness      DocumentFreshness
}

// HierarchicalSkipTrace aggregates a stable selection skip reason.
type HierarchicalSkipTrace struct {
	Reason string
	Count  int
}

// HierarchicalBudgetTrace records configured bounds and observed use.
type HierarchicalBudgetTrace struct {
	CandidateLimit, MaxDepth, MaxGroups, MaxResults, MaxTokens         int
	Timeout                                                            time.Duration
	DepthReached, InspectedCandidates, ReturnedResults, ReturnedTokens int
}

type hierarchyLevel struct {
	kind, name, key string
}

type hierarchyCandidate struct {
	retrievalCandidate
	path []hierarchyLevel
}

type hierarchyGroup struct {
	level      hierarchyLevel
	path       []string
	candidates []int
	rankScore  float64
}

func validateHierarchicalRequest(req HierarchicalRetrievalRequest) error {
	switch {
	case req.Request.K <= 0:
		return fmt.Errorf("rag: hierarchical K must be > 0")
	case req.CandidateLimit <= req.Request.K:
		return fmt.Errorf("rag: hierarchical CandidateLimit must be > K")
	case req.MaxDepth <= 0:
		return fmt.Errorf("rag: hierarchical MaxDepth must be > 0")
	case req.MaxGroups <= 0:
		return fmt.Errorf("rag: hierarchical MaxGroups must be > 0")
	case req.MaxTokens <= 0:
		return fmt.Errorf("rag: hierarchical MaxTokens must be > 0")
	case req.Timeout <= 0:
		return fmt.Errorf("rag: hierarchical Timeout must be > 0")
	default:
		return nil
	}
}

// RetrieveHierarchical performs opt-in bounded group-first retrieval.
func (r *Retriever) RetrieveHierarchical(ctx context.Context, req HierarchicalRetrievalRequest) (HierarchicalRetrievalResponse, error) {
	if err := validateHierarchicalRequest(req); err != nil {
		return HierarchicalRetrievalResponse{}, err
	}
	opCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()

	trace := HierarchicalRetrievalTrace{
		SearchMode: "multi",
		Budget: HierarchicalBudgetTrace{
			CandidateLimit: req.CandidateLimit,
			MaxDepth:       req.MaxDepth,
			MaxGroups:      req.MaxGroups,
			MaxResults:     req.Request.K,
			MaxTokens:      req.MaxTokens,
			Timeout:        req.Timeout,
		},
	}
	if r.usesDenseSearch() {
		trace.SearchMode = "dense"
	}

	response, err := r.retrieveRequest(opCtx, req.Request, req.CandidateLimit, func(ctx context.Context, input retrievalSelectionInput) ([]ScoredResult, error) {
		trace.Budget.MaxResults = input.finalLimit
		return selectHierarchical(ctx, req, input, &trace)
	})
	if err != nil {
		if contextErr := opCtx.Err(); contextErr != nil && !errors.Is(err, contextErr) {
			err = errors.Join(err, contextErr)
		}
		if cause := context.Cause(opCtx); cause != nil && !errors.Is(err, cause) {
			err = errors.Join(err, cause)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return HierarchicalRetrievalResponse{}, err
		}
		response.Results = nil
		trace.Groups = nil
		trace.SelectedPaths = nil
		trace.FinalChunks = nil
		trace.Skipped = nil
		trace.Budget.DepthReached = 0
		trace.Budget.InspectedCandidates = 0
		trace.Budget.ReturnedResults = 0
		trace.Budget.ReturnedTokens = 0
		trace.Policy = response.Policy
		return HierarchicalRetrievalResponse{RetrievalResponse: response, Trace: trace}, err
	}
	trace.Policy = response.Policy
	return HierarchicalRetrievalResponse{RetrievalResponse: response, Trace: trace}, nil
}

func selectHierarchical(ctx context.Context, req HierarchicalRetrievalRequest, input retrievalSelectionInput, trace *HierarchicalRetrievalTrace) ([]ScoredResult, error) {
	candidates := make([]hierarchyCandidate, len(input.candidates))
	for i, candidate := range input.candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		candidates[i] = hierarchyCandidate{
			retrievalCandidate: candidate,
			path:               candidateHierarchyPath(candidate, req.Request.QueryContext.WorkspaceRoot),
		}
	}
	trace.Budget.InspectedCandidates = len(candidates)

	indices := make([]int, len(candidates))
	for i := range indices {
		indices[i] = i
	}
	selected := make([]int, 0, len(indices))
	selectedPaths := make(map[string][]string)
	groupSkipped := 0
	var visit func([]int, int, []string) error
	visit = func(current []int, depth int, prefix []string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth >= req.MaxDepth {
			selected = append(selected, current...)
			addSelectedPath(selectedPaths, prefix)
			return nil
		}

		groupsByKey := make(map[string]*hierarchyGroup)
		for _, index := range current {
			candidate := candidates[index]
			if depth >= len(candidate.path) {
				selected = append(selected, index)
				addSelectedPath(selectedPaths, prefix)
				continue
			}
			level := candidate.path[depth]
			key := level.kind + "\x00" + level.key
			group := groupsByKey[key]
			if group == nil {
				group = &hierarchyGroup{level: level, path: append(append([]string(nil), prefix...), level.kind+":"+level.key), rankScore: candidate.result.RankScore}
				groupsByKey[key] = group
			}
			group.candidates = append(group.candidates, index)
			if cmp.Compare(candidate.result.RankScore, group.rankScore) > 0 {
				group.rankScore = candidate.result.RankScore
			}
		}

		groups := make([]*hierarchyGroup, 0, len(groupsByKey))
		for _, group := range groupsByKey {
			groups = append(groups, group)
		}
		sort.Slice(groups, func(i, j int) bool {
			if order := cmp.Compare(groups[i].rankScore, groups[j].rankScore); order != 0 {
				return order > 0
			}
			if groups[i].level.kind != groups[j].level.kind {
				return groups[i].level.kind < groups[j].level.kind
			}
			return groups[i].level.key < groups[j].level.key
		})
		if len(groups) != 0 && depth+1 > trace.Budget.DepthReached {
			trace.Budget.DepthReached = depth + 1
		}
		for i, group := range groups {
			chosen := i < req.MaxGroups
			entry := HierarchicalGroupTrace{
				Path: append([]string(nil), group.path...), Kind: group.level.kind, Name: group.level.name,
				RankScore: group.rankScore, CandidateCount: len(group.candidates), Selected: chosen,
			}
			if !chosen {
				entry.SkipReason = "group_limit"
				groupSkipped += len(group.candidates)
			}
			trace.Groups = append(trace.Groups, entry)
			if chosen {
				if err := visit(group.candidates, depth+1, group.path); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(indices, 0, nil); err != nil {
		return nil, err
	}

	sort.Slice(selected, func(i, j int) bool {
		left, right := candidates[selected[i]].result, candidates[selected[j]].result
		if order := cmp.Compare(left.RankScore, right.RankScore); order != 0 {
			return order > 0
		}
		if left.Chunk.Source != right.Chunk.Source {
			return left.Chunk.Source < right.Chunk.Source
		}
		if left.Chunk.StartLine != right.Chunk.StartLine {
			return left.Chunk.StartLine < right.Chunk.StartLine
		}
		return left.Chunk.ID < right.Chunk.ID
	})

	resultLimit := input.finalLimit
	if resultLimit <= 0 {
		resultLimit = req.Request.K
	}
	eligible := selected
	resultSkipped := 0
	if len(eligible) > resultLimit {
		resultSkipped = len(eligible) - resultLimit
		eligible = eligible[:resultLimit]
	}
	returned := make([]ScoredResult, 0, len(eligible))
	returnedTokens := 0
	tokenSkipped := 0
	for i, index := range eligible {
		tokens := estimatedChunkTokens(candidates[index].result.Chunk.Content)
		if tokens > req.MaxTokens-returnedTokens {
			tokenSkipped = len(eligible) - i
			break
		}
		returnedTokens += tokens
		result := cloneScoredResults([]ScoredResult{candidates[index].result})[0]
		returned = append(returned, result)
		trace.FinalChunks = append(trace.FinalChunks, HierarchicalChunkTrace{
			Result:         cloneScoredResults([]ScoredResult{result})[0],
			FreshnessKnown: candidates[index].freshness.known,
			Freshness:      candidates[index].freshness.value,
		})
	}
	trace.Budget.ReturnedResults = len(returned)
	trace.Budget.ReturnedTokens = returnedTokens
	trace.SelectedPaths = sortedSelectedPaths(selectedPaths)
	appendHierarchySkip(&trace.Skipped, "group_limit", groupSkipped)
	appendHierarchySkip(&trace.Skipped, "result_limit", resultSkipped)
	appendHierarchySkip(&trace.Skipped, "token_limit", tokenSkipped)
	appendHierarchySkip(&trace.Skipped, "policy_filtered", input.filteredCount)
	appendHierarchySkip(&trace.Skipped, "stale", input.staleDroppedCount)
	return returned, nil
}

func candidateHierarchyPath(candidate retrievalCandidate, workspaceRoot string) []hierarchyLevel {
	if candidate.freshness.managed {
		document := candidate.freshness.document
		path := []hierarchyLevel{
			{kind: "collection", name: document.collection, key: document.collection},
			{kind: "document", name: document.title, key: document.id},
		}
		if section := candidate.result.Chunk.Metadata["section_path"]; section != "" {
			path = append(path, hierarchyLevel{kind: "section", name: section, key: section})
		}
		return path
	}
	return codeHierarchyPath(candidate.result.Chunk, workspaceRoot)
}

func codeHierarchyPath(chunk Chunk, workspaceRoot string) []hierarchyLevel {
	if looksManagedDocumentSource(chunk.Source) {
		return []hierarchyLevel{{kind: "source", name: chunk.Source, key: chunk.Source}}
	}
	workspace := filepath.Clean(workspaceRoot)
	if workspaceRoot == "" {
		workspace = "."
	}
	cleanSource := normalizePath(chunk.Source, workspaceRoot)
	displaySource := cleanSource
	if relative, err := filepath.Rel(workspace, cleanSource); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
		displaySource = relative
	}
	displaySource = filepath.ToSlash(filepath.Clean(displaySource))
	directory := filepath.ToSlash(filepath.Dir(displaySource))
	file := filepath.Base(displaySource)
	path := []hierarchyLevel{
		{kind: "workspace", name: filepath.ToSlash(workspace), key: filepath.ToSlash(workspace)},
		{kind: "directory", name: directory, key: directory},
		{kind: "file", name: file, key: displaySource},
	}
	if symbol := chunk.Metadata["symbol_path"]; symbol != "" {
		path = append(path, hierarchyLevel{kind: "symbol", name: symbol, key: symbol})
	}
	return path
}

func estimatedChunkTokens(content string) int {
	if content == "" {
		return 0
	}
	return 1 + (len(content)-1)/4
}

func addSelectedPath(paths map[string][]string, path []string) {
	if len(path) == 0 {
		return
	}
	key := strings.Join(path, "\x00")
	if _, exists := paths[key]; !exists {
		paths[key] = append([]string(nil), path...)
	}
}

func sortedSelectedPaths(paths map[string][]string) [][]string {
	keys := make([]string, 0, len(paths))
	for key := range paths {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([][]string, len(keys))
	for i, key := range keys {
		result[i] = append([]string(nil), paths[key]...)
	}
	return result
}

func appendHierarchySkip(skips *[]HierarchicalSkipTrace, reason string, count int) {
	if count > 0 {
		*skips = append(*skips, HierarchicalSkipTrace{Reason: reason, Count: count})
	}
}
