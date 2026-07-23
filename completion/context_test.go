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
		// "ab" + "世" (3 bytes) + "界" (3 bytes) = 8 bytes. Budget=1 token=4 bytes.
		// Cut at byte 4 lands mid-rune in "界", so advance to byte 5 → keep "界".
		{name: "utf8 rune boundary", input: "ab世界", maxTokens: 1, want: "界"},
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

func TestTruncateSuffixToTokens(t *testing.T) {
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
		{name: "truncates to first chars", input: "0123456789abcd", maxTokens: 2, want: "01234567"},
		{name: "single token budget", input: "hello world", maxTokens: 1, want: "hell"},
		// "世界abc" = 6+3 = 9 bytes. Budget=1 token=4 bytes.
		// Cut at byte 4 would split "界" (bytes 3-5), so walk back to byte 3 → keep "世".
		{name: "utf8 rune boundary", input: "世界abc", maxTokens: 1, want: "世"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateSuffixToTokens(tt.input, tt.maxTokens)
			if got != tt.want {
				t.Errorf("TruncateSuffixToTokens(%q, %d) = %q, want %q", tt.input, tt.maxTokens, got, tt.want)
			}
		})
	}
}
