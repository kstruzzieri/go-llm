// Package secretscan detects credential and payment-card spans without returning
// their contents. Scanning is local, deterministic, and has no size cutoff.
package secretscan

import (
	"math"
	"slices"
	"strings"
)

// Kind identifies a supported detector and the policy applied to its finding.
type Kind string

// Supported finding kinds.
const (
	OpenAIToken      Kind = "openai_token"
	GitHubToken      Kind = "github_token"
	GitLabToken      Kind = "gitlab_token"
	SlackToken       Kind = "slack_token"
	NPMToken         Kind = "npm_token"
	BearerToken      Kind = "bearer_token"
	SecretAssignment Kind = "secret_assignment"
	PrivateKey       Kind = "private_key"
	PaymentCard      Kind = "payment_card"
)

// Finding is a kind and zero-based byte interval [Start, End). It holds no
// matched text and is safe to include in diagnostics.
type Finding struct {
	Kind  Kind
	Start int
	End   int
}

// Scan examines the complete text and returns findings in source order.
func Scan(text string) []Finding {
	var findings []Finding
	for _, rule := range []struct {
		prefix  string
		kind    Kind
		minimum int
		extra   string
	}{
		{"sk-", OpenAIToken, 17, "_-"},
		{"ghp_", GitHubToken, 16, ""},
		{"github_pat_", GitHubToken, 16, "_"},
		{"glpat-", GitLabToken, 16, "_-"},
		{"xoxb-", SlackToken, 10, "-"},
		{"xoxa-", SlackToken, 10, "-"},
		{"xoxp-", SlackToken, 10, "-"},
		{"xoxr-", SlackToken, 10, "-"},
		{"xoxs-", SlackToken, 10, "-"},
		{"npm_", NPMToken, 16, ""},
	} {
		for next := 0; next < len(text); {
			i := strings.Index(text[next:], rule.prefix)
			if i < 0 {
				break
			}
			start := next + i
			body := start + len(rule.prefix)
			end := body
			for end < len(text) && alphabet(text[end], rule.extra) {
				end++
			}
			next = end
			if start > 0 && alphabet(text[start-1], rule.extra) {
				continue
			}
			if end-body >= rule.minimum && !mask(text[body:end]) {
				findings = append(findings, Finding{Kind: rule.kind, Start: start, End: end})
			}
		}
	}
	findings = scanBearer(text, findings)
	findings = scanAssignments(text, findings)
	findings = scanPrivateKeys(text, findings)
	findings = scanCards(text, findings)
	return resolve(findings)
}

func resolve(findings []Finding) []Finding {
	slices.SortFunc(findings, func(a, b Finding) int {
		if a.Start != b.Start {
			return a.Start - b.Start
		}
		if a.End != b.End {
			return a.End - b.End
		}
		return strings.Compare(string(a.Kind), string(b.Kind))
	})
	resolved := findings[:0]
	for _, finding := range findings {
		if len(resolved) == 0 || finding.Start >= resolved[len(resolved)-1].End {
			resolved = append(resolved, finding)
			continue
		}
		group := &resolved[len(resolved)-1]
		if priority(finding.Kind) < priority(group.Kind) || priority(finding.Kind) == priority(group.Kind) && finding.Kind < group.Kind {
			group.Kind = finding.Kind
		}
		group.End = max(group.End, finding.End)
	}
	return resolved
}

func priority(kind Kind) int {
	switch kind {
	case PrivateKey:
		return 0
	case OpenAIToken, GitHubToken, GitLabToken, SlackToken, NPMToken:
		return 1
	case BearerToken:
		return 2
	case PaymentCard:
		return 3
	default:
		return 4
	}
}

func scanCards(text string, findings []Finding) []Finding {
	for i := 0; i < len(text); {
		if text[i] < '0' || text[i] > '9' {
			i++
			continue
		}
		start, end, count := i, i, 0
		var digits [19]byte
		for i < len(text) {
			c := text[i]
			if c >= '0' && c <= '9' {
				if count < len(digits) {
					digits[count] = c
				}
				count++
				end = i + 1
			} else if c != ' ' && c != '-' {
				break
			}
			i++
		}
		if count < 13 || count > 19 || !validCard(digits[:count]) {
			continue
		}
		findings = append(findings, Finding{Kind: PaymentCard, Start: start, End: end})
	}
	return findings
}

