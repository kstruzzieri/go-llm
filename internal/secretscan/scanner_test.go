package secretscan

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestProviderTokens(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, prefix, body string
		kind               Kind
		minimum            int
	}{
		{"openai", "sk-", "aB3_dE7-fG9_hJ2-kL4", OpenAIToken, 17},
		{"github", "ghp_", "aB3dE7fG9hJ2kL4mN6", GitHubToken, 16},
		{"github_pat", "github_pat_", "aB3_dE7_fG9_hJ2_kL4", GitHubToken, 16},
		{"gitlab", "glpat-", "aB3_dE7-fG9_hJ2-kL4", GitLabToken, 16},
		{"slack_bot", "xoxb-", "aB3-dE7-fG9-hJ2", SlackToken, 10},
		{"slack_app", "xoxa-", "aB3-dE7-fG9-hJ2", SlackToken, 10},
		{"slack_user", "xoxp-", "aB3-dE7-fG9-hJ2", SlackToken, 10},
		{"slack_refresh", "xoxr-", "aB3-dE7-fG9-hJ2", SlackToken, 10},
		{"slack_session", "xoxs-", "aB3-dE7-fG9-hJ2", SlackToken, 10},
		{"npm", "npm_", "aB3dE7fG9hJ2kL4mN6", NPMToken, 16},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			value := tt.prefix + tt.body
			checkScan(t, "("+value+")", []Finding{{Kind: tt.kind, Start: 1, End: 1 + len(value)}})
			minimum := tt.prefix + tt.body[:tt.minimum]
			checkScan(t, minimum, []Finding{{Kind: tt.kind, Start: 0, End: len(minimum)}})
			for _, excluded := range []string{
				tt.prefix + tt.body[:tt.minimum-1], strings.ToUpper(tt.prefix) + tt.body,
				"a" + value, "0" + value, tt.prefix + strings.Repeat("xX", tt.minimum),
				tt.prefix + strings.Repeat("*", tt.minimum),
			} {
				checkScan(t, excluded, nil)
			}
		})
	}
}

func checkScan(t *testing.T, input string, want []Finding) {
	t.Helper()
	if got := Scan(input); !reflect.DeepEqual(got, want) {
		t.Errorf("Scan fixture (%d bytes) = %v, want %v", len(input), got, want)
	}
}

func TestGenericCredentials(t *testing.T) {
	t.Parallel()
	value := "aB3dE7fG9hJ2" + "kL4mN6pQ8rS0"
	for _, key := range []string{"api_key", "apikey", "auth_token", "access_token", "refresh_token", "id_token", "token", "secret", "password", "passwd", "private_key", "authorization", "API-KEY"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			for _, syntax := range []struct{ before, after string }{
				{key + "=", ""}, {key + " \t: \t", ", tail"}, {"\"" + key + "\":\"", "\"}"}, {"'" + key + "' = '", "'"},
			} {
				checkScan(t, syntax.before+value+syntax.after, []Finding{{Kind: SecretAssignment, Start: len(syntax.before), End: len(syntax.before) + len(value)}})
			}
			if !ScanAssignment(key, value) {
				t.Error("ScanAssignment high-entropy fixture = false, want true")
			}
		})
	}
	for _, before := range []string{"Bearer ", "bEaReR\t", "Authorization: Bearer  \t"} {
		checkScan(t, before+value, []Finding{{Kind: BearerToken, Start: len(before), End: len(before) + len(value)}})
	}
	for _, scalar := range []string{value[:20], value + "._~+/-=="} {
		checkScan(t, "Bearer "+scalar+",", []Finding{{Kind: BearerToken, Start: 7, End: 7 + len(scalar)}})
	}
	for _, excluded := range []string{
		"my_api_key=" + value, "apitoken=" + value, "api_key_suffix=" + value, "api-key-more=" + value, "0token=" + value,
		"'token\"=" + value, "tokenx=" + value, "token=\"" + value,
		"Bearer " + value[:19], "Bearer\n" + value, "xBearer " + value, "Bearer =" + value, "Bearer " + value + "=tail",
		"Bearer " + strings.Repeat("a", 30), "token=" + strings.Repeat("a", 30),
	} {
		checkScan(t, excluded, nil)
	}
	for _, terminator := range []string{" ", "\t", "\r", "\n", "\v", "\f", ",", ";", "]", "}", "#"} {
		checkScan(t, "token="+value+terminator+"suffix", []Finding{{Kind: SecretAssignment, Start: 6, End: 6 + len(value)}})
	}
	quoted := value + "\\\"tail"
	checkScan(t, "token=\""+quoted+"\" ignored", []Finding{{Kind: SecretAssignment, Start: 7, End: 7 + len(quoted)}})
	for _, value := range []string{"[ReDaCtEd]", "[redacted_secret]", "[redacted_payment_card]", "<redacted>", "${LONG_EXAMPLE_TEMPLATE_NAME}", "{{LONG_EXAMPLE_TEMPLATE_NAME}}", "<LONG_EXAMPLE_TEMPLATE_NAME>", "YOUR_LONG_EXAMPLE_TEMPLATE_NAME", strings.Repeat("xX*", 10), "example", "placeholder", "changeme", "dummy", "sample", "test", "todo"} {
		if ScanAssignment("api_key", value) {
			t.Error("ScanAssignment whole placeholder = true, want false")
		}
		checkScan(t, "token=\""+value+"\"", nil)
		if ScanAssignment("TOKEN", " \t'"+value+"'\r\n") {
			t.Error("ScanAssignment trimmed placeholder = true, want false")
		}
	}
	for _, value := range []string{value + "example", "test" + value, "dev" + value, "sample" + value, "YOUR_name" + value, "${MixedCaseNameWithEntropy123}", "prefix<EXAMPLE>" + value} {
		if !ScanAssignment("token", value) {
			t.Error("ScanAssignment non-placeholder fixture = false, want true")
		}
		checkScan(t, "token=\""+value+"\"", []Finding{{Kind: SecretAssignment, Start: 7, End: 7 + len(value)}})
	}
	if ScanAssignment("prefix_token", value) || ScanAssignment("token", value[:19]) {
		t.Error("ScanAssignment excluded key/value = true, want false")
	}
}

