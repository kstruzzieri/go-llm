package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

// normalizeWS collapses internal whitespace runs to single spaces and trims
// both ends. strings.Fields splits on any Unicode whitespace and drops empties.
func normalizeWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// quoteInChunk reports whether quote appears in the chunk's raw content after
// conservative whitespace normalization. Case-sensitive (code is). Matches
// against rag.Chunk.Content, never BuildContext output: BuildContext prefixes
// each line with "<n>| " (PR #227 line anchors), which would corrupt matching.
//
// Verification is literal substring presence, with no minimum-meaningfulness
// floor: a trivially short cited quote (e.g. "1") can satisfy it. A length
// threshold is intentionally omitted because it would false-negative legitimate
// short identifiers, and the model is the server's own prompt-controlled local
// model rather than an adversary.
func quoteInChunk(chunk rag.Chunk, quote string) bool {
	q := normalizeWS(quote)
	if q == "" {
		return false
	}
	return strings.Contains(normalizeWS(chunk.Content), q)
}

// extractJSONObjects returns every top-level {...} object found in s, in order.
// It tracks string state and backslash escapes so braces inside JSON strings do
// not affect nesting depth. Text between objects is ignored. Byte iteration is
// sufficient because the structural characters are all ASCII.
func extractJSONObjects(s string) []json.RawMessage {
	var out []json.RawMessage
	depth, start := 0, -1
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					out = append(out, json.RawMessage(s[start:i+1]))
					start = -1
				}
			}
		}
	}
	return out
}

// modelAnswer is the ONLY shape the model is asked to produce. status,
// answer_found, sources, and verified are server-owned and never requested.
type modelAnswer struct {
	Answer   string          `json:"answer"`
	Evidence []modelEvidence `json:"evidence"`
}

type modelEvidence struct {
	ID    string `json:"id"`    // short prompt-assigned label E1..En, not the SHA chunk ID
	Quote string `json:"quote"` // span the model claims to have copied verbatim
}

// parseModelAnswer returns the LAST extracted JSON object that decodes into a
// modelAnswer carrying at least an "answer" or "evidence" key. Local models
// sometimes emit example/scaffolding JSON before the real answer, so the final
// valid object is the answer attempt.
func parseModelAnswer(text string) (modelAnswer, bool) {
	var best modelAnswer
	found := false
	for _, raw := range extractJSONObjects(text) {
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			continue
		}
		_, hasAnswer := keys["answer"]
		_, hasEvidence := keys["evidence"]
		if !hasAnswer && !hasEvidence {
			continue
		}
		var ma modelAnswer
		if err := json.Unmarshal(raw, &ma); err != nil {
			continue
		}
		best, found = ma, true
	}
	return best, found
}

const (
	statusAnswerFound           = "answer_found"
	statusUnverifiedAnswer      = "unverified_answer"
	statusNotInRetrievedContext = "not_in_retrieved_context"
	statusMalformedOutput       = "malformed_output"
	statusRetrievalMiss         = "retrieval_miss"  // #230
	statusCorpusAbsent          = "corpus_absent"   // #230
	statusNoUsefulTerms         = "no_useful_terms" // #230
)

// evidenceBlock pairs a prompt-assigned label (E1..En) with the retrieved chunk
// it represents, so verification can match a cited quote against the exact chunk.
type evidenceBlock struct {
	ID    string
	Chunk rag.Chunk
}

type answerSource struct {
	Source    string `json:"source"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

type answerQuote struct {
	ID        string `json:"id"`
	Source    string `json:"source"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Text      string `json:"text"`
}

type answerDiagnostics struct {
	RetrievedChunkIDs       []string `json:"retrieved_chunk_ids,omitempty"`
	EvidenceIDs             []string `json:"evidence_ids,omitempty"`
	VerifiedQuoteIDs        []string `json:"verified_quote_ids,omitempty"`
	CandidateAnswer         string   `json:"candidate_answer,omitempty"`
	KeywordTerms            []string `json:"keyword_terms,omitempty"`              // #230
	CorpusMatchCount        *int     `json:"corpus_match_count,omitempty"`         // #230
	CorpusOutsideMatchCount *int     `json:"corpus_outside_match_count,omitempty"` // #230
}

