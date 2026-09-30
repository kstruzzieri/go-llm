package consult

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const appTestInitialize = `{"id":1,"method":"initialize","params":{"clientInfo":{"name":"go-llm-consult","version":"1"},"capabilities":{"experimentalApi":false,"requestAttestation":false,"optOutNotificationMethods":["item/agentMessage/delta","item/reasoning/textDelta","item/reasoning/summaryTextDelta","item/reasoning/summaryPartAdded","item/plan/delta"]}}}`
const appTestInitReply = `{"id":1,"result":{"userAgent":"synthetic","codexHome":"/synthetic/native","platformFamily":"unix","platformOs":"macos"}}`
const appTestRemote = `{"method":"remoteControl/status/changed","params":{"status":"disabled","serverName":"synthetic","installationId":"synthetic","environmentId":null}}`
const appTestModel = `{"id":"picker-id","model":"gpt-6-astra","displayName":"Synthetic","description":"synthetic","hidden":true,"isDefault":false,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium","description":"synthetic"}]}`
const appTestThread = `{"id":"thread-one","sessionId":"session-one","model":"gpt-6-astra","modelProvider":"openai","cwd":"/synthetic/consult","source":"vscode","ephemeral":true,"turns":[],"status":{"type":"idle"},"cliVersion":"0.153.4","createdAt":1,"updatedAt":1,"preview":"","projectId":null,"extra":null,"canAcceptDirectInput":true}`
const appTestThreadReply = `{"id":3,"result":{"thread":` + appTestThread + `,"model":"gpt-6-astra","modelProvider":"openai","cwd":"/synthetic/consult","approvalPolicy":"never","approvalsReviewer":"user","sandbox":{"type":"readOnly","networkAccess":false},"runtimeWorkspaceRoots":[],"activePermissionProfile":null,"multiAgentMode":"explicitRequestOnly"}}`
const appTestThreadStarted = `{"method":"thread/started","params":{"thread":` + appTestThread + `}}`
const appTestTurn = `{"id":"turn-one","items":[],"itemsView":"notLoaded","status":"inProgress"}`
const appTestTurnStarted = `{"method":"turn/started","params":{"threadId":"thread-one","turn":` + appTestTurn + `}}`
const appTestTurnReply = `{"id":4,"result":{"turn":` + appTestTurn + `}}`
const appTestAnswer = `{"type":"agentMessage","id":"answer-one","text":"OK.\r\nReady\u001b","phase":"final_answer"}`
const appTestCompleted = `{"method":"item/completed","params":{"threadId":"thread-one","turnId":"turn-one","completedAtMs":3,"item":` + appTestAnswer + `}}`
const appTestTerminal = `{"method":"turn/completed","params":{"threadId":"thread-one","turn":{"id":"turn-one","items":[` + appTestAnswer + `],"itemsView":"summary","status":"completed"}}}`

func appTestRecords() []string {
	return []string{appTestInitReply, appTestRemote, `{"id":2,"result":{"data":[` + appTestModel + `],"nextCursor":null}}`, appTestThreadReply, appTestThreadStarted, appTestTurnStarted, appTestCompleted, appTestTerminal, appTestTurnReply}
}

func appTestSameJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	if len(got) == 0 || got[len(got)-1] != '\n' || strings.Count(string(got), "\n") != 1 {
		t.Fatalf("outbound record has invalid LF framing: %q", got)
	}
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("request = %s; want %s", got, want)
	}
}

func appTestStart(t *testing.T) *appServerStream {
	t.Helper()
	s := &appServerStream{model: "gpt-6-astra", prompt: "Reply OK."}
	a, err := s.start("/synthetic/consult")
	if err != nil || a.closeStdin {
		t.Fatalf("start = %+v, %v", a, err)
	}
	appTestSameJSON(t, a.write, appTestInitialize)
	return s
}

func appTestFeed(t *testing.T, s *appServerStream, records []string) {
	t.Helper()
	for i, record := range records {
		if _, err := s.receive([]byte(record)); err != nil {
			t.Fatalf("record %d rejected: %v", i, err)
		}
	}
}

func TestAppServerRequestsAndTerminalRace(t *testing.T) {
	s := appTestStart(t)
	expected := map[int]string{
		0: `{"method":"initialized"}`,
		1: `{"id":2,"method":"model/list","params":{"includeHidden":true,"limit":100}}`,
		2: `{"id":3,"method":"thread/start","params":{"model":"gpt-6-astra","cwd":"/synthetic/consult","approvalPolicy":"never","approvalsReviewer":"user","sandbox":"read-only","ephemeral":true}}`,
		4: `{"id":4,"method":"turn/start","params":{"threadId":"thread-one","input":[{"type":"text","text":"Reply OK.","text_elements":[]}]}}`,
	}
	for i, record := range appTestRecords() {
		a, err := s.receive([]byte(record))
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if want, ok := expected[i]; ok {
			appTestSameJSON(t, a.write, want)
		} else if len(a.write) != 0 {
			t.Fatalf("record %d sent unexpected request", i)
		}
		if a.closeStdin != (i == 8) {
			t.Fatalf("record %d close=%v; terminal must await correlated start reply", i, a.closeStdin)
		}
		if i < 8 {
			if _, err := s.finish(); err == nil {
				t.Fatalf("record %d admitted early", i)
			}
		}
	}
	facts, err := s.finish()
	if err != nil || facts.Answer != "OK.\nReady�" || facts.UsagePresent || facts.Usage != (usage{}) {
		t.Fatalf("facts = %+v, %v", facts, err)
	}
}

func TestAppServerReplyBeforeTurnStarted(t *testing.T) {
	s := appTestStart(t)
	r := appTestRecords()
	r = append(append(append([]string{}, r[:5]...), appTestTurnReply), r[5:8]...)
	appTestFeed(t, s, r)
	if got, err := s.finish(); err != nil || got.Answer != "OK.\nReady�" {
		t.Fatalf("finish = %+v,%v", got, err)
	}
}

func TestAppServerRejectsMissingAndLateLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name    string
		records []string
	}{
		{"missing start reply", appTestRecords()[:8]},
		{"missing completed item", append(append([]string{}, appTestRecords()[:6]...), appTestTerminal, appTestTurnReply)},
		{"duplicate terminal", append(appTestRecords(), appTestTerminal)},
		{"late action", append(appTestRecords(), `{"method":"item/started","params":{"threadId":"thread-one","turnId":"turn-one","startedAtMs":4,"item":{"id":"tool","type":"commandExecution"}}}`)},
		{"late error", append(appTestRecords(), `{"method":"error","params":{"message":"DO NOT ECHO"}}`)},
		{"wrong response type", append([]string{strings.Replace(appTestInitReply, `"id":1`, `"id":"1"`, 1)}, appTestRecords()[1:]...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := appTestStart(t)
			for _, record := range tc.records {
				_, _ = s.receive([]byte(record))
			}
			got, err := s.finish()
			if err == nil || got != (appServerResult{}) {
				t.Fatalf("invalid stream admitted: %+v,%v", got, err)
			}
		})
	}
}

const appTestRichModel = `{"id":"picker-id","model":"gpt-6-astra","displayName":"Synthetic","description":"synthetic","hidden":true,"isDefault":false,"defaultReasoningEffort":"future-effort","supportedReasoningEfforts":[{"reasoningEffort":"future-effort","description":"synthetic"}],"additionalSpeedTiers":["fast"],"availabilityNux":{"message":"synthetic"},"defaultServiceTier":"standard","inputModalities":["text","image","audio"],"modelSpecialty":"synthetic","multiAgentVersion":"v2","serviceTiers":[{"id":"tier","name":"standard","description":"synthetic"}],"supportsPersonality":true,"upgrade":"other-model","upgradeInfo":{"model":"other-model","migrationMarkdown":"synthetic","modelLink":"synthetic","retirementAt":4,"upgradeCopy":"synthetic"}}`
const appTestRichThread = `{"id":"thread-one","sessionId":"session-one","model":"gpt-6-astra","modelProvider":"openai","cwd":"/synthetic/consult","source":"vscode","ephemeral":true,"turns":[],"status":{"type":"notLoaded"},"cliVersion":"0.153.4","createdAt":1,"updatedAt":0,"preview":"","projectId":"synthetic","extra":{},"canAcceptDirectInput":true,"agentNickname":"synthetic","agentRole":"synthetic","forkedFromId":null,"gitInfo":{"branch":"synthetic","originUrl":"synthetic","sha":"synthetic"},"historyMode":"legacy","name":"synthetic","parentThreadId":null,"path":"synthetic","reasoningEffort":"future-effort","recencyAt":1,"section":{"id":"synthetic","name":"synthetic","appearance":{"color":"synthetic","icon":"synthetic"}},"sectionEnteredAt":1,"threadSource":"synthetic"}`
const appTestRichAnswer = `{"type":"agentMessage","id":"answer-one","text":"Final","phase":null,"delivery":"async","memoryCitation":{"entries":[{"lineStart":0,"lineEnd":4294967295,"note":"synthetic","path":"synthetic"}],"threadIds":["citation-thread"]},"questions":[{"title":"synthetic","options":["one","two"]},{"title":"empty","options":null}]}`
const appTestRateLimits = `{"method":"account/rateLimits/updated","params":{"rateLimits":{"limitId":"synthetic","limitName":"synthetic","primary":{"usedPercent":1,"windowDurationMins":300,"resetsAt":5},"secondary":null,"credits":{"balance":"1","hasCredits":true,"unlimited":false},"planType":"pro","rateLimitReachedType":"rate_limit_reached","spendControlReached":false,"individualLimit":{"limit":"1","used":"0","remainingPercent":100,"resetsAt":5}}}}`
const appTestUsage = `{"method":"thread/tokenUsage/updated","params":{"threadId":"thread-one","turnId":"turn-one","tokenUsage":{"total":{"totalTokens":90,"inputTokens":20,"cachedInputTokens":5,"cacheWriteInputTokens":2,"outputTokens":8,"reasoningOutputTokens":3},"last":{"totalTokens":7,"inputTokens":6,"cachedInputTokens":1,"outputTokens":1,"reasoningOutputTokens":0},"modelContextWindow":100}}}`

