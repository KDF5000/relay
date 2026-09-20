# Relay

English | [简体中文](README.zh-CN.md)

Relay is an SDK and distributed execution component for AI agent runtimes across machines. Applications own their agents, workflows, and business logic; Relay provides runtime discovery, scheduling, execution, workspace preparation, persisted events, and results. The included Web Playground is a reference application for validating integrations.

```text
Application / Host
        │ Go SDK or HTTP API
        ▼
Relay Server ── PostgreSQL / Artifact Store
        │ Node Protocol
        ▼
Relay Node ── Codex / Trae / Custom Runtime
        │
        └── Capability Binding: CLI / HTTP / RPC / Go
```

## Highlights

- Multi-machine Node registration, heartbeats, capacity, and runtime inventory
- Strict Server/Node protocol handshake with independently reported product versions
- Fixed runtime-instance assignment or automatic scheduling by provider and capability
- Native Codex and Trae runtime adapters with streaming output and model discovery
- Durable runs, attempts, leases, retries, cancellation, timeouts, and ordered SSE events
- Local, temporary, Git mirror, and Git worktree workspace providers
- Artifact storage on local volumes or S3-compatible object storage
- Application-defined capabilities through process, CLI, HTTP, RPC, or in-process bindings
- Host/Node token separation plus tenant and project isolation
- Embedded Web Agent Playground reference client and the `relayctl` terminal client
- PostgreSQL-backed coordination for multiple Nodes and Server restarts

## Quick start

### 1. Start Relay Server

Docker Compose starts Relay Server and PostgreSQL. Schema migrations run automatically when the Server starts.

```bash
git clone https://github.com/KDF5000/relay.git
cd relay
cp .env.example .env
```

Set a database password and two independent tokens in `.env`:

```dotenv
POSTGRES_PASSWORD=replace-with-a-long-random-password
RELAY_HOST_TOKEN=replace-with-a-long-random-host-token
RELAY_NODE_TOKEN=replace-with-a-long-random-node-token
```

Start the stack and verify it:

```bash
docker compose up -d --build --wait
curl http://127.0.0.1:8787/health
curl http://127.0.0.1:8787/version
```

The expected response is:

```json
{"status":"ok"}
{"version":"dev","protocol_version":"1"}
```

Open the Web Playground at <http://127.0.0.1:8787/console/> and enter `RELAY_HOST_TOKEN` when prompted.

### 2. Connect a Node

Run this on a machine that already has Codex, `traex`, or `trae-cli` installed. Replace the Server URL with an address reachable from that machine; do not use `127.0.0.1` for a remote Server.

```bash
curl -fsSL https://raw.githubusercontent.com/KDF5000/relay/main/install.sh \
  | RELAY_NODE_TOKEN='the-same-node-token-as-the-server' \
    sh -s -- --server https://relay.example.com --install-service
```

The installer supports macOS and Linux on AMD64 and ARM64. It verifies the release checksum, installs `relay-node`, `relay-tool`, and `relayctl` under `~/.local/bin`, discovers supported runtime CLIs from `PATH`, and writes the Node configuration to `~/.config/relay/node.json`. Discovered Codex and Trae runtimes default to non-ephemeral execution so Relay can preserve their native conversation threads across Runs.

`--install-service` installs and starts a user-level system service:

- Linux: systemd user service
- macOS: LaunchAgent

Once connected, the Node and its runtimes appear in the Playground's **Runtimes** view.

### 3. Create an Agent

Open **Agents** in the Web Playground, select a runtime and model, configure a workspace if needed, and start a conversation. An Agent can either:

- bind to one exact runtime instance on one Node; or
- use automatic scheduling across compatible runtime instances.

The Playground's Agent profiles, conversation index, and chat UX are demonstration-level application state stored in the browser. They are not Relay Core entities or a persistence contract. A production Host should own its Agent definitions, conversations, permissions, and workflow state, and submit execution Requests to Relay.

## Embed with the Go SDK

Applications can use the HTTP transport directly or wrap it with the convenience SDK client:

```go
package main

import (
    "context"
    "log"

    "github.com/KDF5000/relay"
    "github.com/KDF5000/relay/sdk"
    "github.com/KDF5000/relay/transport/httpapi"
)

func main() {
    ctx := context.Background()
    client := sdk.New(httpapi.NewAuthenticatedClient(
        "https://relay.example.com",
        "host-token",
    ))

    run, err := client.Submit(ctx, relay.Request{
        AgentID:        "code-reviewer",
        IdempotencyKey: "review-42",
        Runtime:        relay.RuntimeRequirement{Provider: "codex"},
        Input:          relay.Input{Type: "task", Version: "1", Prompt: "Review change 42"},
    })
    if err != nil {
        log.Fatal(err)
    }
    log.Printf("queued run %s", run.ID)
}
```

Prefer the smallest interface needed by application code: `sdk.Submitter`, `sdk.Runs`, `sdk.Events`, `sdk.Artifacts`, or `sdk.Interactions`. `sdk.Backend` composes the complete surface for the convenience client. This keeps business adapters independent from unrelated Relay features.

### Send images to a Runtime

Hosts can attach up to four PNG, JPEG, WebP, or GIF images to a text input. Relay validates the declared media type and size, transfers the bytes with the persisted Request, materializes them in a run-private directory on the selected Node, and passes them through the Runtime's native image-input protocol.

```go
imageData, err := relay.EncodeInputImages(relay.InputImage{
    Name:        "screenshot.png",
    ContentType: "image/png",
    Data:        screenshotBytes,
})
if err != nil {
    log.Fatal(err)
}

request.Input = relay.Input{
    Type:    "text",
    Version: "1",
    Prompt:  "Explain the error shown in this screenshot.",
    Data:    imageData,
}
```

Each image is limited to 5 MiB and one Request to 16 MiB of image data. Runtime-private files are deleted after execution; Hosts that need conversation history should persist their own attachment metadata and content.

## Deployment

### Railway

Railway is the simplest way to put a temporary Relay control plane on the public internet. New accounts can use Railway's trial credits; keep an eye on usage because Relay Node heartbeats keep the Server and PostgreSQL active.

1. Create an empty Railway project.
2. Add a PostgreSQL database with **New → Database → PostgreSQL**.
3. Add another service with **New → GitHub Repo** and select `KDF5000/relay`. Railway detects the root `Dockerfile` automatically.
4. Add these variables to the Relay service:

   ```dotenv
   RELAY_DATABASE_URL=${{Postgres.DATABASE_URL}}
   RELAY_HOST_TOKEN=replace-with-a-long-random-host-token
   RELAY_NODE_TOKEN=replace-with-a-different-long-random-node-token
   RELAY_TENANT_ID=default
   RELAY_PROJECT_ID=default
   RELAY_ARTIFACT_BACKEND=file
   RELAY_ARTIFACT_ROOT=/tmp/relay-artifacts
   ```

   Railway injects `PORT`; Relay listens on it automatically. If the database service has a different name, replace `Postgres` in the reference variable.

5. Set the health check path to `/health`, leave Serverless/App Sleeping disabled, and generate a public domain under **Settings → Networking**.
6. Verify the deployment and open the Playground:

   ```bash
   curl https://<service>.up.railway.app/health
   ```

   ```text
   https://<service>.up.railway.app/console/
   ```

For a temporary validation deployment, file artifacts are written to ephemeral storage and disappear after a redeploy or restart. Use `RELAY_ARTIFACT_BACKEND=s3` for durable artifacts. PostgreSQL remains persistent independently.

Connect remote Nodes with the public URL:

```bash
curl -fsSL https://raw.githubusercontent.com/KDF5000/relay/main/install.sh \
  | RELAY_NODE_TOKEN='the-same-node-token-as-the-server' \
    sh -s -- --server https://<service>.up.railway.app --install-service
```

### Server operations

```bash
docker compose ps
docker compose logs -f relay-server
docker compose down
```

`docker compose down` preserves PostgreSQL and Artifact volumes. To update a source-built deployment:

```bash
git pull
docker compose up -d --build --wait relay-server
```

By default, Relay Server is published on port `8787`, while PostgreSQL is bound only to `127.0.0.1:55432`. In production, place a TLS reverse proxy in front of port `8787` and avoid exposing PostgreSQL publicly.

Artifacts use a persistent Docker volume by default. Relay Server can also use S3-compatible storage through `RELAY_ARTIFACT_BACKEND=s3` and the `RELAY_S3_*` environment variables.