func TestEntropyThreshold(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, body string
		want       bool
	}{
		{"below", "aaabbccddeefghijk", false},
		{"exactly", "aabbccdde fghijkl", true},
		{"above", "abcdefghijklmnop", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := strings.Repeat(strings.ReplaceAll(tt.body, " ", ""), 2)
			if got := ScanAssignment("token", body); got != tt.want {
				t.Errorf("ScanAssignment entropy fixture = %t, want %t", got, tt.want)
			}
			findings := Scan("Bearer " + body)
			if (len(findings) != 0) != tt.want {
				t.Errorf("Scan Bearer entropy fixture = %v, want detected %t", findings, tt.want)
			}
		})
	}
}

func TestScanHasNoPrefixCutoff(t *testing.T) {
	t.Parallel()
	prefix := strings.Repeat("ordinary source\n", 1<<17)
	value := "npm_" + "aB3dE7fG9hJ2" + "kL4mN6pQ8rS0"
	checkScan(t, prefix+value, []Finding{{Kind: NPMToken, Start: len(prefix), End: len(prefix) + len(value)}})
}

func TestPrivateKeysAndRedaction(t *testing.T) {
	t.Parallel()
	for _, label := range []string{"PRIVATE KEY", "ENCRYPTED PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY", "DSA PRIVATE KEY", "OPENSSH PRIVATE KEY"} {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			for _, newline := range []string{"\n", "\r\n"} {
				begin, end := envelopeMarkers(label)
				value := begin + newline + "placeholder" + newline + end
				input := "before\n" + value + "\nafter"
				want := []Finding{{Kind: PrivateKey, Start: 7, End: 7 + len(value)}}
				checkScan(t, input, want)
				if got := Redact(input, Scan(input)); got != "before\n[REDACTED_SECRET]"+newline+newline+"\nafter" {
					t.Error("Redact private envelope did not preserve surrounding text and line breaks")
				}
				checkScan(t, begin+newline+"placeholder", nil)
				checkScan(t, "placeholder"+newline+end, nil)
				_, otherEnd := envelopeMarkers("OTHER PRIVATE KEY")
				checkScan(t, begin+newline+"placeholder"+newline+otherEnd, nil)
			}
		})
	}
	if got := Redact("unchanged", nil); got != "unchanged" {
		t.Error("Redact with no findings changed text")
	}
}

func envelopeMarkers(label string) (string, string) {
	return strings.Join([]string{"-----", "BEGIN ", label, "-----"}, ""), strings.Join([]string{"-----", "END ", label, "-----"}, "")
}

