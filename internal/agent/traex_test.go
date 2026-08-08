package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTraexAgent_BuildArgs(t *testing.T) {
	ta := &traexAgent{bin: "traex"}
	got := ta.buildArgs("/tmp/schema.json", "")
	want := []string{
		"exec", "-", "--json", "--output-schema", "/tmp/schema.json",
		"--dangerously-bypass-approvals-and-sandbox", "--color", "never",
	}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg[%d] = %q, want %q in %v", i, got[i], want[i], got)
		}
	}
}

func TestTraexAgent_BuildArgs_ResumeUsesNativeNarrowSurface(t *testing.T) {
	ta := &traexAgent{bin: "traex", extraArgs: []string{"-m", "doubao-seed-2.0-code"}}
	got := ta.buildArgs("/tmp/schema.json", "thread-42")
	joined := strings.Join(got, " ")
	if !strings.HasPrefix(joined, "exec resume -m doubao-seed-2.0-code thread-42 - --json") {
		t.Fatalf("resume args use the wrong TraeX shape: %v", got)
	}
	for _, forbidden := range []string{"--output-schema", "--color"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("TraeX resume does not accept %s: %v", forbidden, got)
		}
	}
	if !strings.Contains(joined, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("resume args missing execution mode: %v", got)
	}
}

func TestTraexAgent_BuildArgs_OptOutNeutralizationIsMandatoryAndLast(t *testing.T) {
	ta := &traexAgent{
		bin:                    "traex",
		extraArgs:              []string{"-c", "project_doc_max_bytes=32768", "--model", "custom"},
		disableProjectSettings: true,
	}
	got := ta.buildArgs("", "")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-c project_doc_max_bytes=32768") {
		t.Fatalf("user config override was lost: %v", got)
	}
	wantSuffix := []string{"-c", "project_doc_max_bytes=0", "--ignore-rules"}
	if len(got) < len(wantSuffix) {
		t.Fatalf("args too short: %v", got)
	}
	for i := range wantSuffix {
		if got[len(got)-len(wantSuffix)+i] != wantSuffix[i] {
			t.Fatalf("mandatory neutralization must be the final argv suffix: %v", got)
		}
	}
	if !ta.NeutralizesGateInstructions() {
		t.Fatal("TraeX must report neutralized when the mandatory suffix is enabled")
	}
}

func TestTraexAgent_NeutralizationIsHonestWithoutOptOut(t *testing.T) {
	if (&traexAgent{bin: "traex"}).NeutralizesGateInstructions() {
		t.Fatal("TraeX must not claim project instructions are suppressed without the trusted opt-out")
	}
}

func TestTraexAgent_NeutralizationFailsClosedAfterOptionTerminator(t *testing.T) {
	ta := &traexAgent{
		bin:                    "traex",
		extraArgs:              []string{"--"},
		disableProjectSettings: true,
	}
	if ta.NeutralizesGateInstructions() {
		t.Fatal("TraeX must not claim mandatory flags after -- can be parsed as options")
	}
}