type ragAnswerResult struct {
	Status             string             `json:"status"`
	AnswerFound        bool               `json:"answer_found"`
	Answer             string             `json:"answer"`
	Sources            []answerSource     `json:"sources"`
	Quotes             []answerQuote      `json:"quotes"`
	Verified           bool               `json:"verified"`
	VerificationErrors []string           `json:"verification_errors"`
	Diagnostics        *answerDiagnostics `json:"diagnostics,omitempty"`
}

// deriveAnswer verifies the model's cited evidence against the built blocks and
// produces the audited result for the #229 status set. A returned status of
// statusNotInRetrievedContext marks the model-declined branch that #230 refines.
func deriveAnswer(ma modelAnswer, blocks []evidenceBlock) ragAnswerResult {
	byID := make(map[string]rag.Chunk, len(blocks))
	for _, bl := range blocks {
		byID[bl.ID] = bl.Chunk
	}

	res := ragAnswerResult{
		Sources:            []answerSource{},
		Quotes:             []answerQuote{},
		VerificationErrors: []string{},
	}

	if strings.TrimSpace(ma.Answer) == "" {
		// Model declined; no-answer branch (#230 may refine).
		res.Status = statusNotInRetrievedContext
		return res
	}

	seenSource := make(map[string]bool)
	for _, ev := range ma.Evidence {
		chunk, ok := byID[ev.ID]
		if !ok {
			res.VerificationErrors = append(res.VerificationErrors, fmt.Sprintf("unknown id %q", ev.ID))
			continue
		}
		if !quoteInChunk(chunk, ev.Quote) {
			res.VerificationErrors = append(res.VerificationErrors, fmt.Sprintf("quote not found in %s", ev.ID))
			continue
		}
		res.Quotes = append(res.Quotes, answerQuote{
			ID: ev.ID, Source: chunk.Source, StartLine: chunk.StartLine, EndLine: chunk.EndLine, Text: ev.Quote,
		})
		key := fmt.Sprintf("%s:%d-%d", chunk.Source, chunk.StartLine, chunk.EndLine)
		if !seenSource[key] {
			seenSource[key] = true
			res.Sources = append(res.Sources, answerSource{Source: chunk.Source, StartLine: chunk.StartLine, EndLine: chunk.EndLine})
		}
	}

	if len(res.Quotes) > 0 {
		res.Status = statusAnswerFound
		res.AnswerFound = true
		res.Verified = true
		res.Answer = strings.TrimSpace(ma.Answer) // public answer only when verified
	} else {
		res.Status = statusUnverifiedAnswer // answer given but unsupported (terminal)
	}
	return res
}

// buildEvidenceBlocks assigns stable E1..En labels to the retrieved chunks and
// formats them for the prompt, honoring a character budget (maxTokens*4, the
// same chars-per-token ratio BuildContext uses). Lower-ranked blocks are dropped
// when the budget would be exceeded; a dropped block is omitted from the
// returned slice so the model cannot cite it. The first block is always kept.
func buildEvidenceBlocks(results []rag.SearchResult, maxTokens int) (string, []evidenceBlock) {
	maxChars := maxTokens * 4
	var b strings.Builder
	var blocks []evidenceBlock
	for i, r := range results {
		id := fmt.Sprintf("E%d", i+1)
		block := fmt.Sprintf("[%s] %s (lines %d-%d)\n%s\n\n", id, r.Chunk.Source, r.Chunk.StartLine, r.Chunk.EndLine, r.Chunk.Content)
		if b.Len() > 0 && b.Len()+len(block) > maxChars {
			break
		}
		b.WriteString(block)
		blocks = append(blocks, evidenceBlock{ID: id, Chunk: r.Chunk})
	}
	return strings.TrimRight(b.String(), "\n"), blocks
}

// buildAnswerPrompt produces the audited-answer chat messages. It describes the
// required JSON as a FIELD LIST, not a valid JSON example object, so a model
// that echoes the instructions cannot itself satisfy the parser.
func buildAnswerPrompt(question, evidence string) []ollama.ChatMessage {
	system := "You are an audited code question-answering assistant. " +
		"Answer ONLY from the numbered evidence blocks below. " +
		"Reply with a single JSON object and nothing else, containing exactly these fields:\n" +
		"- answer: a string. Use the empty string if the evidence does not answer the question.\n" +
		"- evidence: a list of items, each with id (the E-label of the block you used) and " +
		"quote (text copied verbatim from that block).\n" +
		"Do not invent ids or quotes. Copy each quote exactly from the block you cite."
	user := fmt.Sprintf("Question: %s\n\nEvidence:\n%s", question, evidence)
	return []ollama.ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
}