func TestAppServerAcceptedMetadata(t *testing.T) {
	s := appTestStart(t)
	r := appTestRecords()
	r[2] = strings.Replace(r[2], appTestModel, appTestRichModel, 1)
	r[3] = strings.Replace(r[3], appTestThread, appTestRichThread, 1)
	r[3] = strings.Replace(r[3], `"runtimeWorkspaceRoots":[]`, `"runtimeWorkspaceRoots":["/synthetic/root"],"instructionSources":["synthetic"],"serviceTier":"standard","reasoningEffort":"future-effort"`, 1)
	r[3] = strings.Replace(r[3], `"activePermissionProfile":null`, `"activePermissionProfile":{"id":"synthetic","extends":"synthetic"}`, 1)
	r[4] = strings.Replace(r[4], appTestThread, appTestRichThread, 1)
	r[6] = strings.Replace(r[6], appTestAnswer, appTestRichAnswer, 1)
	r[7] = strings.Replace(r[7], appTestAnswer, appTestRichAnswer, 1)
	appTestFeed(t, s, r[:3])
	appTestFeed(t, s, []string{
		`{"method":"skills/changed","params":{},"emittedAtMs":null}`,
		appTestRateLimits,
		`{"method":"thread/status/changed","params":{"threadId":"thread-one","status":{"type":"notLoaded"}}}`,
		`{"method":"mcpServer/startupStatus/updated","params":{"name":"synthetic","status":"ready","threadId":"thread-one","error":null,"failureReason":null}}`,
	})
	appTestFeed(t, s, r[3:6])
	appTestFeed(t, s, []string{
		`{"method":"item/started","params":{"threadId":"thread-one","turnId":"turn-one","startedAtMs":1,"item":{"type":"agentMessage","id":"answer-one","text":"","phase":"commentary"}}}`,
		`{"method":"item/completed","params":{"threadId":"thread-one","turnId":"turn-one","completedAtMs":2,"item":{"type":"userMessage","id":"user","clientId":null,"content":[{"type":"text","text":"Reply OK.","text_elements":[]}]}}}`,
		`{"method":"item/completed","params":{"threadId":"thread-one","turnId":"turn-one","completedAtMs":2,"item":{"type":"reasoning","id":"reasoning","summary":["discard"],"content":["discard"]}}}`,
		`{"method":"model/verification","params":{"threadId":"thread-one","turnId":"turn-one","verifications":["trustedAccessForCyber"]}}`,
		`{"method":"model/safetyBuffering/updated","params":{"threadId":"thread-one","turnId":"turn-one","model":"gpt-6-astra","reasons":["synthetic"],"useCases":["synthetic"],"showBufferingUi":true,"fasterModel":"ignored"}}`,
		appTestUsage, appTestUsage,
		`{"method":"thread/status/changed","params":{"threadId":"thread-one","status":{"type":"idle"}}}`,
	})
	appTestFeed(t, s, r[6:])
	appTestFeed(t, s, []string{
		appTestRemote, appTestRateLimits,
		`{"method":"thread/name/updated","params":{"threadId":"thread-one","threadName":"synthetic"},"emittedAtMs":9007199254740992}`,
		`{"method":"thread/closed","params":{"threadId":"thread-one"}}`,
	})
	facts, err := s.finish()
	if err != nil || facts.Answer != "Final" || !facts.UsagePresent || facts.Usage != (usage{Input: 20, Cached: 5, CacheWrite: 2, Output: 8, Reasoning: 3}) {
		t.Fatalf("facts = %+v, %v", facts, err)
	}
}

func appTestReject(t *testing.T, records []string) {
	t.Helper()
	s := appTestStart(t)
	rejected := false
	for _, record := range records {
		if _, err := s.receive([]byte(record)); err != nil {
			rejected = true
		}
	}
	facts, err := s.finish()
	if err == nil || facts != (appServerResult{}) {
		t.Fatalf("invalid stream admitted: %+v, %v", facts, err)
	}
	if !rejected && len(records) >= 9 {
		t.Fatalf("full stream reached EOF without rejecting: %v", err)
	}
	if strings.Contains(err.Error(), "VENDOR-CANARY") {
		t.Fatal("vendor data escaped fixed error")
	}
}

func TestAppServerPagination(t *testing.T) {
	s := appTestStart(t)
	appTestFeed(t, s, appTestRecords()[:2])
	other := strings.Replace(appTestRichModel, `"model":"gpt-6-astra"`, `"model":"other"`, 1)
	a, err := s.receive([]byte(`{"id":2,"result":{"data":[` + other + `],"nextCursor":"next-page"}}`))
	if err != nil {
		t.Fatal(err)
	}
	appTestSameJSON(t, a.write, `{"id":3,"method":"model/list","params":{"includeHidden":true,"limit":100,"cursor":"next-page"}}`)
	a, err = s.receive([]byte(`{"id":3,"result":{"data":[` + appTestModel + `],"nextCursor":null}}`))
	if err != nil {
		t.Fatal(err)
	}
	appTestSameJSON(t, a.write, `{"id":4,"method":"thread/start","params":{"model":"gpt-6-astra","cwd":"/synthetic/consult","approvalPolicy":"never","approvalsReviewer":"user","sandbox":"read-only","ephemeral":true}}`)
	r := appTestRecords()[3:]
	r[0] = strings.Replace(r[0], `"id":3`, `"id":4`, 1)
	r[len(r)-1] = strings.Replace(r[len(r)-1], `"id":4`, `"id":5`, 1)
	appTestFeed(t, s, r)
	if _, err := s.finish(); err != nil {
		t.Fatal(err)
	}
}

func TestAppServerPaginationBounds(t *testing.T) {
	other := strings.Replace(appTestModel, `"model":"gpt-6-astra"`, `"model":"other"`, 1)
	for _, tc := range []struct{ name, data, cursor string }{
		{"selector is not picker id", strings.Replace(other, `"id":"picker-id"`, `"id":"gpt-6-astra"`, 1), "null"},
		{"missing model", other, "null"},
		{"empty continuation", "", `"next"`},
		{"empty cursor", other, `""`},
		{"long cursor", other, `"` + strings.Repeat("c", 129) + `"`},
		{"page size", strings.TrimSuffix(strings.Repeat(appTestModel+",", 101), ","), "null"},
		{"validate after matching selector", appTestModel + `,{"id":"invalid"}`, "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := appTestStart(t)
			appTestFeed(t, s, appTestRecords()[:2])
			a, err := s.receive([]byte(`{"id":2,"result":{"data":[` + tc.data + `],"nextCursor":` + tc.cursor + `}}`))
			if err == nil || len(a.write) != 0 {
				t.Fatalf("invalid page progressed: %+v, %v", a, err)
			}
		})
	}
	t.Run("repeated cursor", func(t *testing.T) {
		s := appTestStart(t)
		appTestFeed(t, s, appTestRecords()[:2])
		appTestFeed(t, s, []string{`{"id":2,"result":{"data":[` + other + `],"nextCursor":"cycle"}}`})
		if _, err := s.receive([]byte(`{"id":3,"result":{"data":[` + other + `],"nextCursor":"cycle"}}`)); err == nil {
			t.Fatal("cursor cycle accepted")
		}
	})
	for _, found := range []bool{false, true} {
		t.Run(fmt.Sprintf("32 pages found=%v", found), func(t *testing.T) {
			s := appTestStart(t)
			appTestFeed(t, s, appTestRecords()[:2])
			for page := 1; page <= 32; page++ {
				model := other
				if page == 32 && found {
					model = appTestModel
				}
				data := strings.TrimSuffix(strings.Repeat(model+",", 100), ",")
				a, err := s.receive([]byte(fmt.Sprintf(`{"id":%d,"result":{"data":[%s],"nextCursor":"page-%d"}}`, page+1, data, page)))
				if page == 32 && !found {
					if err == nil || len(a.write) != 0 {
						t.Fatal("pagination exhausted bound without rejection")
					}
					continue
				}
				if err != nil {
					t.Fatalf("page %d: %v", page, err)
				}
				if page < 32 {
					appTestSameJSON(t, a.write, fmt.Sprintf(`{"id":%d,"method":"model/list","params":{"includeHidden":true,"limit":100,"cursor":"page-%d"}}`, page+2, page))
				}
			}
		})
	}
}

func TestAppServerEnvelopeAndCorrelation(t *testing.T) {
	for _, raw := range []string{
		strings.Replace(appTestInitReply, `"id":1`, `"id":1,"id":1`, 1),
		strings.Replace(appTestInitReply, `"userAgent":"synthetic"`, `"userAgent":"synthetic","userAgent":"synthetic"`, 1),
		strings.Replace(appTestInitReply, "synthetic", "\xff", 1),
		"", " ", "{}", appTestInitReply + "\n", appTestInitReply + "{}", "\xff",
		`{"id":1,"id":1,"result":{}}`, `{"id":1,"result":{"x":{"a":1,"a":2}}}`,
		`{"jsonrpc":"2.0","id":1,"result":{}}`, `{"id":1,"result":{},"error":{}}`,
		`{"id":1,"method":"skills/changed","params":{},"result":{}}`,
		`{"id":1,"method":"skills/changed","params":{},"error":{}}`,
		`{"method":"skills/changed","params":{},"trace":null}`, `{"method":"skills/changed","params":{},"emittedAtMs":-1}`,
		`{"method":"skills/changed","params":{},"emittedAtMs":1e0}`, `{"method":"skills/changed","params":{},"emittedAtMs":9007199254740993}`,
		`{"id":1.0,"result":{}}`, `{"id":1e0,"result":{}}`, `{"id":true,"result":{}}`, `{"id":null,"result":{}}`,
		`{"id":9223372036854775808,"method":"unknown"}`, `{"id":"` + strings.Repeat("i", 257) + `","method":"unknown"}`,
		`{"id":9,"method":"unknown","trace":{"traceparent":false}}`, `{"id":9,"method":"unknown","trace":{"extra":null}}`,
		`{"id":1,"error":{"code":1.1,"message":"VENDOR-CANARY"}}`, `{"id":1,"error":{"code":1,"message":{}}}`,
		`{"id":1,"error":{"code":1,"message":"VENDOR-CANARY","other":null}}`,
		`{"id":1,"error":{"code":-9223372036854775809,"message":"VENDOR-CANARY"}}`,
		`{"id":1,"result":{},"":null}`, `{"id":1,"result":{},"id result":null}`,
	} {
		t.Run(raw, func(t *testing.T) {
			s := appTestStart(t)
			a, err := s.receive([]byte(raw))
			if err == nil || len(a.write) != 0 {
				t.Fatalf("malformed envelope produced output: %+v,%v", a, err)
			}
		})
	}
	for _, records := range [][]string{
		{appTestRemote, appTestInitReply},
		{appTestInitReply, appTestInitReply},
		{strings.Replace(appTestInitReply, `"id":1`, `"id":2`, 1)},
		{`{"method":"skills/changed","params":{}}`},
	} {
		appTestReject(t, records)
	}
	s := appTestStart(t)
	_, err := s.receive([]byte(`{"id":1,"error":{"code":-9223372036854775808,"message":"VENDOR-CANARY","data":{"any":[null,1,"discard"]}}}`))
	if !errors.Is(err, errAppServerVendor) {
		t.Fatalf("valid error = %v", err)
	}
}

