package relay_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KDF5000/relay"
)

func imageInput(t *testing.T, images ...relay.InputImage) relay.Input {
	t.Helper()
	data, err := json.Marshal(map[string]any{"images": images})
	if err != nil {
		t.Fatal(err)
	}
	return relay.Input{Type: "text", Version: "1", Prompt: "inspect image", Data: data}
}

func TestMaterializeInputImagesUsesPrivateTemporaryFiles(t *testing.T) {
	workDir := t.TempDir()
	png := []byte("\x89PNG\r\n\x1a\n")
	input := imageInput(t, relay.InputImage{Name: "../../screen.png", ContentType: "image/png", Data: png})
	paths, cleanup, err := relay.MaterializeInputImages(workDir, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || !strings.HasPrefix(paths[0], filepath.Join(workDir, ".relay", "input-images-")) {
		t.Fatalf("unexpected paths: %v", paths)
	}
	content, err := os.ReadFile(paths[0])
	if err != nil || string(content) != string(png) {
		t.Fatalf("materialized content=%q err=%v", content, err)
	}
	cleanup()
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatalf("temporary image was not removed: %v", err)
	}
}

func TestInputImagesRejectsUnsupportedAndOversizedInput(t *testing.T) {
	_, err := relay.InputImages(imageInput(t, relay.InputImage{Name: "note.txt", ContentType: "text/plain", Data: []byte("no")}))
	if err == nil || !strings.Contains(err.Error(), "unsupported content type") {
		t.Fatalf("expected unsupported type error, got %v", err)
	}
	_, err = relay.InputImages(imageInput(t, relay.InputImage{Name: "huge.png", ContentType: "image/png", Data: make([]byte, relay.MaxInputImageSize+1)}))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size error, got %v", err)
	}
}
