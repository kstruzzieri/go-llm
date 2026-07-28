package modeltext

import "testing"

func TestStripCodeFence(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain go fence", "```go\nfunc x() {}\n```", "func x() {}"},
		{"no lang fence", "```\nabc\n```", "abc"},
		{"unfenced unchanged", "func x() {}", "func x() {}"},
		{"leading text not fenced", "here:\n```\nabc\n```", "here:\n```\nabc\n```"},
		{"multi-block markdown untouched", "```go\na\n```\ntext\n```go\nb\n```", "```go\na\n```\ntext\n```go\nb\n```"},
		{"single line backticks untouched", "```justthis", "```justthis"},
		{"empty wrapping fence", "```go\n   \n```", ""},
		{"json fence", "```json\n{\"a\":1}\n```", `{"a":1}`},
	}
	for _, c := range cases {
		if got := StripCodeFence(c.in); got != c.want {
			t.Errorf("%s: StripCodeFence(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