func TestAppServerClosedProfile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		index    int
		old, new string
	}{
		{"initialize platform", 0, `"macos"`, `"windows"`},
		{"initialize home", 0, `"/synthetic/native"`, `"relative"`},
		{"initialize identity", 0, `"userAgent":"synthetic"`, `"userAgent":""`},
		{"remote connected", 1, `"disabled"`, `"connected"`},
		{"remote environment", 1, `"environmentId":null`, `"environmentId":"VENDOR-CANARY"`},
		{"thread model", 3, `"model":"gpt-6-astra"`, `"model":"other"`},
		{"thread provider", 3, `"modelProvider":"openai"`, `"modelProvider":"other"`},
		{"thread cwd", 3, `"cwd":"/synthetic/consult"`, `"cwd":"/other"`},
		{"thread source", 3, `"source":"vscode"`, `"source":"exec"`},
		{"thread ephemeral", 3, `"ephemeral":true`, `"ephemeral":false`},
		{"thread root", 3, `"preview":""`, `"preview":"","parentThreadId":"parent"`},
		{"thread history", 3, `"preview":""`, `"preview":"","historyMode":"paginated"`},
		{"thread turns", 3, `"turns":[]`, `"turns":[{}]`},
		{"thread direct input absent", 3, `,"canAcceptDirectInput":true`, ``},
		{"thread direct input false", 3, `"canAcceptDirectInput":true`, `"canAcceptDirectInput":false`},
		{"thread extra absent", 3, `,"extra":null`, ``},
		{"thread extra open", 3, `"extra":null`, `"extra":{"unknown":1}`},
		{"runtime roots absent", 3, `,"runtimeWorkspaceRoots":[]`, ``},
		{"runtime roots relative", 3, `"runtimeWorkspaceRoots":[]`, `"runtimeWorkspaceRoots":["relative"]`},
		{"permission profile absent", 3, `,"activePermissionProfile":null`, ``},
		{"permission profile open", 3, `"activePermissionProfile":null`, `"activePermissionProfile":{"id":"p","grant":true}`},
		{"multiagent absent", 3, `,"multiAgentMode":"explicitRequestOnly"`, ``},
		{"multiagent incompatible", 3, `"multiAgentMode":"explicitRequestOnly"`, `"multiAgentMode":"none"`},
		{"reviewer", 3, `"approvalsReviewer":"user"`, `"approvalsReviewer":"auto_review"`},
		{"approval", 3, `"approvalPolicy":"never"`, `"approvalPolicy":"on-request"`},
		{"sandbox", 3, `"type":"readOnly"`, `"type":"workspaceWrite"`},
		{"network", 3, `"networkAccess":false`, `"networkAccess":true`},
		{"timestamp exponent", 3, `"createdAt":1`, `"createdAt":1e0`},
		{"thread started session", 4, `"sessionId":"session-one"`, `"sessionId":"other"`},
		{"thread started direct input", 4, `"canAcceptDirectInput":true`, `"canAcceptDirectInput":null`},
		{"turn started wrong thread", 5, `"threadId":"thread-one"`, `"threadId":"other"`},
		{"turn reply drift", 8, `"id":"turn-one"`, `"id":"other"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := appTestRecords()
			if !strings.Contains(r[tc.index], tc.old) {
				t.Fatal("fixture edit missing")
			}
			r[tc.index] = strings.Replace(r[tc.index], tc.old, tc.new, 1)
			appTestReject(t, r)
		})
	}
	for _, value := range []string{`""`, `"` + strings.Repeat("i", 257) + `"`, `"bad\u0000id"`, `"bad\u007fid"`} {
		r := appTestRecords()
		r[3] = strings.Replace(r[3], `"thread-one"`, value, 1)
		appTestReject(t, r)
	}
}

func TestAppServerLifecycleOrdering(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before int
		raw    string
	}{
		{"thread started before reply", 3, appTestThreadStarted},
		{"duplicate thread started", 5, appTestThreadStarted},
		{"duplicate turn started", 6, appTestTurnStarted},
		{"item before turn started", 5, appTestCompleted},
		{"usage before turn started", 5, appTestUsage},
		{"status before thread requested", 2, `{"method":"thread/status/changed","params":{"threadId":"thread-one","status":{"type":"idle"}}}`},
		{"two tentative threads", 3, `{"method":"thread/status/changed","params":{"threadId":"other","status":{"type":"idle"}}}`},
		{"status system error", 6, `{"method":"thread/status/changed","params":{"threadId":"thread-one","status":{"type":"systemError"}}}`},
		{"status waiting approval", 6, `{"method":"thread/status/changed","params":{"threadId":"thread-one","status":{"type":"active","activeFlags":["waitingOnApproval"]}}}`},
		{"status waiting input", 6, `{"method":"thread/status/changed","params":{"threadId":"thread-one","status":{"type":"active","activeFlags":["waitingOnUserInput"]}}}`},
		{"status not loaded active", 6, `{"method":"thread/status/changed","params":{"threadId":"thread-one","status":{"type":"notLoaded"}}}`},
		{"thread closed early", 7, `{"method":"thread/closed","params":{"threadId":"thread-one"}}`},
		{"thread closed provisional", 8, `{"method":"thread/closed","params":{"threadId":"thread-one"}}`},
		{"mcp failure", 6, `{"method":"mcpServer/startupStatus/updated","params":{"name":"synthetic","status":"failed"}}`},
		{"mcp error", 6, `{"method":"mcpServer/startupStatus/updated","params":{"name":"synthetic","status":"ready","error":"VENDOR-CANARY"}}`},
		{"mcp failure reason", 6, `{"method":"mcpServer/startupStatus/updated","params":{"name":"synthetic","status":"ready","failureReason":"reauthenticationRequired"}}`},
		{"late usage", 9, appTestUsage},
		{"late item", 9, appTestCompleted},
		{"duplicate reply", 9, appTestTurnReply},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := appTestRecords()
			r = append(append(append([]string{}, r[:tc.before]...), tc.raw), r[tc.before:]...)
			appTestReject(t, r)
		})
	}
	s := appTestStart(t)
	appTestFeed(t, s, appTestRecords()[:3])
	appTestFeed(t, s, []string{`{"method":"thread/status/changed","params":{"threadId":"thread-one","status":{"type":"notLoaded"}}}`})
	appTestFeed(t, s, appTestRecords()[3:5])
	appTestFeed(t, s, []string{`{"method":"thread/status/changed","params":{"threadId":"thread-one","status":{"type":"active","activeFlags":[]}}}`})
	appTestFeed(t, s, appTestRecords()[5:])
	if _, err := s.finish(); err != nil {
		t.Fatal(err)
	}
}

func TestAppServerAnswerSelectionAndSummary(t *testing.T) {
	for _, phase := range []string{``, `,"phase":null`, `,"phase":"final_answer"`} {
		s := appTestStart(t)
		r := appTestRecords()
		answer := `{"type":"agentMessage","id":"answer-one","text":"OK"` + phase + `}`
		r[6] = strings.Replace(r[6], appTestAnswer, answer, 1)
		r[7] = strings.Replace(r[7], appTestAnswer, answer, 1)
		appTestFeed(t, s, r[:7])
		appTestFeed(t, s, []string{
			`{"method":"item/completed","params":{"threadId":"thread-one","turnId":"turn-one","completedAtMs":4,"item":{"type":"agentMessage","id":"comment","text":"ignore","phase":"commentary"}}}`,
			`{"method":"item/completed","params":{"threadId":"thread-one","turnId":"turn-one","completedAtMs":4,"item":{"type":"agentMessage","id":"blank","text":" \t\n","phase":"final_answer"}}}`,
		})
		appTestFeed(t, s, r[7:])
		if facts, err := s.finish(); err != nil || facts.Answer != "OK" {
			t.Fatalf("selection=%+v,%v", facts, err)
		}
	}
	t.Run("last eligible wins", func(t *testing.T) {
		s := appTestStart(t)
		appTestFeed(t, s, appTestRecords()[:7])
		appTestFeed(t, s, []string{strings.ReplaceAll(strings.Replace(appTestCompleted, appTestAnswer, `{"type":"agentMessage","id":"answer-two","text":"Latest"}`, 1), `"completedAtMs":3`, `"completedAtMs":0`), strings.Replace(appTestTerminal, appTestAnswer, `{"type":"agentMessage","id":"answer-two","text":"Latest","phase":null,"memoryCitation":null,"delivery":null,"questions":null}`, 1), appTestTurnReply})
		if facts, err := s.finish(); err != nil || facts.Answer != "Latest" {
			t.Fatalf("latest=%+v,%v", facts, err)
		}
	})
	t.Run("notLoaded still needs completion", func(t *testing.T) {
		s := appTestStart(t)
		r := appTestRecords()
		r[7] = strings.Replace(strings.Replace(r[7], `"items":[`+appTestAnswer+`]`, `"items":[]`, 1), `"summary"`, `"notLoaded"`, 1)
		appTestFeed(t, s, r)
		if _, err := s.finish(); err != nil {
			t.Fatal(err)
		}
	})
	for _, tc := range []struct{ name, old, new string }{
		{"text mismatch", `"OK.\r\nReady\u001b"`, `"different"`},
		{"id mismatch", `"answer-one"`, `"different"`},
		{"phase mismatch", `"final_answer"`, `null`},
		{"metadata mismatch", `"phase":"final_answer"`, `"phase":"final_answer","delivery":"async"`},
		{"missing view", `,"itemsView":"summary"`, ``},
		{"full view", `"summary"`, `"full"`},
		{"unknown view", `"summary"`, `"future"`},
		{"two summaries", `"items":[` + appTestAnswer + `]`, `"items":[` + appTestAnswer + `,` + appTestAnswer + `]`},
		{"error", `"status":"completed"`, `"status":"completed","error":{"message":"VENDOR-CANARY"}`},
		{"failed", `"status":"completed"`, `"status":"failed"`},
		{"interrupted", `"status":"completed"`, `"status":"interrupted"`},
		{"in progress", `"status":"completed"`, `"status":"inProgress"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := appTestRecords()
			r[7] = strings.Replace(r[7], tc.old, tc.new, 1)
			appTestReject(t, r)
		})
	}
	for _, phase := range []string{`"commentary"`, `"unknown"`} {
		r := appTestRecords()
		r[6] = strings.Replace(r[6], `"final_answer"`, phase, 1)
		r[7] = strings.Replace(r[7], `"final_answer"`, phase, 1)
		appTestReject(t, r)
	}
	for _, item := range []string{`{"type":"reasoning","id":"answer-one"}`, `{"type":"agentMessage","id":"answer-one","text":" \t\n"}`} {
		r := appTestRecords()
		r[6] = strings.Replace(r[6], appTestAnswer, item, 1)
		r[7] = strings.Replace(r[7], appTestAnswer, item, 1)
		appTestReject(t, r)
	}
}

