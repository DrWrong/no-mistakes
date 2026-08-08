package intent

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func buildTraexFixture(t *testing.T, cwd string) string {
	t.Helper()
	home := t.TempDir()
	traexDir := filepath.Join(home, ".trae", "cli")
	if err := os.MkdirAll(traexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(traexDir, "sessions", "2026", "08", "08", "rollout-thread-traex.jsonl")
	if err := os.MkdirAll(filepath.Dir(rolloutPath), 0o755); err != nil {
		t.Fatal(err)
	}
	rollout := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"add native TraeX support in internal/agent/traex.go"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"implementing internal/agent/traex.go"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"sed\",\"-n\",\"1,20p\",\"internal/agent/traex.go\"]}"}}`,
	}, "\n")
	if err := os.WriteFile(rolloutPath, []byte(rollout), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(traexDir, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE threads (
		id TEXT PRIMARY KEY,
		cwd TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		rollout_path TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO threads (id, cwd, created_at, updated_at, rollout_path) VALUES (?, ?, ?, ?, ?)`,
		"thread-traex", cwd, now, now, rolloutPath); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestTraexReader_DiscoversNativeSessionAndLoadsCodexShapedRollout(t *testing.T) {
	repo := t.TempDir()
	home := buildTraexFixture(t, repo)
	r := NewTraexReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{
		HomeDir: home, OriginCWD: repo,
		WindowStart: time.Now().Add(-time.Hour), WindowEnd: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	if sessions[0].AgentName != TraexReaderName || sessions[0].SessionID != "thread-traex" {
		t.Fatalf("session = %+v", sessions[0])
	}
	if err := r.Load(context.Background(), sessions[0]); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(sessions[0].Messages) != 3 {
		t.Fatalf("messages = %+v", sessions[0].Messages)
	}
	if sessions[0].Messages[0].Role != RoleUser || !strings.Contains(sessions[0].Messages[0].Text, "TraeX") {
		t.Fatalf("user message = %+v", sessions[0].Messages[0])
	}
	if len(sessions[0].Messages[2].FilePaths) == 0 || sessions[0].Messages[2].Text != "" {
		t.Fatalf("tool message leaked text or lost paths: %+v", sessions[0].Messages[2])
	}
}
