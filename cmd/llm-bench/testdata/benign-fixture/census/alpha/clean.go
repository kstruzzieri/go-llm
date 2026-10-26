package alpha

import "strings"

// Join returns the words joined by a single space.
func Join(words []string) string {
	return strings.Join(words, " ")
}
