package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

func TestDoctorListsAndDiagnosesTraex(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	t.Setenv("NM_HOME", t.TempDir())
	binDir := t.TempDir()
	writeDoctorGitBinary(t, binDir)
	writeDoctorStubBinary(t, binDir, "traex")
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	line := doctorAgentLine(t, out, "traex")
	if !strings.Contains(line, binDir) {
		t.Fatalf("TraeX row did not report its resolved binary:\n%s", line)
	}
	if !strings.Contains(out, "traex is runnable") {
		t.Fatalf("configured auto agent should diagnose TraeX as runnable:\n%s", out)
	}
}
