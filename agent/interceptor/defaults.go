package interceptor

import "github.com/kstruzzieri/go-llm/agent"

// Defaults returns the three detection interceptors with default phrases, in
// the order they run. They are value types safe for concurrent use.
func Defaults() []agent.Interceptor {
	return []agent.Interceptor{ZeroWidth{}, Encoding{}, Typoglycemia{}}
}