func TestParseTraexEvents_CapturesStreamingResultSessionUsageAndErrors(t *testing.T) {
	events := strings.Join([]string{
		`not-json`,
		`{"type":"thread.started","thread_id":"019fe039-8a44-7230-a9a2-68cad34db92d"}`,
		`{"type":"item.completed","item":{"type":"reasoning","text":"thinking"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}`,
		`{"type":"error","message":"later warning"}`,
		`{"type":"turn.failed","error":{"message":"turn failed detail"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":47,"cache_creation_input_tokens":3,"cached_input_tokens":11,"output_tokens":13,"reasoning_output_tokens":5}}`,
		"",
	}, "\n")
	var chunks []string
	var usage TokenUsage
	var lastMessage, traexErr, threadID string
	var turnFailed bool
	metrics := newCodexMetricsAccumulator()
	if err := parseTraexEvents(context.Background(), strings.NewReader(events), func(s string) {
		chunks = append(chunks, s)
	}, &usage, &lastMessage, &traexErr, &threadID, &turnFailed, metrics); err != nil {
		t.Fatalf("parseTraexEvents: %v", err)
	}
	if threadID != "019fe039-8a44-7230-a9a2-68cad34db92d" {
		t.Fatalf("threadID = %q", threadID)
	}
	if lastMessage != `{"ok":true}` || len(chunks) != 1 || chunks[0] != lastMessage {
		t.Fatalf("message/chunks = %q / %v", lastMessage, chunks)
	}
	if traexErr != "turn failed detail" {
		t.Fatalf("traexErr = %q", traexErr)
	}
	if !turnFailed {
		t.Fatal("turn.failed event was not retained")
	}
	if usage.InputTokens != 47 || usage.CacheCreationTokens != 3 || usage.CacheReadTokens != 11 || usage.OutputTokens != 13 || usage.ReasoningTokens != 5 {
		t.Fatalf("usage = %+v", usage)
	}
	if !usage.Reported || !usage.CacheCreationReported {
		t.Fatalf("usage reporting flags = %+v", usage)
	}
}

func writeFakeTraex(t *testing.T, dir, posixScript, windowsScript string) string {
	t.Helper()
	name := "traex"
	script := posixScript
	if runtime.GOOS == "windows" {
		name = "traex.cmd"
		script = windowsScript
	}
	bin := filepath.Join(dir, name)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake traex: %v", err)
	}
	return bin
}