func TestAppServerAnswerBoundBeforeClose(t *testing.T) {
	for _, n := range []int{65536, 65537} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			s := appTestStart(t)
			r := appTestRecords()
			answer := `{"type":"agentMessage","id":"answer-one","text":"` + strings.Repeat("a", n) + `"}`
			r[6] = strings.Replace(r[6], appTestAnswer, answer, 1)
			r[7] = strings.Replace(r[7], appTestAnswer, answer, 1)
			appTestFeed(t, s, append(append([]string{}, r[:7]...), appTestTurnReply))
			_, err := s.receive([]byte(r[7]))
			if n == 65537 && !errors.Is(err, errAppServerAnswer) {
				t.Fatalf("oversized sanitized answer reached success close: %v", err)
			}
			if n == 65536 {
				facts, err := s.finish()
				if err != nil || len(facts.Answer) != 65536 {
					t.Fatalf("answer bound = %d,%v", len(facts.Answer), err)
				}
			}
		})
	}
	s := appTestStart(t)
	r := appTestRecords()
	answer := `{"type":"agentMessage","id":"answer-one","text":"` + strings.Repeat(`\u0001`, 21846) + `"}`
	r[6] = strings.Replace(r[6], appTestAnswer, answer, 1)
	r[7] = strings.Replace(r[7], appTestAnswer, answer, 1)
	appTestFeed(t, s, r[:7])
	if _, err := s.receive([]byte(r[7])); !errors.Is(err, errAppServerAnswer) {
		t.Fatalf("sanitizer expansion not bounded: %v", err)
	}
}

func TestAppServerServerRequests(t *testing.T) {
	const command = `{"threadId":"thread-one","turnId":"turn-one","itemId":"tool","startedAtMs":1,"approvalId":"synthetic","command":"never run","cwd":"/synthetic","environmentId":"synthetic","reason":"synthetic","kind":"writeStdin","commandActions":[{"type":"read","command":"synthetic","name":"synthetic","path":"synthetic"},{"type":"listFiles","command":"synthetic","path":null},{"type":"search","command":"synthetic","path":"synthetic","query":"synthetic"},{"type":"unknown","command":"synthetic"}],"networkApprovalContext":{"host":"synthetic","protocol":"socks5Tcp"},"proposedExecpolicyAmendment":["synthetic"],"proposedNetworkPolicyAmendments":[{"host":"synthetic","action":"deny"}]}`
	const file = `{"threadId":"thread-one","turnId":"turn-one","itemId":"tool","startedAtMs":1,"reason":"synthetic","grantRoot":"synthetic"}`
	for _, tc := range []struct{ name, method, id, params, reply string }{
		{"command pending ID collision", "item/commandExecution/requestApproval", `4`, command, `{"id":4,"result":{"decision":"cancel"}}`},
		{"file string ID", "item/fileChange/requestApproval", `"4"`, file, `{"id":"4","result":{"decision":"cancel"}}`},
		{"command negative ID", "item/commandExecution/requestApproval", `-9223372036854775808`, command, `{"id":-9223372036854775808,"result":{"decision":"cancel"}}`},
		{"malformed command", "item/commandExecution/requestApproval", `"bad"`, `{"threadId":"other"}`, `{"id":"bad","error":{"code":-32000,"message":"unsupported server request"}}`},
		{"malformed file", "item/fileChange/requestApproval", `9`, `{"threadId":"other"}`, `{"id":9,"error":{"code":-32000,"message":"unsupported server request"}}`},
		{"wrong thread command", "item/commandExecution/requestApproval", `9`, strings.Replace(command, `"thread-one"`, `"other"`, 1), `{"id":9,"error":{"code":-32000,"message":"unsupported server request"}}`},
		{"experimental command fields stripped", "item/commandExecution/requestApproval", `9`, strings.Replace(command, `"kind":"writeStdin"`, `"kind":"writeStdin","availableDecisions":[]`, 1), `{"id":9,"error":{"code":-32000,"message":"unsupported server request"}}`},
		{"permissions different schema", "item/permissions/requestApproval", `"permissions"`, `{"permissions":{"network":true}}`, `{"id":"permissions","error":{"code":-32000,"message":"unsupported server request"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := appTestStart(t)
			appTestFeed(t, s, appTestRecords()[:6])
			a, err := s.receive([]byte(`{"id":` + tc.id + `,"method":"` + tc.method + `","params":` + tc.params + `,"trace":{"traceparent":null,"tracestate":"discard"}}`))
			if !errors.Is(err, errAppServerRequest) || !a.closeStdin {
				t.Fatalf("request did not abort: %+v, %v", a, err)
			}
			appTestSameJSON(t, a.write, tc.reply)
			if s.pendingID != 4 {
				t.Fatal("server request consumed host reply correlation")
			}
			for _, r := range appTestRecords()[6:] {
				_, _ = s.receive([]byte(r))
			}
			if facts, err := s.finish(); err == nil || facts != (appServerResult{}) {
				t.Fatalf("negative reply admitted: %+v,%v", facts, err)
			}
		})
	}
	for _, method := range []string{"item/tool/requestUserInput", "mcpServer/elicitation/request", "item/permissions/requestApproval", "item/tool/call", "account/chatgptAuthTokens/refresh", "attestation/generate", "currentTime/read", "applyPatchApproval", "execCommandApproval", "unknown/future"} {
		for _, suffix := range []string{``, `,"params":null`, `,"params":[true,{"opaque":"discard"}],"trace":null`} {
			s := appTestStart(t)
			a, err := s.receive([]byte(`{"id":1,"method":"` + method + `"` + suffix + `}`))
			if !errors.Is(err, errAppServerRequest) {
				t.Fatalf("unsupported request %s = %v", method, err)
			}
			appTestSameJSON(t, a.write, `{"id":1,"error":{"code":-32000,"message":"unsupported server request"}}`)
		}
	}
}

func TestAppServerInterruptIDs(t *testing.T) {
	for _, stage := range []int{0, 2, 5, 6, 8, 9} {
		t.Run(fmt.Sprint(stage), func(t *testing.T) {
			s := appTestStart(t)
			appTestFeed(t, s, appTestRecords()[:stage])
			got := s.interrupt()
			if stage == 6 {
				appTestSameJSON(t, got, `{"id":5,"method":"turn/interrupt","params":{"threadId":"thread-one","turnId":"turn-one"}}`)
			} else if len(got) != 0 {
				t.Fatalf("interrupt without active ID: %s", got)
			}
			if len(s.interrupt()) != 0 {
				t.Fatal("duplicate interrupt")
			}
			if facts, err := s.finish(); err == nil || facts != (appServerResult{}) {
				t.Fatalf("cancellation admitted: %+v,%v", facts, err)
			}
		})
	}
	s := appTestStart(t)
	appTestFeed(t, s, append(appTestRecords()[:5], appTestTurnReply))
	appTestSameJSON(t, s.interrupt(), `{"id":5,"method":"turn/interrupt","params":{"threadId":"thread-one","turnId":"turn-one"}}`)
}

func TestAppServerUsageValidation(t *testing.T) {
	for _, tc := range []struct{ name, old, new string }{
		{"fraction", `"totalTokens":90`, `"totalTokens":90.0`},
		{"exponent", `"totalTokens":90`, `"totalTokens":9e1`},
		{"negative", `"inputTokens":20`, `"inputTokens":-1`},
		{"overflow", `"totalTokens":90`, `"totalTokens":9007199254740993`},
		{"cached exceeds input", `"cachedInputTokens":5`, `"cachedInputTokens":21`},
		{"reasoning exceeds output", `"reasoningOutputTokens":3`, `"reasoningOutputTokens":9`},
		{"last exceeds total", `"totalTokens":7`, `"totalTokens":91`},
		{"null cache-write", `"cacheWriteInputTokens":2`, `"cacheWriteInputTokens":null`},
		{"missing input", `"inputTokens":20,`, ``},
		{"zero context window", `"modelContextWindow":100`, `"modelContextWindow":0`},
		{"wrong turn", `"turnId":"turn-one"`, `"turnId":"other"`},
		{"unknown usage field", `"modelContextWindow":100`, `"modelContextWindow":100,"unseen":1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := appTestStart(t)
			appTestFeed(t, s, appTestRecords()[:6])
			if _, err := s.receive([]byte(strings.Replace(appTestUsage, tc.old, tc.new, 1))); err == nil {
				t.Fatal("invalid usage accepted")
			}
		})
	}
	for _, next := range []string{
		strings.Replace(appTestUsage, `"inputTokens":20`, `"inputTokens":19`, 1),
		strings.Replace(appTestUsage, `"totalTokens":7`, `"totalTokens":6`, 1),
		strings.Replace(appTestUsage, `"modelContextWindow":100`, `"modelContextWindow":101`, 1),
	} {
		s := appTestStart(t)
		appTestFeed(t, s, append(appTestRecords()[:6], appTestUsage))
		if _, err := s.receive([]byte(next)); err == nil {
			t.Fatal("conflicting cumulative usage accepted")
		}
	}
	s := appTestStart(t)
	appTestFeed(t, s, append(appTestRecords()[:6], appTestUsage))
	later := strings.Replace(appTestUsage, `"inputTokens":20`, `"inputTokens":21`, 1)
	later = strings.Replace(later, `"outputTokens":8`, `"outputTokens":10`, 1)
	appTestFeed(t, s, append([]string{later, later}, appTestRecords()[6:]...))
	facts, err := s.finish()
	if err != nil || facts.Usage != (usage{Input: 21, Cached: 5, CacheWrite: 2, Output: 10, Reasoning: 3}) {
		t.Fatalf("cumulative replacement=%+v,%v", facts, err)
	}
}