### Node service operations

Nodes persist unacknowledged events before sending. On restart, they replay events
under valid leases and mark interrupted attempts as failed; this does not resume
the agent process. Stale events are discarded with a log entry. Unknown network
outcomes retain the files and prevent startup from claiming new work.

The default spool is under the OS user configuration directory, in
`relay/outbox/<server-node-hash>`. Optional Node JSON settings `outbox_root`
(base directory; the identity subdirectory is always added) and `outbox_max_bytes`
(default 67108864) control its location and capacity. Capacity exhaustion fails
the execution rather than growing without bound. Preserve this directory across
Node updates; it contains lease credentials and private event data.

Linux:

```bash
systemctl --user status relay-node
systemctl --user restart relay-node
journalctl --user -u relay-node -f
sudo loginctl enable-linger "$USER"
```

Enabling linger keeps the user service running after logout and starts it without an interactive login.

macOS:

```bash
launchctl print gui/$(id -u)/dev.relay.node
tail -f ~/.cache/relay/logs/node.log
```

Run the installer without `--install-service` to keep the Node in the foreground:

```bash
~/.local/bin/relay-node -config ~/.config/relay/node.json
```

Useful installer options include `--node-id`, `--capacity`, `--runtime auto|codex|trae|both`, `--version`, `--install-dir`, `--config`, and `--force`. Existing configuration is preserved unless `--force` is set, in which case the installer creates a timestamped backup.

## Terminal client

Build `relayctl` from source, or use the binary installed by `install.sh`:

```bash
make build-relayctl
./bin/relayctl --version
./bin/relayctl doctor
./bin/relayctl runtime list
./bin/relayctl run submit --provider codex --prompt "Inspect this repository"
./bin/relayctl run submit --provider codex --runtime-id developer-node/codex --prompt "Run on this exact runtime"
./bin/relayctl run submit --provider codex --model model-a --prompt "Use a specific model"
./bin/relayctl run submit --provider codex --max-attempts 2 --retry-backoff 2s --prompt "Retry recoverable work"
./bin/relayctl run watch <run-id>
./bin/relayctl run cancel <run-id> --reason "No longer needed"
./bin/relayctl run list
./bin/relayctl run attempts <run-id>
./bin/relayctl run artifacts <run-id>
./bin/relayctl artifact download <artifact-id> ./result.bin
./bin/relayctl run interactions <run-id>
./bin/relayctl interaction resolve <interaction-id> '{"approved":true}'
```

Set `RELAY_SERVER_URL` or pass the global `--server` option to connect to another control plane. `run submit` watches ordered SSE events by default; use `--watch=false` to return after submission.

`relayctl doctor` is read-only by default. It checks Server health and version, Host authentication,
protocol compatibility, online Runtime inventory, and model discovery. To validate the complete Run
and Artifact path with a real Runtime, opt in explicitly:

```bash
relayctl doctor --execute --provider codex
relayctl doctor --execute --runtime-id developer-node/codex --timeout 5m
```

The execution probe invokes the selected AI provider and may consume quota.

## Core concepts

### Runs and leases

A submitted task becomes a Run. A compatible Node atomically claims an Attempt and receives a renewable lease. Lease fencing prevents stale workers from completing reassigned work. If a Node disappears, the reconciler marks the Attempt as lost and applies the Run's retry policy.

Cancellation and timeouts use the same durable path: the Server records the request, the Node receives it during lease renewal, terminates the runtime process, and acknowledges the final state.

### Workspaces

Agents may use temporary directories, existing local directories, Git mirrors, or isolated worktrees. A runtime fixed to a remote Node resolves local workspace paths on that Node, not on the Server.

Workspace is an infrastructure contract, not a project-management model. Relay
does not know about conversations, issues, repositories owned by a product, or
review workflows. A caller may attach an opaque `reuse_key` and set
`lifecycle=reusable` for a Git workspace when multiple Runs must share one
prepared worktree. Relay scopes that key to the Git source, serializes users of
the worktree, and leaves the caller responsible for assigning meaning and
eventually requesting lifecycle cleanup. `branch` is an optional Git-provider
hint; it has no workflow semantics inside Relay.

### Capabilities

Applications expose domain operations without adding domain semantics to Relay Core. A runtime calls `relay-tool`, Relay validates the Run grant and resource scope, and then invokes the configured binding.

