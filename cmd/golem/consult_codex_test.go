//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/consult"
)

const codexConsultStream = `{"type":"thread.started","thread_id":"synthetic"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"answer","type":"agent_message","text":"OK"}}
{"type":"turn.completed","usage":{"input_tokens":7,"cached_input_tokens":2,"output_tokens":3,"reasoning_output_tokens":1}}`

func fakeCodexConsultants(t *testing.T, stream string) map[string]consult.Consultant {
	t.Helper()
	path := fakeConsultantsFile(t, "codex", stream, 0)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg consult.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	c := &cfg.Consultants[0]
	c.Adapter, c.Model, c.TrustedVendorRuntime = "codex", "gpt-6-astra", true
	body, err := os.ReadFile(c.Command)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), "2.1.240 (Claude Code)", "codex-cli 0.153.4", 1))
	// Record every launch, including an unauthorized preflight-only attempt.
	body = []byte(strings.Replace(string(body), "#!/bin/sh\n", "#!/bin/sh\nprintf 'start\\n' >> '"+strings.ReplaceAll(filepath.Join(filepath.Dir(c.Command), "starts"), "'", "'\"'\"'")+"'\n", 1))
	if err := os.WriteFile(c.Command, body, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := consult.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func TestConsultCodexStagesAdviceForOneGoal(t *testing.T) {
	caller := &scriptCaller{}
	sess := newTestSession(t, caller, t.TempDir())
	sess.interceptorsOn = true
	sess.consultants = fakeCodexConsultants(t, codexConsultStream)
	var out bytes.Buffer
	dispatchSlash(context.Background(), &out, sess, "/consult codex question")
	if sess.advisory == nil || !strings.Contains(out.String(), "codex (codex 0.153.4, model gpt-6-astra, exit 0,") || !strings.Contains(out.String(), "staged for the next goal") {
		t.Fatalf("Codex staging: %s, %+v", out.String(), sess.advisory)
	}
	if sess.advisory.Content != "OK" || sess.advisory.Model != "gpt-6-astra" || sess.advisory.Tool != "codex 0.153.4" || sess.advisory.Digest != "565339bc4d33d72817b583024112eb7f5cdf3e5eef0252d6ec1b9c9a94e12bb3" {
		t.Fatalf("Codex attribution: %+v", sess.advisory)
	}
	if got := consultStdin(t, sess.consultants["codex"]); got != "question\n" {
		t.Fatalf("Codex stdin = %q", got)
	}
	if _, err := runOnce(context.Background(), &out, nil, sess, "next goal", nil); err != nil {
		t.Fatal(err)
	}
	if sess.advisory != nil {
		t.Fatal("Codex advice not consumed")
	}
	last := caller.lastRequest.Messages[len(caller.lastRequest.Messages)-1].Content
	if !strings.Contains(last, "<<<CONSULT_ADVICE ") || !strings.Contains(last, "next goal") || !strings.Contains(last, "gpt-6-astra") {
		t.Fatalf("Codex fence missing: %s", last)
	}
	for _, want := range []string{"\nOK\n", "codex 0.153.4", "565339bc4d33d72817b583024112eb7f5cdf3e5eef0252d6ec1b9c9a94e12bb3"} {
		if !strings.Contains(last, want) {
			t.Fatalf("Codex first-goal wire missing %q: %s", want, last)
		}
	}
	if _, err := runOnce(context.Background(), &out, nil, sess, "second goal", nil); err != nil {
		t.Fatal(err)
	}
	for _, m := range caller.lastRequest.Messages {
		if strings.Contains(m.Content, "CONSULT_ADVICE") {
			t.Fatal("Codex advice persisted/replayed on second goal")
		}
	}
}

func TestConsultCodexRetainsAdviceOnFailure(t *testing.T) {
	for _, kind := range []string{"host", "protocol", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			sess := newTestSession(t, &scriptCaller{}, t.TempDir())
			sess.interceptorsOn = true
			old := &agent.Advisory{Source: "previous", Tool: "claude 2.1.240", Model: "opus", Digest: sha256Hex("previous"), Content: "previous", Origin: agent.OriginModel}
			sess.advisory = old
			stream := codexConsultStream
			if kind == "protocol" {
				stream += "\nSECRET_VENDOR_DIAGNOSTIC"
			}
			sess.consultants = fakeCodexConsultants(t, stream)
			if kind == "host" {
				path := sess.consultants["codex"].Command
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(body, []byte("echo SECRET_VENDOR_DIAGNOSTIC >&2\nexit 3\n")...), 0700); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "canceled" {
				cancel()
			}
			var out bytes.Buffer
			dispatchSlash(ctx, &out, sess, "/consult codex q")
			if sess.advisory != old || strings.Contains(out.String(), "SECRET_VENDOR_DIAGNOSTIC") || strings.Contains(out.String(), "staged for the next goal") {
				t.Fatalf("%s failure changed slot/leaked bytes: %s", kind, out.String())
			}
			want := "consult failed:"
			if kind == "canceled" {
				want = "consult canceled"
			}
			if !strings.Contains(out.String(), want) {
				t.Fatalf("%s failure = %s", kind, out.String())
			}
		})
	}
}