// routeChat executes one chat round via the router at temperature 0 and returns
// the response text. Mirrors the routing in handleChat (tools_chat.go).
func (s *Server) routeChat(ctx context.Context, router routeEngine, model string, msgs []ollama.ChatMessage) (string, error) {
	zero := 0.0
	resolved, err := s.routeModelSelector(ctx, model, "chat")
	if err != nil {
		return "", err
	}
	rr := provider.RoutingRequest{
		Model:          resolved,
		UseCase:        "chat",
		RequiredCaps:   provider.CapChat,
		Messages:       toProviderChatMessages(msgs),
		Options:        provider.ModelOptions{Temperature: &zero},
		ExpectedOutput: provider.DefaultExpectedOutput("chat"),
		Priority:       provider.PriorityNormal,
	}
	if rr.Model == "" {
		chain, cerr := s.chainFor("chat")
		if cerr != nil {
			return "", cerr
		}
		rr.PreferredChain = chain
	}
	plan, err := router.Route(ctx, rr)
	if err != nil {
		return "", err
	}
	resp, err := plan.ExecuteChat(ctx)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// callForAnswer runs the audited-answer model call with one repair retry when
// the first reply does not parse. Returns the parsed answer and whether parsing
// ultimately succeeded.
func (s *Server) callForAnswer(ctx context.Context, router routeEngine, model, question, evidence string) (modelAnswer, bool, error) {
	msgs := buildAnswerPrompt(question, evidence)
	text, err := s.routeChat(ctx, router, model, msgs)
	if err != nil {
		return modelAnswer{}, false, err
	}
	if ma, ok := parseModelAnswer(text); ok {
		return ma, true, nil
	}
	repair := append(msgs,
		ollama.ChatMessage{Role: "assistant", Content: text},
		ollama.ChatMessage{Role: "user", Content: "Your previous reply was not valid JSON. Return ONLY a single valid JSON object with fields answer and evidence, and nothing else."},
	)
	text2, err := s.routeChat(ctx, router, model, repair)
	if err != nil {
		return modelAnswer{}, false, err
	}
	ma, ok := parseModelAnswer(text2)
	return ma, ok, nil
}

func (s *Server) handleRAGAnswer(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	if r := s.requireRAG(); r != nil {
		return r, nil
	}
	var args struct {
		Question           string `json:"question"`
		Model              string `json:"model,omitempty"`
		TopK               int    `json:"top_k,omitempty"`
		IncludeDiagnostics bool   `json:"include_diagnostics,omitempty"`
	}
	if err := decodeManagedRAGArguments(req.Params.Arguments, maxRAGSearchArgumentsBytes, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if args.Question == "" {
		return toolError("validation", "question must not be empty"), nil
	}
	topK := args.TopK
	if topK <= 0 {
		topK = 5
	}

	s.mu.RLock()
	retriever := s.retriever
	store := s.store
	s.mu.RUnlock()
	if retriever == nil {
		return toolError("rag", "retriever unavailable; embedding model may not be resolved"), nil
	}

	policy, _, err := s.retrievalPolicyRequest(ctx, req)
	if err != nil {
		return retrievalPolicyRequestError(err), nil
	}
	response, err := retriever.RetrieveRequest(ctx, rag.RetrievalRequest{
		Query: args.Question, K: topK, Policy: policy,
	})
	if err != nil {
		if result := retrievalPolicyToolError(response.Policy, err); result != nil {
			return result, nil
		}
		return toolError("rag", "retrieve: %v", err), nil
	}
	results := flattenRetrievalResults(response.Results, true)

	var (
		ma     modelAnswer
		blocks []evidenceBlock
		result ragAnswerResult
	)

	if len(results) == 0 {
		// No retrieved context: skip the model entirely.
		result = ragAnswerResult{Status: statusNotInRetrievedContext, Sources: []answerSource{}, Quotes: []answerQuote{}, VerificationErrors: []string{}}
	} else {
		var evidenceText string
		evidenceText, blocks = buildEvidenceBlocks(results, ragContextMaxTokens)

		router := s.routerSnapshot()
		if router == nil {
			return withRetrievalPolicyMeta(toolError("config", "router unavailable"), response.Policy), nil
		}
		var parsed bool
		ma, parsed, err = s.callForAnswer(ctx, router, args.Model, args.Question, evidenceText)
		if err != nil {
			return withRetrievalPolicyMeta(toolError("router", "%v", err), response.Policy), nil
		}
		if !parsed {
			result = ragAnswerResult{Status: statusMalformedOutput, Sources: []answerSource{}, Quotes: []answerQuote{}, VerificationErrors: []string{}}
		} else {
			result = deriveAnswer(ma, blocks)
		}
	}

	var (
		cp        rag.CorpusPresence
		corpusRan bool
	)
	if result.Status == statusNotInRetrievedContext && store != nil && !response.Policy.Applied {
		retrievedIDs := make([]string, 0, len(results))
		for _, r := range results {
			retrievedIDs = append(retrievedIDs, r.Chunk.ID)
		}
		result, cp, corpusRan = s.refineWithCorpus(ctx, store, args.Question, retrievedIDs, result)
	}

	if args.IncludeDiagnostics {
		result.Diagnostics = buildDiagnostics(ma, blocks, result)
		if corpusRan {
			total, outside := cp.TotalMatches, cp.OutsideMatches
			result.Diagnostics.KeywordTerms = cp.Terms
			result.Diagnostics.CorpusMatchCount = &total
			result.Diagnostics.CorpusOutsideMatchCount = &outside
		}
	}

	data, err := json.Marshal(result)
	if err != nil {
		return withRetrievalPolicyMeta(toolError("rag", "marshal result: %v", err), response.Policy), nil
	}
	return withRetrievalPolicyMeta(toolResult(string(data)), response.Policy), nil
}

// corpusKeywordSearcher is the subset of the vector store the corpus-absence
// check needs. Defined at the consumer (Go idiom); SQLiteStore implements it.
type corpusKeywordSearcher interface {
	KeywordPresence(ctx context.Context, query string, retrievedIDs []string) (rag.CorpusPresence, error)
}

// refineWithCorpus deepens the model-declined branch using a deterministic
// keyword check. It returns the refined result and the corpus presence (and
// whether the check ran) for diagnostics. On any error or unsupported store it
// fails open: the status stays not_in_retrieved_context.
func (s *Server) refineWithCorpus(ctx context.Context, store rag.VectorStore, question string, retrievedIDs []string, res ragAnswerResult) (ragAnswerResult, rag.CorpusPresence, bool) {
	searcher, ok := store.(corpusKeywordSearcher)
	if !ok {
		return res, rag.CorpusPresence{}, false
	}
	cp, err := searcher.KeywordPresence(ctx, question, retrievedIDs)
	if err != nil {
		return res, rag.CorpusPresence{}, false // fail open
	}
	switch {
	case len(cp.Terms) == 0:
		res.Status = statusNoUsefulTerms
	case cp.OutsideMatches > 0:
		res.Status = statusRetrievalMiss
	case cp.TotalMatches == 0:
		res.Status = statusCorpusAbsent
	default:
		// Matches exist only inside the retrieved set, yet the model declined:
		// not a retrieval miss. Leave status as not_in_retrieved_context.
	}
	return res, cp, true
}

// buildDiagnostics assembles the opt-in diagnostics block. Corpus fields are
// added by the #230 wiring (Task 11).
func buildDiagnostics(ma modelAnswer, blocks []evidenceBlock, res ragAnswerResult) *answerDiagnostics {
	d := &answerDiagnostics{}
	for _, b := range blocks {
		d.RetrievedChunkIDs = append(d.RetrievedChunkIDs, b.Chunk.ID)
	}
	for _, ev := range ma.Evidence {
		d.EvidenceIDs = append(d.EvidenceIDs, ev.ID)
	}
	for _, q := range res.Quotes {
		d.VerifiedQuoteIDs = append(d.VerifiedQuoteIDs, q.ID)
	}
	if res.Answer == "" && strings.TrimSpace(ma.Answer) != "" {
		d.CandidateAnswer = ma.Answer
	}
	return d
}