func TestAppServerItemLifecycles(t *testing.T) {
	const start = `{"method":"item/started","params":{"threadId":"thread-one","turnId":"turn-one","startedAtMs":1,"item":{"type":"agentMessage","id":"answer-one","text":""}}}`
	for _, tc := range []struct {
		name   string
		insert []string
	}{
		{"duplicate start", []string{start, start}},
		{"duplicate completed", []string{appTestCompleted, appTestCompleted}},
		{"start after completion", []string{appTestCompleted, start}},
		{"mismatched type", []string{strings.Replace(start, `"type":"agentMessage","id":"answer-one","text":""`, `"type":"reasoning","id":"answer-one"`, 1), appTestCompleted}},
		{"unfinished item", []string{strings.Replace(start, `"answer-one"`, `"never-completed"`, 1)}},
		{"user content", []string{strings.Replace(appTestCompleted, appTestAnswer, `{"type":"userMessage","id":"user","content":[{"type":"text","text":"wrong"}]}`, 1)}},
		{"reasoning shape", []string{strings.Replace(appTestCompleted, appTestAnswer, `{"type":"reasoning","id":"r","summary":[1]}`, 1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := append(append(append([]string{}, appTestRecords()[:6]...), tc.insert...), appTestRecords()[6:]...)
			appTestReject(t, r)
		})
	}
}

func TestAppServerAllRejectedMethodsAndItems(t *testing.T) {
	for _, tc := range []struct{ method, point string }{
		{"error", "error-notification-rejected"},
		{"thread/archived", "thread-archived-rejected"},
		{"thread/deleted", "thread-deleted-rejected"},
		{"thread/unarchived", "thread-unarchived-rejected"},
		{"thread/reverted", "thread-reverted-rejected"},
		{"thread/goal/updated", "thread-goal-updated-rejected"},
		{"thread/goal/cleared", "thread-goal-cleared-rejected"},
		{"thread/queue/changed", "thread-queue-changed-rejected"},
		{"project/changed", "project-changed-rejected"},
		{"thread/project/updated", "thread-project-updated-rejected"},
		{"thread/environment/connected", "thread-environment-connected-rejected"},
		{"thread/environment/disconnected", "thread-environment-disconnected-rejected"},
		{"thread/settings/updated", "thread-settings-updated-rejected"},
		{"hook/started", "hook-started-rejected"},
		{"hook/completed", "hook-completed-rejected"},
		{"turn/diff/updated", "turn-diff-updated-rejected"},
		{"turn/plan/updated", "turn-plan-updated-rejected"},
		{"item/autoApprovalReview/started", "auto-approval-review-started-rejected"},
		{"item/autoApprovalReview/completed", "auto-approval-review-completed-rejected"},
		{"autoApprovalReview/strictReviewRequired", "auto-approval-review-strict-review-required-rejected"},
		{"rawResponseItem/completed", "raw-response-item-completed-rejected"},
		{"rawResponse/completed", "raw-response-completed-rejected"},
		{"item/agentMessage/delta", "agent-message-delta-rejected"},
		{"item/plan/delta", "plan-delta-rejected"},
		{"command/exec/outputDelta", "command-exec-output-delta-rejected"},
		{"process/outputDelta", "process-output-delta-rejected"},
		{"process/exited", "process-exited-rejected"},
		{"item/commandExecution/outputDelta", "command-execution-output-delta-rejected"},
		{"item/commandExecution/terminalInteraction", "command-execution-terminal-interaction-rejected"},
		{"item/fileChange/outputDelta", "file-change-output-delta-rejected"},
		{"item/fileChange/patchUpdated", "file-change-patch-updated-rejected"},
		{"serverRequest/resolved", "server-request-resolved-rejected"},
		{"item/mcpToolCall/progress", "mcp-tool-call-progress-rejected"},
		{"mcpServer/oauthLogin/completed", "mcp-server-oauth-login-completed-rejected"},
		{"mcpServer/event/stream/notification", "mcp-server-event-stream-notification-rejected"},
		{"app/list/updated", "app-list-updated-rejected"},
		{"externalAgentConfig/import/progress", "external-agent-config-import-progress-rejected"},
		{"externalAgentConfig/import/completed", "external-agent-config-import-completed-rejected"},
		{"fs/changed", "fs-changed-rejected"},
		{"item/reasoning/summaryTextDelta", "reasoning-summary-text-delta-rejected"},
		{"item/reasoning/summaryPartAdded", "reasoning-summary-part-added-rejected"},
		{"item/reasoning/textDelta", "reasoning-text-delta-rejected"},
		{"thread/compacted", "thread-compacted-rejected"},
		{"model/rerouted", "model-rerouted-rejected"},
		{"modelProvider/authRecoveryStarted", "model-provider-auth-recovery-started-rejected"},
		{"modelProvider/authRecoveryCompleted", "model-provider-auth-recovery-completed-rejected"},
		{"turn/moderationMetadata", "turn-moderation-metadata-rejected"},
		{"guardianWarning", "guardian-warning-rejected"},
		{"fuzzyFileSearch/sessionUpdated", "fuzzy-file-search-session-updated-rejected"},
		{"fuzzyFileSearch/sessionCompleted", "fuzzy-file-search-session-completed-rejected"},
		{"thread/realtime/started", "thread-realtime-started-rejected"},
		{"thread/realtime/itemAdded", "thread-realtime-item-added-rejected"},
		{"thread/realtime/item/started", "thread-realtime-item-started-rejected"},
		{"thread/realtime/item/transcript/delta", "thread-realtime-item-transcript-delta-rejected"},
		{"thread/realtime/item/completed", "thread-realtime-item-completed-rejected"},
		{"thread/realtime/transcript/delta", "thread-realtime-transcript-delta-rejected"},
		{"thread/realtime/transcript/done", "thread-realtime-transcript-done-rejected"},
		{"thread/realtime/outputAudio/delta", "thread-realtime-output-audio-delta-rejected"},
		{"thread/realtime/sdp", "thread-realtime-sdp-rejected"},
		{"thread/realtime/error", "thread-realtime-error-rejected"},
		{"thread/realtime/closed", "thread-realtime-closed-rejected"},
		{"windows/worldWritableWarning", "windows-world-writable-warning-rejected"},
		{"windowsSandbox/setupCompleted", "windows-sandbox-setup-completed-rejected"},
		{"account/login/completed", "account-login-completed-rejected"},
		{"future/method", "notification-unknown"},
		{"skills/changed/future", "notification-unknown"},
	} {
		method := tc.method
		t.Run(method, func(t *testing.T) {
			s := appTestStart(t)
			appTestFeed(t, s, appTestRecords()[:6])
			action, err := s.receive([]byte(`{"method":"` + method + `","params":{}}`))
			want := "codex-app-server-invalid-protocol-turn-" + tc.point
			if !action.closeStdin || !errors.Is(err, errAppServerProtocolTurn) || classifyAppServerError(err).Reason != want {
				t.Fatalf("receive rejected method = %s, want %s", classifyAppServerError(err).Reason, want)
			}
		})
	}
	for _, tc := range []struct{ kind, point string }{
		{"hookPrompt", "item-hook-prompt-rejected"},
		{"functionCallOutput", "item-function-call-output-rejected"},
		{"plan", "item-plan-rejected"},
		{"commandExecution", "item-command-execution-rejected"},
		{"fileChange", "item-file-change-rejected"},
		{"mcpToolCall", "item-mcp-tool-call-rejected"},
		{"dynamicToolCall", "item-dynamic-tool-call-rejected"},
		{"collabAgentToolCall", "item-collab-agent-tool-call-rejected"},
		{"subAgentActivity", "item-sub-agent-activity-rejected"},
		{"webSearch", "item-web-search-rejected"},
		{"imageView", "item-image-view-rejected"},
		{"sleep", "item-sleep-rejected"},
		{"imageGeneration", "item-image-generation-rejected"},
		{"enteredReviewMode", "item-entered-review-mode-rejected"},
		{"exitedReviewMode", "item-exited-review-mode-rejected"},
		{"contextCompaction", "item-context-compaction-rejected"},
		{"unknown", "item-unknown"},
		{"agentMessageFuture", "item-unknown"},
	} {
		kind := tc.kind
		t.Run(kind, func(t *testing.T) {
			s := appTestStart(t)
			appTestFeed(t, s, appTestRecords()[:6])
			action, err := s.receive([]byte(strings.Replace(appTestCompleted, appTestAnswer, `{"type":"`+kind+`","id":"action"}`, 1)))
			want := "codex-app-server-invalid-protocol-turn-" + tc.point
			if !action.closeStdin || !errors.Is(err, errAppServerProtocolTurn) || classifyAppServerError(err).Reason != want {
				t.Fatalf("receive rejected item = %s, want %s", classifyAppServerError(err).Reason, want)
			}
		})
	}
}

func TestAppServerBoundedJSON(t *testing.T) {
	for _, tc := range []struct {
		name, params string
		reply        bool
	}{
		{"depth 64", strings.Repeat("[", 63) + "0" + strings.Repeat("]", 63), true},
		{"depth 65", strings.Repeat("[", 64) + "0" + strings.Repeat("]", 64), false},
		{"array 4096", "[" + strings.TrimSuffix(strings.Repeat("0,", 4096), ",") + "]", true},
		{"array 4097", "[" + strings.TrimSuffix(strings.Repeat("0,", 4097), ",") + "]", false},
		{"oversized frame", `"` + strings.Repeat("x", 1048576) + `"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := appTestStart(t)
			a, err := s.receive([]byte(`{"id":1,"method":"unknown","params":` + tc.params + `}`))
			if err == nil || (len(a.write) != 0) != tc.reply {
				t.Fatalf("bounded JSON reply=%d err=%v", len(a.write), err)
			}
		})
	}
}

func TestAppServerEveryAcceptedObjectIsClosed(t *testing.T) {
	richThreadReply := strings.Replace(appTestThreadReply, appTestThread, appTestRichThread, 1)
	richThreadReply = strings.Replace(richThreadReply, `"activePermissionProfile":null`, `"activePermissionProfile":{"id":"synthetic","extends":null}`, 1)
	records := []struct {
		stage int
		raw   string
	}{
		{0, appTestInitReply}, {1, appTestRemote},
		{2, `{"id":2,"result":{"data":[` + appTestRichModel + `]}}`},
		{3, richThreadReply}, {4, strings.Replace(appTestThreadStarted, appTestThread, appTestRichThread, 1)},
		{5, appTestTurnStarted}, {6, strings.Replace(appTestCompleted, appTestAnswer, appTestRichAnswer, 1)},
		{6, appTestUsage}, {6, appTestRateLimits},
		{6, `{"method":"skills/changed","params":{}}`},
		{6, `{"method":"thread/name/updated","params":{"threadId":"thread-one","threadName":null}}`},
		{6, `{"method":"thread/status/changed","params":{"threadId":"thread-one","status":{"type":"active","activeFlags":[]}}}`},
		{6, `{"method":"mcpServer/startupStatus/updated","params":{"name":"synthetic","status":"starting","threadId":null}}`},
		{6, `{"method":"model/verification","params":{"threadId":"thread-one","turnId":"turn-one","verifications":[]}}`},
		{6, `{"method":"model/safetyBuffering/updated","params":{"threadId":"thread-one","turnId":"turn-one","model":"gpt-6-astra","reasons":[],"useCases":[],"showBufferingUi":false}}`},
		{6, strings.Replace(appTestCompleted, appTestAnswer, `{"type":"userMessage","id":"u","content":[{"type":"text","text":"Reply OK."}]}`, 1)},
		{6, strings.Replace(appTestCompleted, appTestAnswer, `{"type":"reasoning","id":"r","summary":[],"content":[]}`, 1)},
		{7, appTestTerminal}, {8, appTestTurnReply},
		{9, `{"method":"thread/closed","params":{"threadId":"thread-one"}}`},
	}
	for index, record := range records {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			var value any
			decoder := json.NewDecoder(strings.NewReader(record.raw))
			decoder.UseNumber()
			if err := decoder.Decode(&value); err != nil {
				t.Fatal(err)
			}
			var visit func(any, string)
			visit = func(node any, path string) {
				switch node := node.(type) {
				case map[string]any:
					for _, unknown := range []string{"_unexpected", "", "id method"} {
						node[unknown] = nil
						mutated, err := json.Marshal(value)
						if err != nil {
							t.Fatal(err)
						}
						delete(node, unknown)
						s := appTestStart(t)
						appTestFeed(t, s, appTestRecords()[:record.stage])
						if _, err := s.receive(mutated); err == nil {
							t.Fatalf("object %s allowed unknown property %q", path, unknown)
						}
					}
					for k, v := range node {
						visit(v, path+"/"+k)
					}
				case []any:
					for i, v := range node {
						visit(v, fmt.Sprintf("%s/%d", path, i))
					}
				}
			}
			// Prove each independent literal is accepted before testing its mutations.
			s := appTestStart(t)
			appTestFeed(t, s, appTestRecords()[:record.stage])
			appTestFeed(t, s, []string{record.raw})
			visit(value, "")
		})
	}
}

func TestAppServerMetadataTypesAndDefaults(t *testing.T) {
	for _, tc := range []struct {
		name          string
		stage         int
		raw, old, new string
	}{
		{"model effort empty", 2, appTestRichModel, `"defaultReasoningEffort":"future-effort"`, `"defaultReasoningEffort":""`},
		{"model efforts default null", 2, appTestRichModel, `"supportedReasoningEfforts":[{"reasoningEffort":"future-effort","description":"synthetic"}]`, `"supportedReasoningEfforts":null`},
		{"model speeds null", 2, appTestRichModel, `"additionalSpeedTiers":["fast"]`, `"additionalSpeedTiers":null`},
		{"model modality unknown", 2, appTestRichModel, `"audio"`, `"future"`},
		{"model specialty type", 2, appTestRichModel, `"modelSpecialty":"synthetic"`, `"modelSpecialty":1`},
		{"model tier required", 2, appTestRichModel, `"name":"standard",`, ``},
		{"model upgrade timestamp", 2, appTestRichModel, `"retirementAt":4`, `"retirementAt":-1`},
		{"model personality type", 2, appTestRichModel, `"supportsPersonality":true`, `"supportsPersonality":null`},
		{"model multiagent unknown", 2, appTestRichModel, `"multiAgentVersion":"v2"`, `"multiAgentVersion":"future"`},
		{"thread section required", 3, appTestRichThread, `"name":"synthetic","appearance"`, `"appearance"`},
		{"thread effort empty", 3, appTestRichThread, `"reasoningEffort":"future-effort"`, `"reasoningEffort":""`},
		{"thread timestamp negative", 3, appTestRichThread, `"updatedAt":0`, `"updatedAt":-1`},
		{"answer delivery unknown", 6, appTestRichAnswer, `"delivery":"async"`, `"delivery":"future"`},
		{"citation uint overflow", 6, appTestRichAnswer, `4294967295`, `4294967296`},
		{"citation ID empty", 6, appTestRichAnswer, `"citation-thread"`, `""`},
		{"question options type", 6, appTestRichAnswer, `["one","two"]`, `[false]`},
		{"rate plan unknown", 6, appTestRateLimits, `"planType":"pro"`, `"planType":"future"`},
		{"rate type unknown", 6, appTestRateLimits, `"rateLimitReachedType":"rate_limit_reached"`, `"rateLimitReachedType":"future"`},
		{"rate used percent overflow", 6, appTestRateLimits, `"usedPercent":1`, `"usedPercent":2147483648`},
		{"rate used percent exponent", 6, appTestRateLimits, `"usedPercent":1`, `"usedPercent":1e0`},
		{"rate spend percent overflow", 6, appTestRateLimits, `"remainingPercent":100`, `"remainingPercent":-2147483649`},
		{"rate balance type", 6, appTestRateLimits, `"balance":"1"`, `"balance":1`},
		{"rate nullable bool type", 6, appTestRateLimits, `"spendControlReached":false`, `"spendControlReached":"false"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Replace(tc.raw, tc.old, tc.new, 1)
			if raw == tc.raw {
				t.Fatal("fixture edit missing")
			}
			switch tc.stage {
			case 2:
				raw = `{"id":2,"result":{"data":[` + raw + `]}}`
			case 3:
				raw = strings.Replace(appTestThreadReply, appTestThread, raw, 1)
			case 6:
				if tc.raw == appTestRichAnswer {
					raw = strings.Replace(appTestCompleted, appTestAnswer, raw, 1)
				}
			}
			s := appTestStart(t)
			appTestFeed(t, s, appTestRecords()[:tc.stage])
			if _, err := s.receive([]byte(raw)); err == nil {
				t.Fatal("invalid metadata type accepted")
			}
		})
	}
	s := appTestStart(t)
	appTestFeed(t, s, appTestRecords()[:6])
	for _, raw := range []string{
		`{"method":"account/rateLimits/updated","params":{"rateLimits":{}}}`,
		`{"method":"account/rateLimits/updated","params":{"rateLimits":{"credits":null,"individualLimit":null,"limitId":null,"limitName":null,"planType":null,"primary":null,"secondary":null,"rateLimitReachedType":null,"spendControlReached":null}}}`,
		`{"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"usedPercent":-2147483648},"secondary":{"usedPercent":2147483647,"resetsAt":null,"windowDurationMins":null}}}}`,
		`{"method":"skills/changed","params":{},"emittedAtMs":0}`,
		`{"method":"mcpServer/startupStatus/updated","params":{"name":"synthetic","status":"cancelled"}}`,
		`{"method":"thread/name/updated","params":{"threadId":"thread-one"}}`,
		`{"method":"model/verification","params":{"threadId":"thread-one","turnId":"turn-one","verifications":[]}}`,
		`{"method":"item/completed","params":{"threadId":"thread-one","turnId":"turn-one","completedAtMs":0,"item":{"type":"reasoning","id":"r"}}}`,
	} {
		appTestFeed(t, s, []string{raw})
	}
	appTestFeed(t, s, appTestRecords()[6:])
	if _, err := s.finish(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`-1`, `1.0`, `1e0`, `9007199254740993`, `"1"`} {
		s := appTestStart(t)
		appTestFeed(t, s, appTestRecords()[:6])
		if _, err := s.receive([]byte(`{"method":"skills/changed","params":{},"emittedAtMs":` + bad + `}`)); err == nil {
			t.Fatal("invalid emitted timestamp accepted")
		}
	}
}

