package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// traexAgent spawns TraeX's native non-interactive CLI for each invocation.
// TraeX deliberately has its own adapter even though its JSONL envelopes are
// codex-shaped: its prompt transport, resume flag surface, cache-creation
// usage, project-instruction controls, and binary contract are independently
// verified and must not silently inherit Codex assumptions.
type traexAgent struct {
	bin       string
	extraArgs []string
	// disableProjectSettings is the resolved, trusted-only opt-out. When true,
	// buildArgs suppresses TraeX's project AGENTS.md and execpolicy rules.
	disableProjectSettings bool
}

func (a *traexAgent) Name() string { return "traex" }

// SupportsSessionResume reports TraeX's native durable-session capability:
// exec --json emits thread.started and exec resume <id> continues the same id.
func (a *traexAgent) SupportsSessionResume() bool { return true }

func (a *traexAgent) ReportsAgentAttempts() bool { return true }

// NeutralizesGateInstructions reports whether the trusted project-settings
// opt-out is active and user arguments have not terminated option parsing.
//
// Verified against TraeX 0.200.19: a unique AGENTS.md marker is present in the
// model context by default and absent with project_doc_max_bytes=0; TraeX does
// not load a colocated CLAUDE.md marker. --ignore-rules completes suppression
// of project execpolicy. Repeated -c values use last-one-wins semantics, so
// buildArgs appends both mandatory controls after every user override. Global
// config validation rejects a bare --, and this independent check keeps direct
// construction fail-closed too.
func (a *traexAgent) NeutralizesGateInstructions() bool {
	return a.disableProjectSettings && !traexArgsTerminateOptions(a.extraArgs)
}

func (a *traexAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "traex", opts, claudeMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *traexAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	resumeID := ""
	if opts.Session != nil {
		resumeID = opts.Session.ID
	}

	validationSchema := opts.JSONSchema
	schemaPath := ""
	if len(opts.JSONSchema) > 0 {
		schema, err := codexOutputSchema(opts.JSONSchema)
		if err != nil {
			return nil, fmt.Errorf("traex schema normalize: %w", err)
		}
		validationSchema = schema

		// TraeX exec accepts --output-schema, but exec resume 0.200.19 rejects
		// it. Fresh turns use the native schema file; resume turns receive the
		// same normalized schema as an explicit stdin contract below and are
		// still validated locally before Result is returned.
		if resumeID == "" {
			f, err := os.CreateTemp("", "no-mistakes-traex-schema-*.json")
			if err != nil {
				return nil, fmt.Errorf("traex schema temp file: %w", err)
			}
			schemaPath = f.Name()
			if _, err := f.Write(schema); err != nil {
				_ = f.Close()
				_ = os.Remove(schemaPath)
				return nil, fmt.Errorf("traex schema temp file write: %w", err)
			}
			if err := f.Close(); err != nil {
				_ = os.Remove(schemaPath)
				return nil, fmt.Errorf("traex schema temp file close: %w", err)
			}
			defer os.Remove(schemaPath)
		}
	}

	prompt := opts.Prompt
	if resumeID != "" && len(validationSchema) > 0 {
		prompt = buildTraexResumePrompt(prompt, validationSchema)
	}
	args := a.buildArgs(schemaPath, resumeID)
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	// TraeX documents `-` as stdin prompt transport for both exec and
	// exec resume. It keeps large pipeline prompts and their contents out of
	// argv while preserving an unambiguous positional shape.
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = gitSafeEnv(opts.CWD)
	shellenv.ConfigureShellCommand(cmd)

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	started, err := startNativeAgentCommand(cmd)
	if err != nil {
		return nil, fmt.Errorf("traex start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, "traex", pid)

	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(started.stderr)
	}()

	var usage TokenUsage
	var lastMessage, traexErr, threadID string
	var turnFailed bool
	metrics := newCodexMetricsAccumulator()
	if err := parseTraexEvents(ctx, started.stdout, opts.OnChunk, &usage, &lastMessage, &traexErr, &threadID, &turnFailed, metrics); err != nil {
		err = started.waitAfterParseError(err)
		stderrWG.Wait()
		retErr := fmt.Errorf("traex parse events: %w", err)
		emitAgentExited(opts, "traex", pid, retErr)
		return nil, retErr
	}

	waitErr := started.wait()
	stderrWG.Wait()
	if waitErr != nil {
		detail := strings.TrimSpace(traexErr)
		stderr := strings.TrimSpace(string(stderrBuf))
		if detail != "" && stderr != "" {
			detail += "; " + stderr
		} else if detail == "" {
			detail = stderr
		}
		retErr := fmt.Errorf("traex exited: %w: %s", waitErr, detail)
		emitAgentExited(opts, "traex", pid, retErr)
		return nil, retErr
	}
	if turnFailed || (lastMessage == "" && traexErr != "") {
		retErr := fmt.Errorf("traex failed: %s", strings.TrimSpace(traexErr))
		emitAgentExited(opts, "traex", pid, retErr)
		return nil, retErr
	}
	if threadID == "" && resumeID != "" {
		// The requested durable thread remains the session identity even if a
		// future TraeX version stops repeating thread.started on resume.
		threadID = resumeID
	}

	res, err := finalizeTextResult("traex", lastMessage, validationSchema, usage)
	if res != nil {
		res.SessionID = threadID
		res.Resumed = resumeID != ""
		res.SessionUsageCumulative = true
		res.CacheCreationReported = usage.CacheCreationReported
		m := metrics.metrics()
		res.Metrics = &m
	}
	emitAgentExited(opts, "traex", pid, err)
	return res, err
}

func (a *traexAgent) Close() error { return nil }

// buildArgs constructs TraeX's native argv. User overrides follow exec (or
// exec resume) and precede no-mistakes' managed transport/result flags. Resume
// intentionally omits --output-schema and --color because TraeX 0.200.19's
// narrower resume parser rejects them.
func (a *traexAgent) buildArgs(schemaPath, resumeID string) []string {
	args := make([]string, 0, len(a.extraArgs)+13)
	args = append(args, "exec")
	if resumeID != "" {
		args = append(args, "resume")
	}
	args = append(args, a.extraArgs...)
	if resumeID != "" {
		args = append(args, resumeID)
	}
	args = append(args, "-", "--json")
	if resumeID == "" && schemaPath != "" {
		args = append(args, "--output-schema", schemaPath)
	}
	if !traexUserSetExecutionMode(a.extraArgs) {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	}
	if resumeID == "" {
		args = append(args, "--color", "never")
	}
	if a.disableProjectSettings {
		// These controls are mandatory, not defaults: repeated TraeX -c values
		// are last-one-wins, so placing =0 after all user args prevents an
		// agent_args_override from re-enabling AGENTS.md. --ignore-rules has no
		// inverse flag and is likewise appended after user arguments.
		args = append(args, "-c", "project_doc_max_bytes=0", "--ignore-rules")
	}
	return args
}

func traexArgsTerminateOptions(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return true
		}
	}
	return false
}

