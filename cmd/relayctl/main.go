// Command relayctl is a small terminal workbench for inspecting and driving a
// Relay control plane without depending on a host product.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
	"github.com/KDF5000/relay/sdk"
	"github.com/KDF5000/relay/transport/httpapi"
)

const defaultServerURL = "http://127.0.0.1:8787"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	global := flag.NewFlagSet("relayctl", flag.ContinueOnError)
	global.SetOutput(stderr)
	server := global.String("server", envOr("RELAY_SERVER_URL", defaultServerURL), "Relay control plane URL")
	showVersion := global.Bool("version", false, "print version and exit")
	if err := global.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Fprintln(stdout, relay.VersionLine("relayctl"))
		return nil
	}
	remaining := global.Args()
	if len(remaining) == 0 {
		printUsage(stdout)
		return nil
	}
	transport := httpapi.NewAuthenticatedClient(*server, os.Getenv("RELAY_HOST_TOKEN"))
	client := sdk.New(transport)
	switch remaining[0] {
	case "version":
		fmt.Fprintln(stdout, relay.VersionLine("relayctl"))
		return nil
	case "doctor":
		return doctor(ctx, transport, client, remaining[1:], stdout, stderr)
	case "runtime", "runtimes":
		if len(remaining) == 1 || remaining[1] == "list" {
			return runtimeList(ctx, client, remaining[min(2, len(remaining)):], stdout, stderr)
		}
	case "run":
		if len(remaining) < 2 {
			return errors.New("usage: relayctl run <submit|get|events|watch|cancel>")
		}
		switch remaining[1] {
		case "submit":
			return runSubmit(ctx, client, remaining[2:], stdout, stderr)
		case "list":
			values, err := client.ListRuns(ctx, 50)
			if err != nil {
				return err
			}
			return printJSON(stdout, values)
		case "get":
			return runGet(ctx, client, remaining[2:], stdout, stderr)
		case "events":
			return runEvents(ctx, client, remaining[2:], stdout, stderr)
		case "watch":
			return runWatchCommand(ctx, client, remaining[2:], stdout, stderr)
		case "cancel":
			return runCancel(ctx, client, remaining[2:], stdout, stderr)
		case "attempts":
			if len(remaining) != 3 {
				return errors.New("usage: relayctl run attempts <run-id>")
			}
			values, err := client.Attempts(ctx, remaining[2])
			if err != nil {
				return err
			}
			return printJSON(stdout, values)
		case "artifacts":
			if len(remaining) != 3 {
				return errors.New("usage: relayctl run artifacts <run-id>")
			}
			values, err := client.Artifacts(ctx, remaining[2])
			if err != nil {
				return err
			}
			return printJSON(stdout, values)
		case "interactions":
			if len(remaining) != 3 {
				return errors.New("usage: relayctl run interactions <run-id>")
			}
			values, err := client.Interactions(ctx, remaining[2])
			if err != nil {
				return err
			}
			return printJSON(stdout, values)
		}
	case "interaction":
		if len(remaining) != 4 || remaining[1] != "resolve" {
			return errors.New("usage: relayctl interaction resolve <interaction-id> <json-response>")
		}
		var response json.RawMessage = []byte(remaining[3])
		if !json.Valid(response) {
			encoded, _ := json.Marshal(remaining[3])
			response = encoded
		}
		value, err := client.ResolveInteraction(ctx, remaining[2], response)
		if err != nil {
			return err
		}
		return printJSON(stdout, value)
	case "session":
		if len(remaining) != 3 || remaining[1] != "runs" {
			return errors.New("usage: relayctl session runs <session-id>")
		}
		values, err := client.SessionRuns(ctx, remaining[2], 50)
		if err != nil {
			return err
		}
		return printJSON(stdout, values)
	case "artifact":
		if len(remaining) != 4 || remaining[1] != "download" {
			return errors.New("usage: relayctl artifact download <artifact-id> <output-file>")
		}
		reader, err := client.OpenArtifact(ctx, remaining[2])
		if err != nil {
			return err
		}
		defer reader.Close()
		file, err := os.OpenFile(remaining[3], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(file, reader)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	case "help", "--help", "-h":
		printUsage(stdout)
		return nil
	}
	return fmt.Errorf("unknown command %q", strings.Join(remaining, " "))
}