func TestAppServerConfirmedTurnCannotBecomeNotLoaded(t *testing.T) {
	s := appTestStart(t)
	appTestFeed(t, s, append(appTestRecords()[:5], appTestTurnReply))
	if _, err := s.receive([]byte(`{"method":"thread/status/changed","params":{"threadId":"thread-one","status":{"type":"notLoaded"}}}`)); err == nil {
		t.Fatal("confirmed active turn accepted notLoaded before turn/started")
	}
}

func TestAppServerHostReplyCorrelation(t *testing.T) {
	for _, index := range []int{0, 2, 3, 8} {
		r := appTestRecords()
		r[index] = strings.Replace(r[index], `"id":`, `"id":9`, 1)
		// Restore a valid but wrong integer literal, rather than malformed JSON.
		s := appTestStart(t)
		appTestFeed(t, s, r[:index])
		if _, err := s.receive([]byte(r[index])); err == nil {
			t.Fatalf("reply %d consumed a different host ID", index)
		}
	}
	s := appTestStart(t)
	appTestFeed(t, s, appTestRecords())
	late := strings.Replace(strings.Replace(appTestCompleted, `"answer-one"`, `"late-valid"`, 1), `"final_answer"`, `"commentary"`, 1)
	if _, err := s.receive([]byte(late)); err == nil {
		t.Fatal("new valid item after terminal accepted")
	}
}

func TestAppServerPromptFraming(t *testing.T) {
	s := &appServerStream{model: "gpt-6-astra", prompt: "Quote \" slash \\ tab\t newline\n雪"}
	if _, err := s.start("/synthetic/consult"); err != nil {
		t.Fatal(err)
	}
	appTestFeed(t, s, appTestRecords()[:4])
	a, err := s.receive([]byte(appTestThreadStarted))
	if err != nil {
		t.Fatal(err)
	}
	appTestSameJSON(t, a.write, `{"id":4,"method":"turn/start","params":{"threadId":"thread-one","input":[{"type":"text","text":"Quote \" slash \\ tab\t newline\n雪","text_elements":[]}]}}`)
}

// These checks intentionally inspect only fixed diagnostics, never private frames.
func TestAppServerDiagnosticPhases(t *testing.T) {
	for _, tc := range []struct {
		name                string
		prefix              int
		frame, phase, point string
	}{
		{"initialize reply", 0, strings.Replace(appTestInitReply, "macos", "windows", 1), "initialize", "initialize-reply"},
		{"waiting for remote", 1, strings.Replace(appTestRemote, "disabled", "connected", 1), "initialize", "remote-status"},
		{"discovery reply", 2, `{"id":2,"result":{"data":"PRIVATE-DIAGNOSTIC"}}`, "discovery", "model-list-shape"},
		{"thread reply", 3, strings.ReplaceAll(appTestThreadReply, "gpt-6-astra", "PRIVATE-DIAGNOSTIC"), "thread", "thread-reply-profile"},
		{"turn before reply", 8, strings.Replace(appTestTurnReply, "turn-one", "PRIVATE-DIAGNOSTIC", 1), "turn", "turn-id"},
		{"shutdown", 9, `{"method":"PRIVATE-DIAGNOSTIC","params":{}}`, "shutdown", "notification-unknown"},
		{"malformed", 0, `{"PRIVATE-DIAGNOSTIC":`, "initialize", "record-decode"},
		{"unknown", 5, `{"method":"PRIVATE-DIAGNOSTIC","params":{"path":"PRIVATE-DIAGNOSTIC"}}`, "turn", "notification-unknown"},
		{"malformed known envelope", 1, `{"method":"configWarning","params":{},"PRIVATE-DIAGNOSTIC":true}`, "initialize", "notification-envelope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := appTestStart(t)
			appTestFeed(t, s, appTestRecords()[:tc.prefix])
			action, err := s.receive([]byte(tc.frame))
			want := "codex-app-server-invalid-protocol-" + tc.phase + "-" + tc.point
			if !action.closeStdin || !errors.Is(err, errAppServerProtocol) || classifyAppServerError(fmt.Errorf("private: %w", err)).Reason != want {
				t.Fatalf("fixed phase classification mismatch; want %s", want)
			}
			if strings.Contains(err.Error(), "PRIVATE-DIAGNOSTIC") {
				t.Fatal("private diagnostic escaped")
			}
			_, later := s.receive([]byte(`{"method":"warning","params":null}`))
			result, finished := s.finish()
			if later != err || finished != err || result != (appServerResult{}) {
				t.Fatal("first rejection or zero result lost")
			}
			if tc.name == "discovery reply" && s.pendingID != 0 {
				t.Fatal("test did not cross reply-state reset")
			}
			if tc.name == "turn before reply" && (!s.terminal || s.closed) {
				t.Fatal("provisional terminal misclassified as shutdown")
			}
		})
	}
}