func traexUserSetExecutionMode(args []string) bool {
	for _, arg := range args {
		switch {
		case arg == "--dangerously-bypass-approvals-and-sandbox", arg == "-y",
			arg == "--permission-mode", arg == "--sandbox", arg == "-s":
			return true
		case strings.HasPrefix(arg, "--permission-mode="), strings.HasPrefix(arg, "--sandbox="):
			return true
		}
	}
	return false
}

func buildTraexResumePrompt(prompt string, schema json.RawMessage) string {
	pretty, err := json.MarshalIndent(json.RawMessage(schema), "", "  ")
	if err != nil {
		pretty = []byte(schema)
	}
	return prompt + "\n\n## no-mistakes final output contract\n\n" +
		"TraeX exec resume does not accept --output-schema. Your final assistant response for this turn must be only valid JSON matching this JSON Schema. " +
		"Do not wrap it in Markdown fences. Do not include prose before or after the JSON object.\n\n" + string(pretty)
}

type traexEvent struct {
	Type     string      `json:"type"`
	Item     *codexItem  `json:"item,omitempty"`
	Usage    *traexUsage `json:"usage,omitempty"`
	Message  string      `json:"message,omitempty"`
	Error    *traexError `json:"error,omitempty"`
	ThreadID string      `json:"thread_id,omitempty"`
}

type traexError struct {
	Message string `json:"message"`
}

type traexUsage struct {
	InputTokens         int `json:"input_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
	CachedInputTokens   int `json:"cached_input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	ReasoningTokens     int `json:"reasoning_output_tokens"`
}

func parseTraexEvents(ctx context.Context, r io.Reader, onChunk func(string), usage *TokenUsage, lastMessage, traexErr, threadID *string, turnFailed *bool, metrics *codexMetricsAccumulator) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024*1024)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var event traexEvent
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		switch event.Type {
		case "error":
			if event.Message != "" && traexErr != nil {
				*traexErr = event.Message
			}
		case "turn.failed":
			if turnFailed != nil {
				*turnFailed = true
			}
			if event.Error != nil && event.Error.Message != "" && traexErr != nil {
				*traexErr = event.Error.Message
			}
		case "thread.started":
			if event.ThreadID != "" && threadID != nil {
				*threadID = event.ThreadID
			}
		case "item.started":
			if metrics != nil {
				metrics.onItem(event.Type, event.Item, time.Now())
			}
		case "item.completed":
			if metrics != nil {
				metrics.onItem(event.Type, event.Item, time.Now())
			}
			if event.Item != nil && event.Item.Type == "agent_message" {
				if lastMessage != nil {
					*lastMessage = event.Item.Text
				}
				if onChunk != nil {
					onChunk(event.Item.Text)
				}
			}
		case "turn.completed":
			if event.Usage != nil && usage != nil {
				usage.Add(TokenUsage{
					InputTokens:           event.Usage.InputTokens,
					OutputTokens:          event.Usage.OutputTokens,
					CacheReadTokens:       event.Usage.CachedInputTokens,
					CacheCreationTokens:   event.Usage.CacheCreationTokens,
					ReasoningTokens:       event.Usage.ReasoningTokens,
					Reported:              true,
					CacheCreationReported: true,
				})
			}
		}
	}
	return scanner.Err()
}
