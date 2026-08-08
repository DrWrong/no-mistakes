package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// recordTraex captures TraeX's native exec JSONL stream. The recorder uses an
// ephemeral session so refreshing fixtures does not leave durable test threads
// in the operator's TraeX state.
func recordTraex(ctx context.Context, out string, args []string) int {
	bin, forward := splitBinArgs(args, "traex")
	schema := `{"type":"object","properties":{"findings":{"type":"array","items":{"type":"object"}},"risk_level":{"type":"string"},"risk_rationale":{"type":"string"},"summary":{"type":"string"}},"required":["findings","risk_level","risk_rationale","summary"],"additionalProperties":false}`
	if err := captureTraex(ctx, bin, forward,
		`Return only JSON: {"findings":[],"risk_level":"low","risk_rationale":"no risks","summary":"ok"}`,
		schema, filepath.Join(out, "structured.jsonl")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := captureTraex(ctx, bin, forward, "Reply with the literal word OK and nothing else.", "", filepath.Join(out, "plain.jsonl")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "traex fixtures written to %s\n", out)
	return 0
}

func captureTraex(ctx context.Context, bin string, forward []string, prompt, schema, outPath string) error {
	tmp, err := os.MkdirTemp("", "recordtraex-*")
	if err != nil {
		return fmt.Errorf("tempdir: %w", err)
	}
	defer os.RemoveAll(tmp)

	cmdArgs := []string{"exec"}
	cmdArgs = append(cmdArgs, forward...)
	cmdArgs = append(cmdArgs, "-", "--json")
	if schema != "" {
		schemaPath := filepath.Join(tmp, "schema.json")
		if err := os.WriteFile(schemaPath, []byte(schema), 0o644); err != nil {
			return fmt.Errorf("write schema: %w", err)
		}
		cmdArgs = append(cmdArgs, "--output-schema", schemaPath)
	}
	cmdArgs = append(cmdArgs,
		"--ephemeral",
		"--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox",
		"--color", "never",
		"-c", "project_doc_max_bytes=0",
		"--ignore-rules",
	)
	cmd := exec.CommandContext(ctx, bin, cmdArgs...)
	cmd.Dir = tmp
	cmd.Stdin = strings.NewReader(prompt)

	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", outPath, err)
	}
	defer f.Close()
	cmd.Stdout = f
	cmd.Stderr = os.Stderr
	fmt.Fprintf(os.Stderr, "recording traex → %s\n", outPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run traex: %w", err)
	}
	if err := scrubFile(outPath); err != nil {
		return fmt.Errorf("scrub %s: %w", outPath, err)
	}
	return nil
}