func TestConsultCodexRequiresInterceptors(t *testing.T) {
	sess := newTestSession(t, &scriptCaller{}, t.TempDir())
	sess.consultants = fakeCodexConsultants(t, codexConsultStream)
	var out bytes.Buffer
	dispatchSlash(context.Background(), &out, sess, "/consult codex q")
	if !strings.Contains(out.String(), "consult requires -interceptors") || sess.advisory != nil {
		t.Fatalf("ungated Codex = %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(sess.consultants["codex"].Command), "starts")); !os.IsNotExist(err) {
		t.Fatal("ungated Codex started a process")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(sess.consultants["codex"].Command), "stdin")); !os.IsNotExist(err) {
		t.Fatal("ungated Codex started prompt")
	}
}

func TestConsultCodexBlockedAdviceIsNotStaged(t *testing.T) {
	caller := &scriptCaller{}
	sess := newTestSession(t, caller, t.TempDir())
	sess.interceptorsOn = true
	sess.orch = agent.New(caller, agent.ContextManager{}, agent.WithInterceptors(blockAdvice{marker: "OK", verdict: agent.VerdictBlock}))
	sess.consultants = fakeCodexConsultants(t, codexConsultStream)
	var out bytes.Buffer
	dispatchSlash(context.Background(), &out, sess, "/consult codex q")
	if sess.advisory != nil || !strings.Contains(out.String(), "blocked by interceptor policy") {
		t.Fatalf("Codex block: %s", out.String())
	}
}

func TestConsultCodexAnnotationAndStepZero(t *testing.T) {
	caller := &scriptCaller{}
	root := t.TempDir()
	sess := newTestSession(t, caller, root)
	sess.interceptorsOn = true
	sess.orch = agent.New(caller, agent.ContextManager{}, agent.WithInterceptors(blockAdvice{marker: "OK", verdict: agent.VerdictTag}))
	sess.consultants = fakeCodexConsultants(t, codexConsultStream)
	var out bytes.Buffer
	dispatchSlash(context.Background(), &out, sess, "/consult codex q")
	if sess.advisory == nil || sess.advisory.Content != "OK" || sess.advisory.Digest != sha256Hex("OK") || sess.advisory.Annotation == "" {
		t.Fatalf("Codex annotation: %+v, %s", sess.advisory, out.String())
	}
	// Preview approval does not bypass the authoritative next-goal inspection.
	sess.orch = agent.New(caller, agent.ContextManager{}, agent.WithInterceptors(blockAdvice{marker: "OK", verdict: agent.VerdictBlock}))
	sess.runtime = newTestRuntime(t, root, sess.baseSystem, sess.orch, nil)
	_, err := runOnce(context.Background(), &out, nil, sess, "next goal", nil)
	if !errors.Is(err, agent.ErrAdvisoryBlocked) || sess.advisory != nil || len(caller.lastRequest.Messages) != 0 {
		t.Fatalf("Codex step-zero = %v, slot=%+v", err, sess.advisory)
	}
}