func doctor(ctx context.Context, transport *httpapi.Client, client *sdk.Client, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	execute := flags.Bool("execute", false, "run a real agent task and verify its artifact (may consume provider quota)")
	provider := flags.String("provider", "", "runtime provider; defaults to the first healthy runtime")
	runtimeID := flags.String("runtime-id", "", "bind the probe to an exact runtime instance")
	model := flags.String("model", "", "runtime model override")
	timeout := flags.Duration("timeout", 3*time.Minute, "maximum time for the execution probe")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("doctor accepts flags only")
	}
	if *timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	if err := transport.Health(ctx); err != nil {
		return fmt.Errorf("server health: %w", err)
	}
	fmt.Fprintln(stdout, "[ok] server health")
	info, err := transport.BuildInfo(ctx)
	if err != nil {
		return fmt.Errorf("server version: %w", err)
	}
	fmt.Fprintf(stdout, "[ok] server %s · protocol %s\n", info.Version, info.ProtocolVersion)
	if info.ProtocolVersion != relay.ProtocolVersion {
		return fmt.Errorf("protocol mismatch: relayctl=%s server=%s", relay.ProtocolVersion, info.ProtocolVersion)
	}
	nodes, err := client.Nodes(ctx)
	if err != nil {
		return fmt.Errorf("host authentication or node inventory: %w", err)
	}
	fmt.Fprintln(stdout, "[ok] host authentication")
	type candidate struct {
		node    controlplane.Node
		runtime controlplane.Runtime
	}
	var candidates []candidate
	for _, node := range nodes {
		if node.State != controlplane.NodeOnline || node.ProtocolVersion != relay.ProtocolVersion {
			continue
		}
		for _, runtime := range node.Runtimes {
			if runtime.State == "unhealthy" || (*provider != "" && runtime.Provider != *provider) || (*runtimeID != "" && runtime.ID != *runtimeID) {
				continue
			}
			candidates = append(candidates, candidate{node: node, runtime: runtime})
		}
	}
	if len(candidates) == 0 {
		return fmt.Errorf("no healthy runtime matches provider=%q runtime-id=%q", *provider, *runtimeID)
	}
	selected := candidates[0]
	if *provider == "" {
		*provider = selected.runtime.Provider
	}
	fmt.Fprintf(stdout, "[ok] runtime %s on %s · runtime %s · node %s\n", selected.runtime.Provider, selected.node.ID, emptyDash(selected.runtime.Version), emptyDash(selected.node.Version))
	modelCount := len(selected.runtime.ModelCatalog)
	if modelCount == 0 {
		modelCount = len(selected.runtime.Models)
	}
	if modelCount == 0 && selected.runtime.DefaultModel == "" {
		fmt.Fprintln(stdout, "[warn] runtime did not advertise a model catalog")
	} else {
		fmt.Fprintf(stdout, "[ok] model discovery · %d advertised\n", max(1, modelCount))
	}
	if !*execute {
		fmt.Fprintln(stdout, "[skip] execution probe; use --execute to test Run and Artifact delivery")
		return nil
	}
	fmt.Fprintln(stdout, "[run] execution probe may consume provider quota")
	probeCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	queued, err := client.Submit(probeCtx, relay.Request{
		AgentID:        "relayctl-doctor",
		IdempotencyKey: fmt.Sprintf("relayctl-doctor-%d", time.Now().UnixNano()),
		Runtime:        relay.RuntimeRequirement{ID: selected.runtime.ID, Provider: *provider, Model: *model},
		Source:         relay.Source{Kind: "relayctl.doctor"},
		Input:          relay.Input{Type: "task", Version: "1", Prompt: "Relay connectivity check. Do not modify files or call tools. Reply with exactly: RELAY_DOCTOR_OK"},
		Principal:      relay.Principal{Type: "system", ID: "relayctl-doctor"},
		Timeout:        (*timeout).String(),
	})
	if err != nil {
		return fmt.Errorf("submit execution probe: %w", err)
	}
	if err := watchRun(probeCtx, client, queued.ID, 500*time.Millisecond, stdout); err != nil {
		return fmt.Errorf("execution probe: %w", err)
	}
	artifacts, err := client.Artifacts(probeCtx, queued.ID)
	if err != nil {
		return fmt.Errorf("artifact probe: %w", err)
	}
	if len(artifacts) == 0 {
		return errors.New("artifact probe: run succeeded without an artifact")
	}
	fmt.Fprintf(stdout, "[ok] artifact delivery · %d artifact(s)\n", len(artifacts))
	return nil
}