func TestTraexAgent_RunDeliversSchemaOnStdinAndReturnsSession(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeTraex(t, dir, `#!/bin/sh
dir=$(dirname "$0")
printf '%s\n' "$@" > "$dir/args.txt"
want_schema=""
for arg do
  if [ "$want_schema" = "1" ]; then
    cp "$arg" "$dir/schema.json"
    want_schema=""
  elif [ "$arg" = "--output-schema" ]; then
    want_schema="1"
  fi
done
cat > "$dir/prompt.txt"
printf '%s\n' '{"type":"thread.started","thread_id":"thread-fresh"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":10,"cache_creation_input_tokens":2,"cached_input_tokens":3,"output_tokens":4,"reasoning_output_tokens":1}}'
`, strings.Join([]string{
		"@echo off",
		"setlocal",
		"set \"dir=%~dp0\"",
		"> \"%dir%args.txt\" echo(%*",
		"copy /Y \"%~5\" \"%dir%schema.json\" >nul || exit /b 3",
		"more > \"%dir%prompt.txt\"",
		"echo {\"type\":\"thread.started\",\"thread_id\":\"thread-fresh\"}",
		"echo {\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{\\\"ok\\\":true}\"}}",
		"echo {\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":10,\"cache_creation_input_tokens\":2,\"cached_input_tokens\":3,\"output_tokens\":4,\"reasoning_output_tokens\":1}}",
	}, "\r\n"))

	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	var lifecycle []LifecycleEvent
	result, err := (&traexAgent{bin: bin}).Run(context.Background(), RunOpts{
		Prompt:      "review the branch",
		CWD:         t.TempDir(),
		JSONSchema:  schema,
		Session:     &SessionRef{},
		OnLifecycle: func(ev LifecycleEvent) { lifecycle = append(lifecycle, ev) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(result.Output) != `{"ok":true}` || result.SessionID != "thread-fresh" || result.Resumed {
		t.Fatalf("result = %+v", result)
	}
	if !result.SessionUsageCumulative || !result.CacheCreationReported {
		t.Fatalf("TraeX usage/session flags = %+v", result)
	}
	prompt, err := os.ReadFile(filepath.Join(dir, "prompt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(prompt)) != "review the branch" {
		t.Fatalf("stdin prompt = %q", prompt)
	}
	capturedSchema, err := os.ReadFile(filepath.Join(dir, "schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantSchema := `{"additionalProperties":false,"properties":{"ok":{"type":"boolean"}},"required":["ok"],"type":"object"}`
	if string(capturedSchema) != wantSchema {
		t.Fatalf("schema = %s, want %s", capturedSchema, wantSchema)
	}
	if len(lifecycle) != 2 || lifecycle[0].Phase != LifecyclePhaseStart || lifecycle[1].Phase != LifecyclePhaseExit {
		t.Fatalf("lifecycle = %+v", lifecycle)
	}
}

func TestTraexAgent_RunResumeInlinesSchemaAndKeepsSessionIdentity(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeTraex(t, dir, `#!/bin/sh
dir=$(dirname "$0")
printf '%s\n' "$@" > "$dir/args.txt"
cat > "$dir/prompt.txt"
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":25,"cache_creation_input_tokens":0,"cached_input_tokens":5,"output_tokens":7,"reasoning_output_tokens":0}}'
`, strings.Join([]string{
		"@echo off",
		"setlocal",
		"set \"dir=%~dp0\"",
		"> \"%dir%args.txt\" echo(%*",
		"more > \"%dir%prompt.txt\"",
		"echo {\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{\\\"ok\\\":true}\"}}",
		"echo {\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":25,\"cache_creation_input_tokens\":0,\"cached_input_tokens\":5,\"output_tokens\":7,\"reasoning_output_tokens\":0}}",
	}, "\r\n"))

	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	result, err := (&traexAgent{bin: bin}).Run(context.Background(), RunOpts{
		Prompt:     "fix the finding",
		CWD:        t.TempDir(),
		JSONSchema: schema,
		Session:    &SessionRef{ID: "thread-resume", Agent: "traex"},
	})
	if err != nil {
		t.Fatalf("Run resume: %v", err)
	}
	if !result.Resumed || result.SessionID != "thread-resume" || string(result.Output) != `{"ok":true}` {
		t.Fatalf("result = %+v", result)
	}
	argsRaw, err := os.ReadFile(filepath.Join(dir, "args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(strings.Fields(string(argsRaw)), " ")
	if !strings.Contains(args, "exec resume") || !strings.Contains(args, "thread-resume") || strings.Contains(args, "--output-schema") {
		t.Fatalf("resume argv = %q", args)
	}
	prompt, err := os.ReadFile(filepath.Join(dir, "prompt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fix the finding", "no-mistakes final output contract", `"ok"`} {
		if !strings.Contains(string(prompt), want) {
			t.Fatalf("resume stdin missing %q: %s", want, prompt)
		}
	}
}

func TestTraexAgent_RunIncludesJSONLErrorAndStderrOnFailure(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeTraex(t, dir, `#!/bin/sh
printf '%s\n' '{"type":"error","message":"TraeX schema rejected"}'
echo 'provider detail' >&2
exit 1
`, strings.Join([]string{
		"@echo off",
		"echo {\"type\":\"error\",\"message\":\"TraeX schema rejected\"}",
		"echo provider detail 1>&2",
		"exit /b 1",
	}, "\r\n"))
	_, err := (&traexAgent{bin: bin}).Run(context.Background(), RunOpts{Prompt: "p", CWD: t.TempDir()})
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, want := range []string{"TraeX schema rejected", "provider detail"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
}

func TestTraexAgent_RunSurfacesTurnFailureEvenWhenProcessExitsZero(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeTraex(t, dir, `#!/bin/sh
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"stale partial answer"}}'
printf '%s\n' '{"type":"turn.failed","error":{"message":"provider quota exhausted"}}'
`, strings.Join([]string{
		"@echo off",
		"echo {\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"stale partial answer\"}}",
		"echo {\"type\":\"turn.failed\",\"error\":{\"message\":\"provider quota exhausted\"}}",
	}, "\r\n"))
	_, err := (&traexAgent{bin: bin}).Run(context.Background(), RunOpts{Prompt: "p", CWD: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "provider quota exhausted") {
		t.Fatalf("turn failure was not surfaced: %v", err)
	}
}