func TestAppServerDiagnosticNotifications(t *testing.T) {
	for _, tc := range []struct {
		method, reason string
		prefix         int
	}{
		{"configWarning", "config-warning", 1}, {"warning", "warning", 6},
		{"deprecationNotice", "deprecation-notice", 1}, {"account/updated", "account-updated", 1},
	} {
		t.Run(tc.method, func(t *testing.T) {
			for _, payload := range []string{`{"message":"PRIVATE-DIAGNOSTIC"}`, `"PRIVATE-DIAGNOSTIC"`, `null`} {
				s := appTestStart(t)
				appTestFeed(t, s, appTestRecords()[:tc.prefix])
				action, err := s.receive([]byte(`{"method":"` + tc.method + `","params":` + payload + `}`))
				want := "codex-app-server-" + tc.reason + "-rejected"
				if !action.closeStdin || !errors.Is(err, errAppServerProtocol) || classifyAppServerError(fmt.Errorf("private: %w", err)).Reason != want {
					t.Fatalf("fixed notification classification mismatch; want %s", want)
				}
				if strings.Contains(err.Error(), "PRIVATE-DIAGNOSTIC") {
					t.Fatal("private diagnostic escaped")
				}
				_, later := s.receive([]byte(`{"method":"unknown","params":{}}`))
				result, finished := s.finish()
				if later != err || finished != err || result != (appServerResult{}) {
					t.Fatal("first rejection or zero result lost")
				}
			}
		})
	}
}

// Literal rejected records distinguish local predicates without retaining their payloads.
func TestAppServerRejectionPoints(t *testing.T) {
	for _, tc := range []struct {
		name         string
		prefix       int
		extra        []string
		frame, fault string
	}{
		{"framing", 6, nil, "{}\n", "record-framing"},
		{"method shape", 6, nil, `{"method":1,"params":{}}`, "method-shape"},
		{"request envelope", 6, nil, `{"id":true,"method":"PRIVATE-CANARY","params":{}}`, "request-envelope"},
		{"reply envelope", 6, nil, `{"id":4,"result":{},"PRIVATE-CANARY":true}`, "reply-envelope"},
		{"reply id", 6, nil, `{"id":"4","result":{}}`, "reply-id"},
		{"RPC error shape", 6, nil, `{"id":4,"error":{"code":"PRIVATE-CANARY","message":"PRIVATE-CANARY"}}`, "rpc-error-shape"},
		{"record envelope", 6, nil, `{"id":4,"result":{},"error":{}}`, "record-envelope"},
		{"skills shape", 6, nil, `{"method":"skills/changed","params":{"PRIVATE-CANARY":true}}`, "skills-shape"},
		{"rate limits shape", 6, nil, `{"method":"account/rateLimits/updated","params":{"rateLimits":{"PRIVATE-CANARY":true}}}`, "rate-limits-shape"},
		{"thread name", 6, nil, `{"method":"thread/name/updated","params":{"threadId":"thread-one","threadName":false}}`, "thread-name"},
		{"MCP startup", 6, nil, `{"method":"mcpServer/startupStatus/updated","params":{"name":"PRIVATE-CANARY","status":"failed"}}`, "mcp-startup-failed"},
		{"model verification", 6, nil, `{"method":"model/verification","params":{"threadId":"thread-one","turnId":"turn-one","verifications":["PRIVATE-CANARY"]}}`, "model-verification"},
		{"model safety", 6, nil, `{"method":"model/safetyBuffering/updated","params":{"threadId":"thread-one","turnId":"turn-one","model":"PRIVATE-CANARY","reasons":[],"useCases":[],"showBufferingUi":false}}`, "model-safety-buffering"},
		{"user shape", 6, nil, `{"method":"item/completed","params":{"threadId":"thread-one","turnId":"turn-one","completedAtMs":1,"item":{"id":"user-one","type":"userMessage","content":[],"clientId":"PRIVATE-CANARY"}}}`, "item-user-shape"},
		{"user content", 6, nil, `{"method":"item/completed","params":{"threadId":"thread-one","turnId":"turn-one","completedAtMs":1,"item":{"id":"user-one","type":"userMessage","content":[]}}}`, "item-user-content"},
		{"user text elements", 6, nil, `{"method":"item/completed","params":{"threadId":"thread-one","turnId":"turn-one","completedAtMs":1,"item":{"id":"user-one","type":"userMessage","content":[{"type":"text","text":"Reply OK.","text_elements":["PRIVATE-CANARY"]}]}}}`, "item-user-elements"},
		{"reasoning shape", 6, nil, `{"method":"item/completed","params":{"threadId":"thread-one","turnId":"turn-one","completedAtMs":1,"item":{"id":"reason-one","type":"reasoning","PRIVATE-CANARY":true}}}`, "item-reasoning-shape"},
		{"reasoning content", 6, nil, `{"method":"item/completed","params":{"threadId":"thread-one","turnId":"turn-one","completedAtMs":1,"item":{"id":"reason-one","type":"reasoning","content":false}}}`, "item-reasoning-content"},
		{"agent shape", 6, nil, `{"method":"item/completed","params":{"threadId":"thread-one","turnId":"turn-one","completedAtMs":1,"item":{"id":"answer-one","type":"agentMessage","text":false}}}`, "item-agent-shape"},
		{"agent citation", 6, nil, `{"method":"item/completed","params":{"threadId":"thread-one","turnId":"turn-one","completedAtMs":1,"item":{"id":"answer-one","type":"agentMessage","text":"PRIVATE-CANARY","memoryCitation":{"path":"PRIVATE-CANARY"}}}}`, "item-agent-citation"},
		{"agent questions", 6, nil, `{"method":"item/completed","params":{"threadId":"thread-one","turnId":"turn-one","completedAtMs":1,"item":{"id":"answer-one","type":"agentMessage","text":"PRIVATE-CANARY","questions":false}}}`, "item-agent-questions"},
		{"usage breakdown shape", 6, nil, `{"method":"thread/tokenUsage/updated","params":{"threadId":"thread-one","turnId":"turn-one","tokenUsage":{"total":{},"last":{}}}}`, "usage-shape"},
		{"usage last exceeds total", 6, nil, strings.Replace(appTestUsage, `"totalTokens":7`, `"totalTokens":91`, 1), "usage-counters"},
		{"usage last changes at same total", 6, []string{appTestUsage}, strings.Replace(appTestUsage, `"totalTokens":7`, `"totalTokens":8`, 1), "usage-monotonic"},
		{"usage window changes", 6, []string{appTestUsage}, strings.Replace(appTestUsage, `"modelContextWindow":100`, `"modelContextWindow":101`, 1), "usage-window"},
		{"terminal unloaded items", 7, nil, strings.Replace(appTestTerminal, `"itemsView":"summary"`, `"itemsView":"notLoaded"`, 1), "terminal-items"},
		{"decode", 6, nil, `{"PRIVATE-CANARY":`, "record-decode"},
		{"notification envelope", 6, nil, `{"method":"warning","params":{},"PRIVATE-CANARY":true}`, "notification-envelope"},
		{"unknown method", 6, nil, `{"method":"PRIVATE-CANARY","params":{"path":"PRIVATE-CANARY"}}`, "notification-unknown"},
		{"error no retry", 6, nil, `{"method":"error","params":{"threadId":"thread-one","turnId":"turn-one","willRetry":false,"error":{"message":"PRIVATE-CANARY","codexErrorInfo":null,"additionalDetails":null}}}`, "error-notification-rejected"},
		{"error retry", 6, nil, `{"method":"error","params":{"threadId":"thread-one","turnId":"turn-one","willRetry":true,"error":{"message":"PRIVATE-CANARY"}}}`, "error-notification-rejected"},
		{"error arbitrary payload", 6, nil, `{"method":"error","params":"PRIVATE-CANARY"}`, "error-notification-rejected"},
		{"system error", 6, nil, `{"method":"thread/status/changed","params":{"threadId":"thread-one","status":{"type":"systemError"}}}`, "system-error-rejected"},
		{"uncorrelated system error", 6, nil, `{"method":"thread/status/changed","params":{"threadId":"PRIVATE-CANARY","status":{"type":"systemError"}}}`, "thread-correlation"},
		{"malformed system error", 6, nil, `{"method":"thread/status/changed","params":{"threadId":"thread-one","status":{"type":"systemError","PRIVATE-CANARY":true}}}`, "thread-status"},
		{"notification params", 6, nil, `{"method":"thread/status/changed","params":null}`, "notification-shape"},
		{"reply correlation", 6, nil, `{"id":400,"result":{}}`, "reply-correlation"},
		{"turn reply shape", 6, nil, `{"id":4,"result":{"turn":{},"PRIVATE-CANARY":true}}`, "turn-reply-shape"},
		{"turn shape", 6, nil, `{"id":4,"result":{"turn":{}}}`, "turn-shape"},
		{"turn id", 6, nil, `{"id":4,"result":{"turn":{"id":"PRIVATE-CANARY","items":[],"itemsView":"notLoaded","status":"inProgress"}}}`, "turn-id"},
		{"turn error", 6, nil, `{"id":4,"result":{"turn":{"id":"turn-one","items":[],"itemsView":"notLoaded","status":"inProgress","error":"PRIVATE-CANARY"}}}`, "turn-error"},
		{"turn timing", 6, nil, `{"id":4,"result":{"turn":{"id":"turn-one","items":[],"itemsView":"notLoaded","status":"inProgress","startedAt":-1}}}`, "turn-timing"},
		{"turn items", 6, nil, `{"id":4,"result":{"turn":{"id":"turn-one","items":null,"itemsView":"notLoaded","status":"inProgress"}}}`, "turn-items"},
		{"turn status", 6, nil, `{"id":4,"result":{"turn":{"id":"turn-one","items":[],"itemsView":"notLoaded","status":"failed"}}}`, "turn-status"},
		{"turn view", 6, nil, `{"id":4,"result":{"turn":{"id":"turn-one","items":[],"itemsView":"summary","status":"inProgress"}}}`, "turn-view"},
		{"turn notification order", 6, nil, appTestTurnStarted, "notification-order"},
		{"item shape", 6, nil, `{"method":"item/completed","params":{"threadId":"thread-one","turnId":"turn-one","completedAtMs":1,"item":null}}`, "item-shape"},
		{"item correlation", 6, nil, `{"method":"item/completed","params":{"threadId":"thread-one","turnId":"PRIVATE-CANARY","completedAtMs":1,"item":null}}`, "active-correlation"},
		{"item timing", 6, nil, `{"method":"item/completed","params":{"threadId":"thread-one","turnId":"turn-one","completedAtMs":-1,"item":null}}`, "item-timing"},
		{"prompt echo", 6, nil, `{"method":"item/completed","params":{"threadId":"thread-one","turnId":"turn-one","completedAtMs":1,"item":{"id":"user-one","type":"userMessage","content":[{"type":"text","text":"PRIVATE-CANARY"}]}}}`, "item-prompt-echo"},
		{"known compaction", 6, nil, `{"method":"item/completed","params":{"threadId":"thread-one","turnId":"turn-one","completedAtMs":1,"item":{"id":"action-one","type":"contextCompaction"}}}`, "item-context-compaction-rejected"},
		{"unknown item", 6, nil, `{"method":"item/completed","params":{"threadId":"thread-one","turnId":"turn-one","completedAtMs":1,"item":{"id":"action-one","type":"PRIVATE-CANARY"}}}`, "item-unknown"},
		{"duplicate completed", 7, nil, appTestCompleted, "item-lifecycle"},
		{"usage shape", 6, nil, `{"method":"thread/tokenUsage/updated","params":{"threadId":"thread-one","turnId":"turn-one","tokenUsage":null}}`, "usage-shape"},
		{"usage counters", 6, nil, strings.Replace(appTestUsage, `"inputTokens":20`, `"inputTokens":-1`, 1), "usage-counters"},
		{"usage decreases", 6, []string{appTestUsage}, strings.Replace(appTestUsage, `"inputTokens":20`, `"inputTokens":19`, 1), "usage-monotonic"},
		{"usage window", 6, nil, strings.Replace(appTestUsage, `"modelContextWindow":100`, `"modelContextWindow":0`, 1), "usage-window"},
		{"terminal answer missing", 6, nil, `{"method":"turn/completed","params":{"threadId":"thread-one","turn":{"id":"turn-one","items":[],"itemsView":"notLoaded","status":"completed"}}}`, "terminal-answer-missing"},
		{"terminal summary", 7, nil, `{"method":"turn/completed","params":{"threadId":"thread-one","turn":{"id":"turn-one","items":[],"itemsView":"summary","status":"completed"}}}`, "terminal-summary"},
		{"terminal mismatch", 7, nil, strings.Replace(appTestTerminal, `Ready`, `PRIVATE-CANARY`, 1), "terminal-summary-mismatch"},
		{"terminal unfinished item", 7, []string{`{"method":"item/started","params":{"threadId":"thread-one","turnId":"turn-one","startedAtMs":1,"item":{"id":"reason-one","type":"reasoning"}}}`}, appTestTerminal, "terminal-items-incomplete"},
		{"known delta", 6, nil, `{"method":"item/agentMessage/delta","params":{"delta":"PRIVATE-CANARY"}}`, "agent-message-delta-rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := appTestStart(t)
			appTestFeed(t, s, appTestRecords()[:tc.prefix])
			appTestFeed(t, s, tc.extra)
			action, err := s.receive([]byte(tc.frame))
			want := "codex-app-server-invalid-protocol-turn-" + tc.fault
			got := classifyAppServerError(fmt.Errorf("PRIVATE-CANARY: %w", err))
			if !action.closeStdin || !errors.Is(err, errAppServerProtocolTurn) || got.Code != "protocol" || got.Reason != want {
				t.Errorf("receive(%s) = close %v, %s/%s, want close true, protocol/%s", tc.name, action.closeStdin, got.Code, got.Reason, want)
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE-CANARY") || strings.Contains(got.Error(), "PRIVATE-CANARY") {
				t.Error("receive retained private record content")
			}
			_, later := s.receive([]byte(`{"method":"warning","params":null}`))
			facts, finished := s.finish()
			if later != err || finished != err || facts != (appServerResult{}) {
				t.Error("receive lost first error identity or finish returned nonzero facts")
			}
		})
	}
}

