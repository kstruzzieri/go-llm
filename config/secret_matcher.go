package config

import "strings"

// secretMinBytes is the fixed membership floor, in Go string bytes. Shorter
// configured values get no exact protection (#424 spec D1).
const secretMinBytes = 8

// SecretMatcher returns a function reporting whether s contains, as a
// case-sensitive contiguous substring, any credential this configuration
// resolved. Members are every effective provider api_key plus each value an
// ${NAME} reference in that key resolved to during finalization, kept when
// at least eight bytes long and deduplicated by exact bytes. Environment
// variable names, authored templates and unreferenced environment values are
// never members. Components are kept only while their provider still holds
// the key they produced, so a copy that removes or replaces a provider drops
// them; a serialized config loses them entirely.
//
// The result is never nil: a nil receiver or an empty set yields an
// always-false function. It captures a private copy of the set, performs no
// lookup or I/O when called, and is safe for concurrent use. It is a
// trusted host API; never expose it, or its answers, to a model.
func (c *Config) SecretMatcher() func(string) bool {
	var members []string
	if c != nil {
		seen := map[string]bool{}
		add := func(v string) {
			if len(v) >= secretMinBytes && !seen[v] {
				seen[v] = true
				members = append(members, v)
			}
		}
		for name, p := range c.Providers {
			add(p.APIKey)
			if rk, ok := c.resolved[name]; ok && rk.effective == p.APIKey {
				for _, v := range rk.components {
					add(v)
				}
			}
		}
	}
	return func(s string) bool {
		for _, m := range members {
			if strings.Contains(s, m) {
				return true
			}
		}
		return false
	}
}
