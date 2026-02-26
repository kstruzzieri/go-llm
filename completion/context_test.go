package completion

import "testing"

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{name: "empty string", input: "", want: 0},
		{name: "single char", input: "a", want: 1},
		{name: "four chars", input: "abcd", want: 1},
		{name: "five chars", input: "abcde", want: 2},
		{name: "eight chars", input: "abcdefgh", want: 2},
		{name: "twelve chars", input: "abcdefghijkl", want: 3},
		{name: "code snippet", input: "func main() {}", want: 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EstimateTokens(tt.input)
			if got != tt.want {
				t.Errorf("EstimateTokens(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestTruncateToTokens(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		maxTokens int
		want      string
	}{
		{name: "empty string", input: "", maxTokens: 10, want: ""},
		{name: "zero tokens", input: "hello", maxTokens: 0, want: ""},
		{name: "negative tokens", input: "hello", maxTokens: -1, want: ""},
		{name: "within budget", input: "abcd", maxTokens: 1, want: "abcd"},
		{name: "exact fit", input: "abcdefgh", maxTokens: 2, want: "abcdefgh"},
		{name: "truncates to last chars", input: "0123456789abcd", maxTokens: 2, want: "6789abcd"},
		{name: "single token budget", input: "hello world", maxTokens: 1, want: "orld"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateToTokens(tt.input, tt.maxTokens)
			if got != tt.want {
				t.Errorf("TruncateToTokens(%q, %d) = %q, want %q", tt.input, tt.maxTokens, got, tt.want)
			}
		})
	}
}
