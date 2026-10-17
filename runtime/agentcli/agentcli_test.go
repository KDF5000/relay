package agentcli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/KDF5000/relay"
)

func TestArtifactsOnlyIncludeCurrentDeclaredFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".relay"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, ".relay", "artifacts.json")
	// A reused workspace must not re-upload a previous turn's artifacts.
	if err := os.WriteFile(manifest, []byte(`{"artifacts":[{"path":"stale.txt"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run, err := Prepare(relay.Execution{RunID: "test", WorkDir: dir, Instructions: relay.CompiledInstructions{Stable: "rules"}, Capabilities: relay.NewCapabilityInvoker(relay.CapabilityInvokerOptions{})}, Config{}, "pi")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	artifacts, err := run.Artifacts("Plain chat response")
	if err != nil || len(artifacts) != 0 {
		t.Fatalf("plain chat artifacts=%+v err=%v", artifacts, err)
	}
	// Internal files remain available locally, but are not cloud artifacts.
	for _, path := range []string{run.Instruction, run.FinalPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("# Report"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(`{"artifacts":[{"path":"report.md","type":"report"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts, err = run.Artifacts("Created the requested report")
	if err != nil || len(artifacts) != 1 || artifacts[0].Name != "report.md" || artifacts[0].Size != 8 {
		t.Fatalf("report artifacts=%+v err=%v", artifacts, err)
	}
}
