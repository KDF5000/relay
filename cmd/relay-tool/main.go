package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/runtime/antigravity"
	"github.com/KDF5000/relay/runtime/toolbridge"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "antigravity-hook" {
		flags := flag.NewFlagSet("antigravity-hook", flag.ExitOnError)
		url := flags.String("url", os.Getenv("RELAY_AGY_HOOK_URL"), "loopback lifecycle endpoint")
		token := flags.String("token", os.Getenv("RELAY_AGY_HOOK_TOKEN"), "lifecycle connection token")
		_ = flags.Parse(os.Args[2:])
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := antigravity.ForwardLifecycleHook(ctx, os.Stdin, *url, *token); err != nil {
			log.Fatal(err)
		}
		fmt.Println("{}")
		return
	}
	if len(os.Args) < 2 || os.Args[1] != "call" {
		log.Fatal("usage: relay-tool call [flags] <capability>")
	}
	flags := flag.NewFlagSet("call", flag.ExitOnError)
	version := flags.String("version", "1", "capability version")
	resource := flags.String("resource", "", "resource ID")
	idempotency := flags.String("idempotency", "", "idempotency key")
	input := flags.String("input", "{}", "JSON input")
	_ = flags.Parse(os.Args[2:])
	if flags.NArg() != 1 {
		log.Fatal("one capability name is required")
	}
	if *idempotency == "" {
		log.Fatal("--idempotency is required")
	}
	call := relay.CapabilityCall{Name: flags.Arg(0), Version: *version, Resource: *resource, IdempotencyKey: *idempotency, Input: json.RawMessage(*input)}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if bridgeDir := os.Getenv("RELAY_TOOL_DIR"); bridgeDir != "" {
		result, err := toolbridge.CallFile(ctx, bridgeDir, os.Getenv("RELAY_TOOL_TOKEN"), call)
		if err != nil {
			log.Fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			log.Fatal(err)
		}
		return
	}
	body, err := json.Marshal(call)
	if err != nil {
		log.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, os.Getenv("RELAY_TOOL_URL")+"/v1/call", bytes.NewReader(body))
	if err != nil {
		log.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+os.Getenv("RELAY_TOOL_TOKEN"))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		log.Fatal(err)
	}
	defer response.Body.Close()
	output, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		log.Fatal(err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		log.Fatalf("tool call failed: %s", output)
	}
	fmt.Print(string(output))
}
