# Read-only workspace inspection

Host APIs:

- `GET /v1/runs/{runID}/workspace?operation=list|read|diff|git-status&path=...`
  inspects the workspace prepared for an existing run.
- `GET /v1/nodes/{nodeID}/workspace?operation=list|read|diff|git-status&path=...&workspace={json}`
  prepares and inspects the supplied workspace directly on the selected Node,
  without requiring a prior run. The `workspace` query value is a JSON-encoded
  `WorkspaceSpec`.

Nodes poll for inspection requests with their existing Node credentials; no
inbound Node listener or SSH access is required.

`list` and `read` resolve relative paths using `os.Root`. Paths outside the
prepared workspace, symlink escapes, non-regular files, binary content and text
files larger than 1 MiB are rejected. Directory listings are limited to 1,000
entries and hide `.git` and symlinks. `diff` returns the current tracked Git
changes relative to HEAD, limited to 2 MiB, with external diff helpers disabled.
It is not a per-run patch and may include changes made before the run.
`git-status` returns the current branch and short commit for Git workspaces, or
`repository: false` for ordinary directories. Detached HEAD workspaces return the
short commit with `detached: true`.

The Node persists its run-to-prepared-directory mapping under the configured
workspace root for run inspection. Direct Node inspection prepares the supplied
workspace for the duration of the request; local directory workspaces therefore
remain inspectable after a Node is re-enrolled with a new identity.

Deploy updated Relay Server and Relay Nodes together. Direct workspace
inspection requires protocol version 2.
The broker is transient and single-server: Server restart fails in-flight reads;
the user can retry. A multi-replica deployment requires a shared RPC broker or
sticky routing and is not supported by this initial transport.
