package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

const (
	MaxInputImages     = 4
	MaxInputImageSize  = 5 << 20
	MaxInputImagesSize = 16 << 20
)

// InputImage is an image carried in Input.Data. Keeping binary input in the
// versioned Data envelope lets older Relay clients continue using Input.
type InputImage struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Data        []byte `json:"data"`
}

type inputImageEnvelope struct {
	Images []InputImage `json:"images,omitempty"`
}

// EncodeInputImages builds the versioned Input.Data payload accepted by Relay.
func EncodeInputImages(images ...InputImage) (json.RawMessage, error) {
	data, err := json.Marshal(inputImageEnvelope{Images: images})
	if err != nil {
		return nil, err
	}
	if _, err := InputImages(Input{Data: data}); err != nil {
		return nil, err
	}
	return data, nil
}

// InputImages decodes and validates image input from Input.Data.
func InputImages(input Input) ([]InputImage, error) {
	if len(input.Data) == 0 {
		return nil, nil
	}
	var envelope inputImageEnvelope
	if err := json.Unmarshal(input.Data, &envelope); err != nil {
		return nil, fmt.Errorf("relay: decode input images: %w", err)
	}
	if len(envelope.Images) > MaxInputImages {
		return nil, fmt.Errorf("relay: at most %d input images are allowed", MaxInputImages)
	}
	total := 0
	for index, image := range envelope.Images {
		if image.Name == "" {
			return nil, fmt.Errorf("relay: input image %d requires a name", index+1)
		}
		if imageExtension(image.ContentType) == "" {
			return nil, fmt.Errorf("relay: input image %q has unsupported content type %q", image.Name, image.ContentType)
		}
		if len(image.Data) == 0 {
			return nil, fmt.Errorf("relay: input image %q is empty", image.Name)
		}
		if len(image.Data) > MaxInputImageSize {
			return nil, fmt.Errorf("relay: input image %q exceeds %d bytes", image.Name, MaxInputImageSize)
		}
		if detected := http.DetectContentType(image.Data); detected != image.ContentType {
			return nil, fmt.Errorf("relay: input image %q content does not match %q", image.Name, image.ContentType)
		}
		total += len(image.Data)
		if total > MaxInputImagesSize {
			return nil, fmt.Errorf("relay: input images exceed %d bytes in total", MaxInputImagesSize)
		}
	}
	return envelope.Images, nil
}

// MaterializeInputImages writes validated images into a run-private directory.
// Callers must invoke the returned cleanup function after the runtime exits.
func MaterializeInputImages(workDir string, input Input) ([]string, func(), error) {
	images, err := InputImages(input)
	if err != nil || len(images) == 0 {
		return nil, func() {}, err
	}
	if workDir == "" {
		return nil, func() {}, errors.New("relay: work directory is required for image input")
	}
	relayDir := filepath.Join(workDir, ".relay")
	if err := os.MkdirAll(relayDir, 0o700); err != nil {
		return nil, func() {}, err
	}
	dir, err := os.MkdirTemp(relayDir, "input-images-")
	if err != nil {
		return nil, func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	paths := make([]string, 0, len(images))
	for index, image := range images {
		path := filepath.Join(dir, fmt.Sprintf("%02d%s", index+1, imageExtension(image.ContentType)))
		if err := os.WriteFile(path, image.Data, 0o600); err != nil {
			cleanup()
			return nil, func() {}, err
		}
		paths = append(paths, path)
	}
	return paths, cleanup, nil
}

func imageExtension(contentType string) string {
	switch contentType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ""
	}
}