func runtimeList(ctx context.Context, client *sdk.Client, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("runtime list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("runtime list accepts no arguments")
	}
	nodes, err := client.Nodes(ctx)
	if err != nil {
		return err
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	if *jsonOutput {
		return printJSON(stdout, nodes)
	}
	writer := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "NODE\tSTATE\tNODE VERSION\tPROTOCOL\tRUNTIME\tINSTANCE\tRUNTIME VERSION\tLOAD\tCAPABILITIES\tLAST SEEN")
	for _, node := range nodes {
		runtimes := append([]controlplane.Runtime(nil), node.Runtimes...)
		sort.Slice(runtimes, func(i, j int) bool { return runtimes[i].Provider < runtimes[j].Provider })
		capabilities := capabilityInventory(node.Capabilities)
		if len(runtimes) == 0 {
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t-\t-\t-\t%d/%d\t%s\t%s\n", node.ID, node.State, emptyDash(node.Version), emptyDash(node.ProtocolVersion), node.Active, node.Capacity, capabilities, relativeTime(node.LastSeen))
			continue
		}
		for index, runtime := range runtimes {
			nodeID, state, nodeVersion, protocol, load, caps, seen := "", "", "", "", "", "", ""
			if index == 0 {
				nodeID = node.ID
				state = node.State
				nodeVersion = emptyDash(node.Version)
				protocol = emptyDash(node.ProtocolVersion)
				load = fmt.Sprintf("%d/%d", node.Active, node.Capacity)
				caps = capabilities
				seen = relativeTime(node.LastSeen)
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", nodeID, state, nodeVersion, protocol, runtime.Provider, emptyDash(runtime.ID), emptyDash(runtime.Version), load, caps, seen)
		}
	}
	return writer.Flush()
}

func runSubmit(ctx context.Context, client *sdk.Client, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("run submit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	agent := flags.String("agent", "relayctl", "logical agent ID")
	provider := flags.String("provider", "codex", "runtime provider")
	runtimeID := flags.String("runtime-id", "", "bind to an exact runtime instance ID")
	model := flags.String("model", "", "runtime model override")
	runtimeVersion := flags.String("runtime-version", "", "runtime semantic version constraint")
	sessionID := flags.String("session", "", "optional session ID grouping related runs")
	prompt := flags.String("prompt", "", "task prompt")
	promptFile := flags.String("prompt-file", "", "read task prompt from file")
	idempotency := flags.String("idempotency", "", "stable submission idempotency key")
	sourceKind := flags.String("source-kind", "terminal", "source kind")
	sourceID := flags.String("source-id", "", "source external ID")
	principalType := flags.String("principal-type", "user", "principal type")
	principalID := flags.String("principal-id", "local", "principal ID")
	watch := flags.Bool("watch", true, "follow the run until it finishes")
	jsonOutput := flags.Bool("json", false, "print submitted run as JSON (requires --watch=false)")
	maxAttempts := flags.Int("max-attempts", 1, "maximum attempts after a running node is lost")
	retryBackoff := flags.String("retry-backoff", "0s", "delay before a recovered attempt can be claimed")
	timeout := flags.String("timeout", "", "maximum run duration, for example 30m")
	workspaceKind := flags.String("workspace-kind", "", "workspace provider: temp, local, or git")
	workspaceSource := flags.String("workspace-source", "", "absolute local path or Git URL/path")
	workspaceRef := flags.String("workspace-ref", "", "Git ref for a git workspace")
	workspaceSubdir := flags.String("workspace-subdir", "", "subdirectory inside the prepared workspace")
	workspaceEphemeral := flags.Bool("workspace-ephemeral", false, "remove a prepared temp workspace after completion")
	workspaceReuseKey := flags.String("workspace-reuse-key", "", "opaque identity used to reuse a prepared workspace")
	workspaceLifecycle := flags.String("workspace-lifecycle", "", "workspace lifecycle: attempt (default) or reusable")
	workspaceBranch := flags.String("workspace-branch", "", "branch created for a reusable Git workspace")
	var grants stringList
	var labels keyValues
	flags.Var(&grants, "grant", "capability grant name@version:effect:resource1,resource2 (repeatable)")
	flags.Var(&labels, "label", "required node label key=value (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("run submit accepts flags only")
	}
	if *prompt != "" && *promptFile != "" {
		return errors.New("use only one of --prompt and --prompt-file")
	}
	if *promptFile != "" {
		content, err := os.ReadFile(*promptFile)
		if err != nil {
			return err
		}
		*prompt = string(content)
	}
	if strings.TrimSpace(*prompt) == "" {
		return errors.New("--prompt or --prompt-file is required")
	}
	parsedGrants := make([]relay.CapabilityGrant, 0, len(grants))
	for _, value := range grants {
		grant, err := parseGrant(value)
		if err != nil {
			return err
		}
		parsedGrants = append(parsedGrants, grant)
	}
	if *idempotency == "" {
		*idempotency = fmt.Sprintf("relayctl-%d", time.Now().UnixNano())
	}
	queued, err := client.Submit(ctx, relay.Request{
		AgentID:        *agent,
		IdempotencyKey: *idempotency,
		Runtime:        relay.RuntimeRequirement{ID: *runtimeID, Provider: *provider, Model: *model, Version: *runtimeVersion, Labels: labels},
		SessionID:      *sessionID,
		Source:         relay.Source{Kind: *sourceKind, ExternalID: *sourceID},
		Input:          relay.Input{Type: "task", Version: "1", Prompt: *prompt},
		Capabilities:   parsedGrants,
		Principal:      relay.Principal{Type: *principalType, ID: *principalID},
		Retry:          relay.RetryPolicy{MaxAttempts: *maxAttempts, Backoff: *retryBackoff},
		Timeout:        *timeout,
		Workspace:      relay.WorkspaceSpec{Kind: *workspaceKind, Source: *workspaceSource, Ref: *workspaceRef, Subdir: *workspaceSubdir, Ephemeral: *workspaceEphemeral, ReuseKey: *workspaceReuseKey, Lifecycle: *workspaceLifecycle, Branch: *workspaceBranch},
	})
	if err != nil {
		return err
	}
	if *jsonOutput {
		if *watch {
			return errors.New("--json requires --watch=false")
		}
		return printJSON(stdout, queued)
	}
	fmt.Fprintf(stdout, "Submitted %s  status=%s  provider=%s\n", queued.ID, queued.Status, queued.Runtime.Provider)
	if !*watch {
		return nil
	}
	return watchRun(ctx, client, queued.ID, 500*time.Millisecond, stdout)
}

func runCancel(ctx context.Context, client *sdk.Client, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("run cancel", flag.ContinueOnError)
	flags.SetOutput(stderr)
	reason := flags.String("reason", "cancelled by user", "cancellation reason")
	requestedBy := flags.String("requested-by", "local", "actor requesting cancellation")
	jsonOutput := flags.Bool("json", false, "print JSON")
	runID := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		runID, args = args[0], args[1:]
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if runID == "" {
		if flags.NArg() != 1 {
			return errors.New("usage: relayctl run cancel <run-id> [--reason TEXT]")
		}
		runID = flags.Arg(0)
	} else if flags.NArg() != 0 {
		return errors.New("usage: relayctl run cancel <run-id> [--reason TEXT]")
	}
	run, err := client.CancelRun(ctx, runID, controlplane.CancelRequest{Reason: *reason, RequestedBy: *requestedBy})
	if err != nil {
		return err
	}
	if *jsonOutput {
		return printJSON(stdout, run)
	}
	printRun(stdout, run)
	return nil
}

func runGet(ctx context.Context, client *sdk.Client, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("run get", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: relayctl run get <run-id>")
	}
	run, err := client.Run(ctx, flags.Arg(0))
	if err != nil {
		return err
	}
	if *jsonOutput {
		return printJSON(stdout, run)
	}
	printRun(stdout, run)
	return nil
}

func runEvents(ctx context.Context, client *sdk.Client, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("run events", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: relayctl run events <run-id>")
	}
	events, err := client.Events(ctx, flags.Arg(0))
	if err != nil {
		return err
	}
	if *jsonOutput {
		return printJSON(stdout, events)
	}
	printEvents(stdout, events)
	return nil
}

func runWatchCommand(ctx context.Context, client *sdk.Client, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("run watch", flag.ContinueOnError)
	flags.SetOutput(stderr)
	interval := flags.Duration("interval", 500*time.Millisecond, "stream reconnect interval")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: relayctl run watch <run-id>")
	}
	if *interval < 100*time.Millisecond {
		return errors.New("--interval must be at least 100ms")
	}
	return watchRun(ctx, client, flags.Arg(0), *interval, stdout)
}

func watchRun(ctx context.Context, client *sdk.Client, runID string, interval time.Duration, stdout io.Writer) error {
	lastSequence := 0
	for {
		streamErr := client.StreamEvents(ctx, runID, lastSequence, func(event relay.Event) error {
			fmt.Fprintf(stdout, "%4d  %s  %-36s %s\n", event.Sequence, event.CreatedAt.Local().Format("15:04:05"), event.Type, compactJSON(event.Data))
			lastSequence = event.Sequence
			return nil
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		run, err := client.Run(ctx, runID)
		if err != nil {
			if streamErr != nil {
				return streamErr
			}
			return err
		}
		if terminalStatus(run.Status) {
			fmt.Fprintln(stdout)
			printRun(stdout, run)
			if run.Status == relay.RunFailed {
				return fmt.Errorf("run failed: %s", run.Error)
			}
			return nil
		}
		if streamErr != nil && errors.Is(streamErr, controlplane.ErrNotFound) {
			return streamErr
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func printRun(output io.Writer, run relay.Run) {
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintf(writer, "RUN\t%s\n", run.ID)
	fmt.Fprintf(writer, "STATUS\t%s\n", run.Status)
	fmt.Fprintf(writer, "RUNTIME\t%s\n", run.Runtime.Provider)
	fmt.Fprintf(writer, "AGENT\t%s\n", run.AgentID)
	fmt.Fprintf(writer, "NODE\t%s\n", emptyDash(run.Attempt.NodeID))
	if run.Error != "" {
		fmt.Fprintf(writer, "ERROR\t%s\n", run.Error)
	}
	if run.Result != nil && run.Result.Summary != "" {
		fmt.Fprintf(writer, "SUMMARY\t%s\n", strings.ReplaceAll(run.Result.Summary, "\n", " "))
	}
	_ = writer.Flush()
}

func printEvents(output io.Writer, events []relay.Event) {
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "SEQ\tTIME\tTYPE\tDATA")
	for _, event := range events {
		fmt.Fprintf(writer, "%d\t%s\t%s\t%s\n", event.Sequence, event.CreatedAt.Local().Format("15:04:05"), event.Type, compactJSON(event.Data))
	}
	_ = writer.Flush()
}

func parseGrant(value string) (relay.CapabilityGrant, error) {
	parts := strings.SplitN(value, ":", 3)
	identity := strings.SplitN(parts[0], "@", 2)
	if len(identity) != 2 || identity[0] == "" || identity[1] == "" {
		return relay.CapabilityGrant{}, fmt.Errorf("invalid grant %q: expected name@version:effect:resources", value)
	}
	grant := relay.CapabilityGrant{Name: identity[0], Version: identity[1], Effect: "read"}
	if len(parts) >= 2 && parts[1] != "" {
		grant.Effect = parts[1]
	}
	if len(parts) == 3 && parts[2] != "" {
		for _, resource := range strings.Split(parts[2], ",") {
			if resource = strings.TrimSpace(resource); resource != "" {
				grant.Resources = append(grant.Resources, resource)
			}
		}
	}
	return grant, nil
}

type stringList []string

func (v *stringList) String() string { return strings.Join(*v, ",") }
func (v *stringList) Set(value string) error {
	*v = append(*v, value)
	return nil
}

type keyValues map[string]string

func (v *keyValues) String() string {
	values := make([]string, 0, len(*v))
	for key, value := range *v {
		values = append(values, key+"="+value)
	}
	sort.Strings(values)
	return strings.Join(values, ",")
}
func (v *keyValues) Set(value string) error {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 || parts[0] == "" {
		return fmt.Errorf("invalid label %q: expected key=value", value)
	}
	if *v == nil {
		*v = make(map[string]string)
	}
	(*v)[parts[0]] = parts[1]
	return nil
}

func capabilityInventory(values []controlplane.Capability) string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.Name+"@"+value.Version)
	}
	sort.Strings(result)
	if len(result) == 0 {
		return "-"
	}
	return strings.Join(result, ", ")
}

func terminalStatus(status relay.RunStatus) bool {
	return status == relay.RunSucceeded || status == relay.RunFailed || status == relay.RunCancelled
}

func compactJSON(value json.RawMessage) string {
	if len(value) == 0 || string(value) == "null" {
		return ""
	}
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, value); err != nil {
		return string(value)
	}
	result := buffer.String()
	if len(result) > 160 {
		return result[:160] + "…"
	}
	return result
}

func printJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func relativeTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	age := time.Since(value)
	if age < time.Minute {
		return fmt.Sprintf("%ds ago", max(0, int(age.Seconds())))
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	}
	return value.Local().Format("2006-01-02 15:04")
}

func emptyDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func printUsage(output io.Writer) {
	fmt.Fprint(output, `Relay terminal workbench

Usage:
  relayctl --version
  relayctl [--server URL] doctor [--execute] [options]
  relayctl [--server URL] runtime list [--json]
  relayctl [--server URL] run submit --prompt TEXT [options]
  relayctl [--server URL] run list
  relayctl [--server URL] run get <run-id> [--json]
  relayctl [--server URL] run events <run-id> [--json]
  relayctl [--server URL] run watch <run-id>
  relayctl [--server URL] run cancel <run-id> [--reason TEXT]
  relayctl [--server URL] run list
  relayctl [--server URL] run attempts <run-id>
  relayctl [--server URL] run artifacts <run-id>
  relayctl [--server URL] artifact download <artifact-id> <output-file>
  relayctl [--server URL] run interactions <run-id>
  relayctl [--server URL] interaction resolve <interaction-id> <json-response>
  relayctl [--server URL] session runs <session-id>
  relayctl [--server URL] run attempts|artifacts|interactions <run-id>
  relayctl [--server URL] interaction resolve <interaction-id> <json-response>
  relayctl [--server URL] artifact download <artifact-id> <output-file>

Submit examples:
  relayctl run submit --provider codex --prompt "Inspect this workspace"
  relayctl run submit --prompt "Read MUL-42" --grant issue.read@1:read:MUL-42

Environment:
  RELAY_SERVER_URL  control plane URL (default http://127.0.0.1:8787)
  RELAY_HOST_TOKEN  optional host bearer token
`)
}