func TestPaymentCards(t *testing.T) {
	t.Parallel()
	base := "453201987" + "654321098"
	for i, check := range "7905963" {
		value := base[:12+i] + string(check)
		checkScan(t, "("+value+")", []Finding{{Kind: PaymentCard, Start: 1, End: 1 + len(value)}})
		formatted := strings.Join(strings.Split(value, ""), " --  ")
		checkScan(t, " -- "+formatted+" -- ", []Finding{{Kind: PaymentCard, Start: 4, End: 4 + len(formatted)}})
		checkScan(t, value[:len(value)-1]+string('0'+(check-'0'+1)%10), nil)
	}
	for _, value := range []string{base[:12], base + "34", base + " -- 34", strings.Repeat("1", 16), strings.Repeat("0", 16), "0" + base + "3", base + "3" + "0", strings.Repeat("1 - ", 10000) + "1"} {
		checkScan(t, value, nil)
	}
	for _, parts := range [][2]string{{"41111111", "11111111"}, {"42424242", "42424242"}, {"55555555", "55554444"}, {"37828224", "6310005"}, {"60111111", "11111117"}, {"3056930", "9025904"}, {"35660020", "20360505"}} {
		checkScan(t, parts[0]+parts[1], nil)
		checkScan(t, parts[0]+" - "+parts[1], nil)
	}
	card := base[:15] + "5"
	input := "token=" + card
	findings := Scan(input)
	checkScan(t, input, []Finding{{Kind: PaymentCard, Start: 6, End: 6 + len(card)}})
	redacted := Redact(input, findings)
	if redacted != "token=[REDACTED_PAYMENT_CARD]" {
		t.Error("Redact card assignment used wrong marker or coverage")
	}
	checkScan(t, redacted, nil)
	if Redact(redacted, Scan(redacted)) != redacted {
		t.Error("Redact card assignment is not idempotent")
	}
}

func TestRedactDoesNotCreateAssignment(t *testing.T) {
	t.Parallel()
	card := strings.Repeat("0", 5) + "12" + "0" + "7" + strings.Repeat("0", 9)
	value := strings.Repeat("0", 6) + "\\" + card + "A" + strings.Repeat("0", 3) + "1"
	input := "token=\"" + value + "\""
	findings := Scan(input)
	if len(findings) != 1 || findings[0].Kind != PaymentCard {
		t.Fatalf("Scan low-entropy scalar = %v, want one card finding", findings)
	}
	checkScan(t, Redact(input, findings), nil)
}

func TestOverlapResolution(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		input, want []Finding
	}{
		{"narrow priority full union", []Finding{{Kind: SecretAssignment, Start: 0, End: 20}, {Kind: OpenAIToken, Start: 3, End: 15}}, []Finding{{Kind: OpenAIToken, Start: 0, End: 20}}},
		{"transitive", []Finding{{Kind: SecretAssignment, Start: 0, End: 3}, {Kind: PrivateKey, Start: 2, End: 6}, {Kind: PaymentCard, Start: 5, End: 12}}, []Finding{{Kind: PrivateKey, Start: 0, End: 12}}},
		{"adjacent source order", []Finding{{Kind: PaymentCard, Start: 8, End: 12}, {Kind: OpenAIToken, Start: 0, End: 8}}, []Finding{{Kind: OpenAIToken, Start: 0, End: 8}, {Kind: PaymentCard, Start: 8, End: 12}}},
		{"provider tie", []Finding{{Kind: OpenAIToken, Start: 0, End: 20}, {Kind: GitHubToken, Start: 3, End: 15}, {Kind: GitLabToken, Start: 2, End: 18}}, []Finding{{Kind: GitHubToken, Start: 0, End: 20}}},
		{"bearer over card", []Finding{{Kind: PaymentCard, Start: 0, End: 20}, {Kind: BearerToken, Start: 3, End: 15}}, []Finding{{Kind: BearerToken, Start: 0, End: 20}}},
		{"card over assignment", []Finding{{Kind: SecretAssignment, Start: 0, End: 20}, {Kind: PaymentCard, Start: 3, End: 15}}, []Finding{{Kind: PaymentCard, Start: 0, End: 20}}},
		{"same kind ties", []Finding{{Kind: OpenAIToken, Start: 3, End: 15}, {Kind: OpenAIToken, Start: 0, End: 20}, {Kind: OpenAIToken, Start: 0, End: 15}}, []Finding{{Kind: OpenAIToken, Start: 0, End: 20}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := resolve(tt.input); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("resolve spans = %v, want %v", got, tt.want)
			}
		})
	}
	body := "aB3dE7fG9hJ2" + "kL4mN6pQ8rS0"
	provider := "sk-" + body
	for _, newline := range []string{"\n", "\r\n", "\r"} {
		value := "prefix /" + provider + newline + "tail"
		input := "token=\"" + value + "\""
		want := []Finding{{Kind: OpenAIToken, Start: 7, End: 7 + len(value)}}
		checkScan(t, input, want)
		redacted := Redact(input, Scan(input))
		if redacted != "token=\"[REDACTED_SECRET]"+newline+"\"" {
			t.Error("Redact overlapping assignment narrowed union or changed line breaks")
		}
		checkScan(t, redacted, nil)
	}
	input := "Bearer " + provider + " / npm_" + body
	checkScan(t, input, []Finding{{Kind: OpenAIToken, Start: 7, End: 7 + len(provider)}, {Kind: NPMToken, Start: 10 + len(provider), End: len(input)}})
	card := "45320198" + "7654321" + "5"
	value := "aB3dE7fG9hJ2!" + card + "\r\ntail"
	input = "token='" + value + "'"
	checkScan(t, input, []Finding{{Kind: PaymentCard, Start: 7, End: 7 + len(value)}})
	if got := Redact(input, Scan(input)); got != "token='[REDACTED_PAYMENT_CARD]\r\n'" {
		t.Error("Redact canonical card union changed marker or line breaks")
	}
}

