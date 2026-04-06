package provider

import "strings"

// Collect wraps a streaming ChatResponse callback to accumulate the
// aggregated final response. Chunks stream to fn in real-time; call
// getFinal after streaming completes (or is cancelled) to retrieve
// the accumulated result.
//
// Streaming callbacks are delta-oriented: resp.Content and resp.Thinking
// contain only the newly emitted text for that chunk. Collect turns
// those deltas into a final aggregated response.
//
// If fn is nil, chunks are accumulated without forwarding.
//
// Usage:
//
//	callback, getFinal := provider.Collect(myHandler)
//	err := p.ChatStream(ctx, req, callback)
//	final := getFinal()
func Collect(fn func(ChatResponse) error) (wrapped func(ChatResponse) error, getFinal func() ChatResponse) {
	var (
		contentBuf  strings.Builder
		thinkingBuf strings.Builder
		result      ChatResponse
	)

	wrapped = func(resp ChatResponse) error {
		if resp.Content != "" {
			contentBuf.WriteString(resp.Content)
		}
		if resp.Thinking != "" {
			thinkingBuf.WriteString(resp.Thinking)
		}

		if resp.Model != "" {
			result.Model = resp.Model
		}
		if resp.Provider != "" {
			result.Provider = resp.Provider
		}

		if len(resp.ToolCalls) > 0 {
			result.ToolCalls = append(result.ToolCalls, resp.ToolCalls...)
		}

		if resp.Done {
			result.Done = true
			result.Partial = resp.Partial
			result.Usage = resp.Usage
			result.Latency = resp.Latency
		}

		if fn != nil {
			return fn(resp)
		}
		return nil
	}

	getFinal = func() ChatResponse {
		result.Content = contentBuf.String()
		result.Thinking = thinkingBuf.String()
		return result
	}

	return wrapped, getFinal
}
