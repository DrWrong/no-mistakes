package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractTraexPromptReadsManagedStdinSentinel(t *testing.T) {
	got, err := extractTraexPrompt([]string{"exec", "-m", "model", "-", "--json"}, strings.NewReader("review from stdin"))
	if err != nil {
		t.Fatalf("extractTraexPrompt: %v", err)
	}
	if got != "review from stdin" {
		t.Fatalf("prompt = %q", got)
	}
}

func TestExtractTraexPromptReadsResumeStdin(t *testing.T) {
	got, err := extractTraexPrompt([]string{"exec", "resume", "thread-1", "-", "--json"}, strings.NewReader("fix from stdin"))
	if err != nil {
		t.Fatalf("extractTraexPrompt: %v", err)
	}
	if got != "fix from stdin" {
		t.Fatalf("prompt = %q", got)
	}
}

func TestRunTraexEmitsNativeJSONLAndFiltersSchema(t *testing.T) {
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.json")
	if err := os.WriteFile(schemaPath, []byte(`{"type":"object","properties":{"findings":{}},"additionalProperties":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKEAGENT_FIXTURE", "")
	t.Setenv("FAKEAGENT_LOG", "")
	scenario := defaultScenario()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := runTraex([]string{"exec", "-", "--json", "--output-schema", schemaPath}, strings.NewReader("review"), scenario)
	_ = w.Close()
	os.Stdout = oldStdout
	out, _ := io.ReadAll(r)
	_ = r.Close()
	if code != 0 {
		t.Fatalf("runTraex code = %d", code)
	}
	for _, want := range [][]byte{[]byte(`"type":"thread.started"`), []byte(`"type":"agent_message"`), []byte(`"cache_creation_input_tokens"`)} {
		if !bytes.Contains(out, want) {
			t.Fatalf("output missing %s: %s", want, out)
		}
	}
	if bytes.Contains(out, []byte(`"risk_level"`)) {
		t.Fatalf("structured response was not filtered to schema: %s", out)
	}
}