// A diagnostic must name the first failed check without admitting the record or
// evaluating its later, potentially stateful thread correlation.
func TestAppServerMCPStartupDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, params, phase, point string
		prefix                     int
	}{
		{"null params", `null`, "thread", "shape", 3},
		{"missing name", `{"status":"failed"}`, "thread", "shape", 3},
		{"missing status", `{"name":"PRIVATE-CANARY"}`, "thread", "shape", 3},
		{"shape before name", `{"name":false,"status":"failed","PRIVATE-CANARY":true,"threadId":"thread-one"}`, "thread", "shape", 3},
		{"name before status", `{"name":null,"status":"failed","error":"PRIVATE-CANARY","threadId":"thread-one"}`, "thread", "name", 3},
		{"failed status", `{"name":"PRIVATE-CANARY","status":"failed","error":null,"failureReason":null,"threadId":"thread-one"}`, "turn", "failed", 6},
		{"failed before metadata", `{"name":"PRIVATE-CANARY","status":"failed","error":{"private":"PRIVATE-CANARY"},"failureReason":false,"threadId":"thread-one"}`, "thread", "failed", 3},
		{"status before metadata", `{"name":"PRIVATE-CANARY","status":"PRIVATE-CANARY","error":false,"failureReason":false,"threadId":"thread-one"}`, "thread", "status", 3},
		{"status object", `{"name":"PRIVATE-CANARY","status":{"private":"PRIVATE-CANARY"},"threadId":"thread-one"}`, "thread", "status", 3},
		{"status null", `{"name":"PRIVATE-CANARY","status":null,"threadId":"thread-one"}`, "thread", "status", 3},
		{"error before failure reason", `{"name":"PRIVATE-CANARY","status":"ready","error":"PRIVATE-CANARY","failureReason":"PRIVATE-CANARY","threadId":"thread-one"}`, "thread", "error", 3},
		{"false error", `{"name":"PRIVATE-CANARY","status":"ready","error":false,"threadId":"thread-one"}`, "thread", "error", 3},
		{"failure reason before thread", `{"name":"PRIVATE-CANARY","status":"ready","error":null,"failureReason":"PRIVATE-CANARY","threadId":"thread-one"}`, "thread", "failure-reason", 3},
		{"false failure reason", `{"name":"PRIVATE-CANARY","status":"ready","failureReason":false,"threadId":"thread-one"}`, "thread", "failure-reason", 3},
		{"mismatched thread", `{"name":"PRIVATE-CANARY","status":"ready","threadId":"PRIVATE-CANARY"}`, "turn", "thread", 6},
		{"malformed thread", `{"name":"PRIVATE-CANARY","status":"ready","threadId":false}`, "thread", "thread", 3},
		{"empty thread", `{"name":"PRIVATE-CANARY","status":"ready","threadId":""}`, "thread", "thread", 3},
		{"thread before request", `{"name":"PRIVATE-CANARY","status":"ready","threadId":"thread-one"}`, "discovery", "thread", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := appTestStart(t)
			appTestFeed(t, s, appTestRecords()[:tc.prefix])
			threadID := s.threadID
			action, err := s.receive([]byte(`{"method":"mcpServer/startupStatus/updated","params":` + tc.params + `}`))
			got := classifyAppServerError(fmt.Errorf("PRIVATE-CANARY: %w", err))
			want := "codex-app-server-invalid-protocol-" + tc.phase + "-mcp-startup-" + tc.point
			if !action.closeStdin || len(action.write) != 0 || !errors.Is(err, errAppServerProtocol) || got.Code != "protocol" || got.Reason != want {
				t.Errorf("receive(%s) = close %v, write %d, %s/%s, want close true, write 0, protocol/%s", tc.name, action.closeStdin, len(action.write), got.Code, got.Reason, want)
			}
			if s.threadID != threadID {
				t.Errorf("receive(%s) thread ID = %q, want unchanged %q", tc.name, s.threadID, threadID)
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE-CANARY") || strings.Contains(got.Error(), "PRIVATE-CANARY") {
				t.Errorf("receive(%s) exposed private record content", tc.name)
			}
			_, later := s.receive([]byte(`{"method":"warning","params":null}`))
			facts, finished := s.finish()
			if later != err || finished != err || facts != (appServerResult{}) {
				t.Errorf("receive(%s) lost first error identity or finish returned nonzero facts", tc.name)
			}
		})
	}
}

func TestAppServerMCPStartupAccepted(t *testing.T) {
	for _, tc := range []struct {
		name, params, threadID string
		prefix                 int
	}{
		{"starting null metadata", `{"name":"synthetic","status":"starting","threadId":null,"error":null,"failureReason":null}`, "", 1},
		{"ready omitted metadata", `{"name":"synthetic","status":"ready"}`, "", 3},
		{"cancelled correlated thread", `{"name":"synthetic","status":"cancelled","threadId":"thread-one","error":null,"failureReason":null}`, "thread-one", 6},
		{"empty name", `{"name":"","status":"starting"}`, "", 3},
		{"tentative thread", `{"name":"synthetic","status":"ready","threadId":"thread-one"}`, "thread-one", 3},
		{"null thread after start", `{"name":"synthetic","status":"cancelled","threadId":null}`, "thread-one", 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := appTestStart(t)
			appTestFeed(t, s, appTestRecords()[:tc.prefix])
			action, err := s.receive([]byte(`{"method":"mcpServer/startupStatus/updated","params":` + tc.params + `}`))
			if err != nil || action.closeStdin || len(action.write) != 0 || s.threadID != tc.threadID {
				t.Fatalf("receive(%s) = close %v, write %d, thread %q, error %v, want close false, write 0, thread %q, nil", tc.name, action.closeStdin, len(action.write), s.threadID, err, tc.threadID)
			}
			appTestFeed(t, s, appTestRecords()[tc.prefix:])
			facts, err := s.finish()
			if err != nil || facts != (appServerResult{Answer: "OK.\nReady�"}) {
				t.Errorf("finish(%s) = %+v, %v, want original answer only, nil", tc.name, facts, err)
			}
		})
	}
}