func FuzzScan(f *testing.F) {
	for _, seed := range []string{"", "\r\n\n\r", "[REDACTED_PAYMENT_CARD]", "${EXAMPLE_TOKEN}", "\"'\\,;]}#", "token=", "Bearer ", "sk-", " -- 123 -- "} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, suffix string) {
		body := "aB3dE7fG9hJ2" + "kL4mN6pQ8rS0"
		begin, end := envelopeMarkers("PRIVATE KEY")
		for _, input := range []string{suffix, "token=\"" + suffix + "\"", "Bearer " + suffix, "token=\"prefix /sk-" + body + suffix + "\"", begin + "\r\n" + suffix + "\n" + end} {
			findings := Scan(input)
			if !reflect.DeepEqual(findings, Scan(input)) {
				t.Fatal("Scan is nondeterministic")
			}
			lastEnd := 0
			for _, finding := range findings {
				if finding.Start < lastEnd || finding.Start >= finding.End || finding.End > len(input) {
					t.Fatalf("Scan returned invalid span: %v", finding)
				}
				lastEnd = finding.End
			}
			redacted := Redact(input, findings)
			if lineBreaks(input) != lineBreaks(redacted) {
				t.Fatal("Redact changed CR/LF sequence")
			}
			remaining := Scan(redacted)
			if len(remaining) != 0 {
				t.Fatalf("Scan after complete redaction = %v, want no findings", remaining)
			}
			if Redact(redacted, remaining) != redacted {
				t.Fatal("Redact is not idempotent")
			}
		}
	})
}

func lineBreaks(value string) string {
	var out strings.Builder
	for i := range len(value) {
		if value[i] == '\r' || value[i] == '\n' {
			out.WriteByte(value[i])
		}
	}
	return out.String()
}

func BenchmarkScan(b *testing.B) {
	body := "aB3dE7fG9hJ2" + "kL4mN6pQ8rS0"
	begin, _ := envelopeMarkers("PRIVATE KEY")
	for _, sample := range []struct{ name, text string }{
		{"clean_source", "func add(a, b int) int { return a + b }\n"},
		{"many_candidates", "token=\"" + body + "\"\nBearer " + body + "\nsk-" + body + "\n"},
		{"incomplete_envelopes", begin + "\nplaceholder\n"},
		{"digit_separator_run", "1234567890 --  -- "},
	} {
		for _, size := range []int{64 << 10, 1 << 20, 8 << 20} {
			b.Run(fmt.Sprintf("%s/%dKiB", sample.name, size>>10), func(b *testing.B) {
				input := strings.Repeat(sample.text, size/len(sample.text)+1)[:size]
				b.SetBytes(int64(len(input)))
				b.ReportAllocs()
				for b.Loop() {
					Scan(input)
				}
			})
		}
	}
}

func BenchmarkRedact(b *testing.B) {
	card := strings.Repeat("0", 5) + "12" + "0" + "7" + strings.Repeat("0", 9)
	promotion := "token=\"" + strings.Repeat("0", 6) + "\\" + card + "A" + strings.Repeat("0", 3) + "1\"\n"
	for _, sample := range []struct{ name, text string }{
		{"clean_source", "func add(a, b int) int { return a + b }\n"},
		{"card_assignment_promotion", promotion},
	} {
		for _, size := range []int{64 << 10, 1 << 20, 8 << 20} {
			b.Run(fmt.Sprintf("%s/%dKiB", sample.name, size>>10), func(b *testing.B) {
				input := strings.Repeat(sample.text, size/len(sample.text)+1)[:size]
				findings := Scan(input)
				b.SetBytes(int64(len(input)))
				b.ReportAllocs()
				for b.Loop() {
					Redact(input, findings)
				}
			})
		}
	}
}