Supported binding styles include:

- application-provided CLI processes;
- HTTP endpoints;
- RPC adapters; and
- in-process Go providers.

`relay-tool` relies on short-lived Run-scoped environment values injected by the Node. Business credentials remain inside the selected binding and are not exposed directly to the agent runtime.

### Events and interactions

Events are persisted before delivery and receive an ordered sequence number. The SSE endpoint supports both `after={sequence}` and the standard `Last-Event-ID` header, allowing clients to reconnect without losing or duplicating events.

A stream is complete only after a terminal `run.succeeded`, `run.failed`, or `run.cancelled` event. The Go HTTP client reports an interrupted stream when the connection ends earlier, so Hosts can reconnect from the last processed sequence. A complete assistant message alone is not proof that a Run completed.

Long-running runtimes can create approval or input interactions, pause, and resume after a Host resolves them.

## Runtime notes

- Relay has separate product and Node protocol versions. `relay-server --version`, `relay-node --version`,
  and `relayctl --version` print both. A Node registration with a different protocol version is rejected
  with HTTP `426 Upgrade Required`; product-version differences are diagnostic and do not affect scheduling.
- Codex and Trae use `"protocol": "app-server"` for incremental `assistant.message.delta` events and runtime model discovery.
- Non-ephemeral Codex and Trae runs persist the provider-native thread for the same tenant, project, session, Agent, Runtime, and Node. Later Runs send only the new turn through native resume, preserving the Runtime's own context compaction. If the thread cannot be resumed or execution moves to another Node, Relay starts a replacement thread with the Host-provided recovery prompt.
- App-server execution succeeds only after the runtime reports `turn/completed`; partial output is retained as events when the protocol ends early, but the Run fails.
- The legacy `exec` protocol remains available for compatible non-interactive CLIs but only produces complete messages.
- Relay does not override runtime permission policy. Legacy Node `sandbox` and `permission_mode` fields are ignored; configure permissions in Codex or Trae itself.
- Runtime subprocesses receive a restricted environment by default. Add variables explicitly through `pass_env` or `env` in the Node configuration.
- Codex-like runtimes can declare durable output files by writing `.relay/artifacts.json` in the work directory before they finish. Each entry contains a relative `path`, a semantic `type`, and optional `name` and `content_type`, for example `{"artifacts":[{"path":"report.md","type":"report","content_type":"text/markdown"}]}`. Relay validates that declared files stay inside the workspace and uploads them separately from the runtime's final message.

## Development

Requirements: Go 1.26 and Docker with Compose.

```bash
make verify
make demo
make codex-smoke
make trae-smoke
make codex-capability-smoke
```

`codex-smoke`, `trae-smoke`, and `codex-capability-smoke` invoke locally authenticated AI runtimes and may consume provider usage.

Run the Server from source with PostgreSQL:

```bash
make db-up
export RELAY_DATABASE_URL='postgres://relay:relay@127.0.0.1:55432/relay?sslmode=disable'
go run ./cmd/relay-server -listen 127.0.0.1:8787
make postgres-test
```

Use `-memory` only for tests and temporary demos. Production deployments require PostgreSQL.

When authentication is enabled, both `RELAY_HOST_TOKEN` and `RELAY_NODE_TOKEN` must be configured. `relayctl` reads `RELAY_HOST_TOKEN`; a Node reads the token from its configuration or `RELAY_NODE_TOKEN`.

The Web Agent Playground is embedded into the Server binary. After changing files under `transport/httpapi/console/`, rebuild or restart `relay-server` so Go can embed the updated assets. Runs, events, results, and artifacts are persisted by the Relay API.

Pushing a `v*` tag runs the [release workflow](.github/workflows/release.yml), verifies the project, and publishes checksum-protected archives for Linux and macOS on AMD64 and ARM64.

## Documentation

- [Component boundaries and execution guarantees (Chinese)](docs/component-contract.md)
- [Architecture and design](docs/design.md)
- [Example Node configuration](examples/relay-node.example.json)
- [Releases](https://github.com/KDF5000/relay/releases)

Host products can expose their own capabilities through bindings while Relay Core remains unaware of
their domain semantics. The next product-level validation will be designed separately from Relay Core.
