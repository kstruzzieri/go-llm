package conversation

// messageCost computes the estimated token cost of a message using all
// prompt-visible fields.
func messageCost(m Message, estimator TokenEstimator) int {
	cost := estimator(m.Content)
	if len(m.ToolCalls) > 0 {
		cost += estimator(string(m.ToolCalls))
	}
	if m.ToolName != "" {
		cost += estimator(m.ToolName)
	}
	if m.ToolCallID != "" {
		cost += estimator(m.ToolCallID)
	}
	return cost
}

// toolChain represents a contiguous tool-call sequence in the message slice.
type toolChain struct {
	start     int
	end       int
	completed bool
	cost      int
}

// identifyToolChains scans messages and returns all tool-call chains.
func identifyToolChains(msgs []Message, estimator TokenEstimator) []toolChain {
	var chains []toolChain
	i := 0
	for i < len(msgs) {
		m := msgs[i]
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			chain := toolChain{start: i, end: i}
			cost := 0
			if estimator != nil {
				cost = messageCost(m, estimator)
			}
			j := i + 1
			for j < len(msgs) && msgs[j].Role == "tool" {
				chain.end = j
				if estimator != nil {
					cost += messageCost(msgs[j], estimator)
				}
				j++
			}
			chain.cost = cost
			if j < len(msgs) && msgs[j].Role == "assistant" && len(msgs[j].ToolCalls) == 0 {
				chain.completed = true
			}
			chains = append(chains, chain)
			i = j
		} else {
			i++
		}
	}
	return chains
}

// TrimMessages returns the most recent messages that fit within maxTokens.
// Safety invariants are enforced unconditionally, even when all messages fit.
// Panics if estimator is nil.
func TrimMessages(msgs []Message, maxTokens int, estimator TokenEstimator) TrimResult {
	if estimator == nil {
		panic("conversation: TrimMessages requires non-nil TokenEstimator")
	}
	if len(msgs) == 0 {
		return TrimResult{Messages: []Message{}}
	}

	var systemMsgs []Message
	var nonSystem []Message
	systemCost := 0
	for _, m := range msgs {
		if m.Role == "system" {
			systemMsgs = append(systemMsgs, m)
			systemCost += messageCost(m, estimator)
		} else {
			nonSystem = append(nonSystem, m)
		}
	}

	if len(nonSystem) == 0 {
		return TrimResult{
			Messages:        systemMsgs,
			TrimmedCount:    0,
			EstimatedTokens: systemCost,
		}
	}

	budget := maxTokens - systemCost
	if budget < 0 {
		budget = 0
	}

	keep := make([]bool, len(nonSystem))
	for i := range keep {
		keep[i] = true
	}

	currentCost := 0
	for _, m := range nonSystem {
		currentCost += messageCost(m, estimator)
	}

	chains := identifyToolChains(nonSystem, estimator)

	// Determine the unresolved tail boundary including preceding user message.
	unresolvedProtectFrom := -1
	if len(chains) > 0 {
		lastChain := chains[len(chains)-1]
		if !lastChain.completed {
			unresolvedProtectFrom = lastChain.start
			for t := lastChain.start - 1; t >= 0; t-- {
				if nonSystem[t].Role == "user" {
					unresolvedProtectFrom = t
					break
				}
			}
		}
	}

	needsTrimming := currentCost > budget

	if needsTrimming {
		// Tier 1: Remove completed tool chains, oldest first.
		for _, chain := range chains {
			if currentCost <= budget {
				break
			}
			if !chain.completed {
				continue
			}
			for k := chain.start; k <= chain.end; k++ {
				keep[k] = false
			}
			currentCost -= chain.cost
		}

		// Tier 2: Remove remaining messages oldest first, protecting unresolved tail.
		for i := 0; i < len(nonSystem) && currentCost > budget; i++ {
			if !keep[i] {
				continue
			}
			if unresolvedProtectFrom >= 0 && i >= unresolvedProtectFrom {
				continue
			}
			keep[i] = false
			currentCost -= messageCost(nonSystem[i], estimator)
		}
	}

	// Enforce first-non-system-is-user rule unconditionally.
	// Do NOT remove messages in the unresolved tail.
	for i := 0; i < len(nonSystem); i++ {
		if !keep[i] {
			continue
		}
		if nonSystem[i].Role == "user" {
			break
		}
		if unresolvedProtectFrom >= 0 && i >= unresolvedProtectFrom {
			break
		}
		keep[i] = false
		currentCost -= messageCost(nonSystem[i], estimator)
	}

	// Enforce tool-pair atomicity.
	for _, chain := range chains {
		assistantKept := keep[chain.start]
		for k := chain.start; k <= chain.end; k++ {
			keep[k] = assistantKept
		}
	}

	// Ensure at least one non-system message if any existed.
	hasKeptNonSystem := false
	for _, kept := range keep {
		if kept {
			hasKeptNonSystem = true
			break
		}
	}
	if !hasKeptNonSystem && len(nonSystem) > 0 {
		fallback := len(nonSystem) - 1
		for j := len(nonSystem) - 1; j >= 0; j-- {
			if nonSystem[j].Role == "user" {
				fallback = j
				break
			}
		}
		keep[fallback] = true
	}

	// Build result preserving original message order.
	var result []Message
	trimmedCount := 0
	keptCost := systemCost
	nsIdx := 0
	for _, m := range msgs {
		if m.Role == "system" {
			result = append(result, m)
		} else {
			if keep[nsIdx] {
				result = append(result, m)
				keptCost += messageCost(m, estimator)
			} else {
				trimmedCount++
			}
			nsIdx++
		}
	}

	return TrimResult{
		Messages:        result,
		TrimmedCount:    trimmedCount,
		EstimatedTokens: keptCost,
	}
}

