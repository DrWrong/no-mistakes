package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// runTraex speaks the native TraeX exec JSONL contract. It intentionally owns
// a separate entry point from the Codex fake even though the event envelopes
// overlap, so stdin prompt transport, cache-creation usage, and future wire
// drift remain independently testable.
func runTraex(args []string, stdin io.Reader, scenario *Scenario) int {
	prompt, err := extractTraexPrompt(args, stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent: traex prompt: %v\n", err)
		return 1
	}
	logInvocation("traex", prompt, args)

	action := scenario.Match(prompt)
	if err := applyAction(action); err != nil {
		return 1
	}
	if schemaPath := extractCodexOutputSchema(args); schemaPath != "" && action.Structured != nil {
		filtered, err := filterStructuredToSchema(action.Structured, schemaPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakeagent: traex schema filter: %v\n", err)
			return 1
		}
		action.Structured = filtered
	}

	flavour := "structured"
	if !action.hasStructuredOutput() && action.Text != "" {
		flavour = "plain"
	}
	if data, err := readFixtureFile(fixtureDir("traex"), flavour, ".jsonl"); err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent: traex fixture: %v\n", err)
		return 1
	} else if data != nil {
		patched, err := patchCodexFixture(data, action)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakeagent: traex patch: %v\n", err)
			return 1
		}
		_, _ = os.Stdout.Write(patched)
		return 0
	}

	body := action.textOrDefault()
	if action.hasStructuredOutput() {
		body = string(action.structuredJSON())
	}
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(map[string]any{
		"type":      "thread.started",
		"thread_id": "fake-traex-thread",
	})
	_ = enc.Encode(map[string]any{
		"type": "item.completed",
		"item": map[string]any{"type": "agent_message", "text": body},
	})
	_ = enc.Encode(map[string]any{
		"type": "turn.completed",
		"usage": map[string]int{
			"input_tokens":                100,
			"cache_creation_input_tokens": 0,
			"cached_input_tokens":         0,
			"output_tokens":               50,
			"reasoning_output_tokens":     0,
		},
	})
	return 0
}

func extractTraexPrompt(args []string, stdin io.Reader) (string, error) {
	for _, arg := range args {
		if arg != "-" {
			continue
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, stdin); err != nil {
			return "", err
		}
		return buf.String(), nil
	}
	return "", fmt.Errorf("missing managed stdin prompt sentinel")
}
