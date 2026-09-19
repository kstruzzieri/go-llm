package interceptor

import "github.com/kstruzzieri/go-llm/agent"

// Defaults returns the default chain in the order it runs: the three
// detection interceptors (#436) followed by the two #439 guards, the
// argument invariants (block) and the egress classifier (tag), then Secrets
// (block at every origin). They are value types safe for concurrent use.
func Defaults() []agent.Interceptor {
	return []agent.Interceptor{ZeroWidth{}, Encoding{}, Typoglycemia{}, mustInvariants(DefaultInvariants()), Egress{}, Secrets{}}
}