// TrimByExchanges keeps the most recent maxExchanges user-assistant exchanges.
// System messages are always preserved. Unresolved tool-call tails are preserved.
// EstimatedTokens is 0 since no estimator is used.
func TrimByExchanges(msgs []Message, maxExchanges int) TrimResult {
	if len(msgs) == 0 {
		return TrimResult{Messages: []Message{}}
	}

	var systemMsgs []Message
	var nonSystem []Message
	for _, m := range msgs {
		if m.Role == "system" {
			systemMsgs = append(systemMsgs, m)
		} else {
			nonSystem = append(nonSystem, m)
		}
	}

	if len(nonSystem) == 0 {
		return TrimResult{Messages: systemMsgs, TrimmedCount: 0}
	}

	type exchange struct {
		start int
		end   int
	}

	unresolvedStart := -1
	chains := identifyToolChains(nonSystem, nil)
	if len(chains) > 0 {
		lastChain := chains[len(chains)-1]
		if !lastChain.completed {
			unresolvedStart = lastChain.start
		}
	}

	resolvedEnd := len(nonSystem)
	if unresolvedStart >= 0 {
		resolvedEnd = unresolvedStart
		for resolvedEnd > 0 && nonSystem[resolvedEnd-1].Role != "user" {
			resolvedEnd--
		}
		if resolvedEnd > 0 {
			resolvedEnd--
		}
	}

	var exchanges []exchange
	i := 0
	for i < resolvedEnd {
		if nonSystem[i].Role == "user" {
			ex := exchange{start: i}
			j := i + 1
			for j < resolvedEnd {
				if nonSystem[j].Role == "assistant" && len(nonSystem[j].ToolCalls) == 0 {
					ex.end = j
					exchanges = append(exchanges, ex)
					j++
					break
				}
				j++
			}
			i = j
		} else {
			i++
		}
	}

	keepFrom := 0
	if maxExchanges >= 0 && maxExchanges < len(exchanges) {
		keepFrom = len(exchanges) - maxExchanges
	}

	keep := make([]bool, len(nonSystem))

	for idx := keepFrom; idx < len(exchanges); idx++ {
		ex := exchanges[idx]
		for k := ex.start; k <= ex.end; k++ {
			keep[k] = true
		}
	}

	if unresolvedStart >= 0 {
		tailStart := unresolvedStart
		for t := unresolvedStart - 1; t >= 0; t-- {
			if nonSystem[t].Role == "user" {
				tailStart = t
				break
			}
		}
		for k := tailStart; k < len(nonSystem); k++ {
			keep[k] = true
		}
	}

	// Build result preserving original message order.
	var result []Message
	trimmedCount := 0
	nsIdx := 0
	for _, m := range msgs {
		if m.Role == "system" {
			result = append(result, m)
		} else {
			if keep[nsIdx] {
				result = append(result, m)
			} else {
				trimmedCount++
			}
			nsIdx++
		}
	}

	return TrimResult{
		Messages:        result,
		TrimmedCount:    trimmedCount,
		EstimatedTokens: 0,
	}
}