func validCard(digits []byte) bool {
	var sum int
	repeated := true
	for i, digit := range digits {
		repeated = repeated && digit == digits[0]
		n := int(digit - '0')
		if (len(digits)-i)%2 == 0 {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
	}
	if repeated || sum%10 != 0 {
		return false
	}
	switch string(digits) {
	case "41111111" + "11111111", "42424242" + "42424242", "55555555" + "55554444", "37828224" + "6310005", "60111111" + "11111117", "3056930" + "9025904", "35660020" + "20360505":
		return false
	}
	return true
}

// Redact consumes the complete findings returned by Scan and preserves every
// removed CR and LF byte. Marker insertion can raise an enclosing assignment's
// entropy, so redaction also removes any newly detected scalar until clean.
// It does not accept arbitrary spans or partial findings.
func Redact(text string, findings []Finding) string {
	for len(findings) != 0 {
		text = replaceSpans(text, findings)
		// Markers cannot form new provider, Bearer, key-envelope, or card
		// candidates. Any newly detected assignment loses its complete value
		// on the next pass, leaving an exempt marker and its outer delimiters.
		findings = Scan(text)
	}
	return text
}

func replaceSpans(text string, findings []Finding) string {
	var out strings.Builder
	start := 0
	for _, finding := range findings {
		out.WriteString(text[start:finding.Start])
		if finding.Kind == PaymentCard {
			out.WriteString("[REDACTED_PAYMENT_CARD]")
		} else {
			out.WriteString("[REDACTED_SECRET]")
		}
		for i := finding.Start; i < finding.End; i++ {
			if text[i] == '\r' || text[i] == '\n' {
				out.WriteByte(text[i])
			}
		}
		start = finding.End
	}
	out.WriteString(text[start:])
	return out.String()
}

func scanPrivateKeys(text string, findings []Finding) []Finding {
	for _, label := range []string{"PRIVATE KEY", "ENCRYPTED PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY", "DSA PRIVATE KEY", "OPENSSH PRIVATE KEY"} {
		begin, end := "-----BEGIN "+label+"-----", "-----END "+label+"-----"
		for next := 0; next < len(text); {
			start := strings.Index(text[next:], begin)
			if start < 0 {
				break
			}
			start += next
			body := start + len(begin)
			finish := strings.Index(text[body:], end)
			if finish < 0 {
				break
			}
			next = body + finish + len(end)
			findings = append(findings, Finding{Kind: PrivateKey, Start: start, End: next})
		}
	}
	return findings
}

// ScanAssignment classifies an already decoded string member using the same
// key, entropy, and placeholder rules as raw text assignments.
func ScanAssignment(key, value string) bool {
	return assignmentKey(key) && credential(value)
}

func assignmentKey(key string) bool {
	if len(key) < len("token") || len(key) > len("authorization") {
		return false
	}
	var normalized [len("authorization")]byte
	for i := range len(key) {
		c := key[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c == '-' {
			c = '_'
		}
		normalized[i] = c
	}
	switch string(normalized[:len(key)]) {
	case "api_key", "apikey", "auth_token", "access_token", "refresh_token", "id_token", "token", "secret", "password", "passwd", "private_key", "authorization":
		return true
	}
	return false
}

func credential(value string) bool {
	if len(value) < 20 || exempt(value) {
		return false
	}
	var counts [256]int
	for i := range len(value) {
		counts[value[i]]++
	}
	var entropy float64
	for _, count := range counts {
		if count == 0 {
			continue
		}
		p := float64(count) / float64(len(value))
		entropy -= p * math.Log2(p)
	}
	return entropy >= 3.5
}

func exempt(value string) bool {
	value = strings.Trim(value, " \t\r\n\v\f")
	if len(value) >= 2 && (value[0] == '\'' || value[0] == '"') && value[0] == value[len(value)-1] {
		value = value[1 : len(value)-1]
	}
	switch strings.ToLower(value) {
	case "[redacted]", "[redacted_secret]", "[redacted_payment_card]", "<redacted>", "example", "placeholder", "changeme", "dummy", "sample", "test", "todo":
		return true
	}
	if mask(value) {
		return true
	}
	var name string
	switch {
	case strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}"):
		name = value[2 : len(value)-1]
	case strings.HasPrefix(value, "{{") && strings.HasSuffix(value, "}}"):
		name = value[2 : len(value)-2]
	case strings.HasPrefix(value, "<") && strings.HasSuffix(value, ">"):
		name = value[1 : len(value)-1]
	case strings.HasPrefix(value, "YOUR_"):
		name = value[5:]
	}
	if name == "" || name[0] < 'A' || name[0] > 'Z' {
		return false
	}
	for i := range len(name) {
		if !(name[i] >= 'A' && name[i] <= 'Z' || name[i] >= '0' && name[i] <= '9' || name[i] == '_') {
			return false
		}
	}
	return true
}

func scanBearer(text string, findings []Finding) []Finding {
	for i := 0; i+6 < len(text); i++ {
		if (text[i] != 'b' && text[i] != 'B') || !strings.EqualFold(text[i:i+6], "Bearer") {
			continue
		}
		if i > 0 && alphabet(text[i-1], "_-") {
			continue
		}
		start := i + 6
		if !horizontalSpace(text[start]) {
			continue
		}
		for start < len(text) && horizontalSpace(text[start]) {
			start++
		}
		end := start
		for end < len(text) && alphabet(text[end], "._~+/-") {
			end++
		}
		bodyEnd := end
		for end < len(text) && text[end] == '=' {
			end++
		}
		i = max(i+6, end-1)
		if bodyEnd == start || end < len(text) && alphabet(text[end], "._~+/-") {
			continue
		}
		if credential(text[start:end]) {
			findings = append(findings, Finding{Kind: BearerToken, Start: start, End: end})
		}
	}
	return findings
}

func scanAssignments(text string, findings []Finding) []Finding {
	for i := 0; i < len(text); {
		if !alphabet(text[i], "_-") {
			i++
			continue
		}
		keyStart := i
		for i < len(text) && alphabet(text[i], "_-") {
			i++
		}
		if !assignmentKey(text[keyStart:i]) {
			continue
		}
		keyEnd := i
		if keyStart > 0 && (text[keyStart-1] == '\'' || text[keyStart-1] == '"') {
			if i == len(text) || text[i] != text[keyStart-1] {
				continue
			}
			i++
			if keyStart > 1 && alphabet(text[keyStart-2], "_-") {
				continue
			}
		}
		for i < len(text) && horizontalSpace(text[i]) {
			i++
		}
		if i == len(text) || text[i] != '=' && text[i] != ':' {
			continue
		}
		i++
		for i < len(text) && horizontalSpace(text[i]) {
			i++
		}
		if i == len(text) {
			break
		}
		start, end := i, i
		if text[i] == '\'' || text[i] == '"' {
			quote := text[i]
			start++
			end = start
			for end < len(text) && text[end] != quote {
				if text[end] == '\\' && end+1 < len(text) {
					end++
				}
				end++
			}
			i = min(end+1, len(text))
			if end == len(text) {
				continue
			}
		} else {
			for end < len(text) && !scalarBoundary(text[end]) {
				end++
			}
			// Closing template/marker delimiters belong to the complete
			// placeholder, even where they normally delimit an unquoted scalar.
			for n := 1; n <= 2 && end+n <= len(text); n++ {
				if exempt(text[start:end+n]) && (end+n == len(text) || scalarBoundary(text[end+n])) {
					end += n
					break
				}
			}
			i = max(end, start+1)
		}
		if ScanAssignment(text[keyStart:keyEnd], text[start:end]) {
			findings = append(findings, Finding{Kind: SecretAssignment, Start: start, End: end})
		}
	}
	return findings
}

func horizontalSpace(c byte) bool { return c == ' ' || c == '\t' }

func scalarBoundary(c byte) bool { return strings.IndexByte(" \t\r\n\v\f,;]}#", c) >= 0 }

func alphabet(c byte, extra string) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(extra, c) >= 0
}

func mask(value string) bool {
	if value == "" {
		return false
	}
	for i := range len(value) {
		if value[i] != 'x' && value[i] != 'X' && value[i] != '*' {
			return false
		}
	}
	return true
}
