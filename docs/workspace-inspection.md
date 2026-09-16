# Read-only workspace inspection

Host API: `GET /v1/runs/{runID}/workspace?operation=list|read|diff&path=...`.
The Server authorizes access to the run and routes the request to its latest
attempt's Node. Nodes poll for inspection requests with their existing Node
credentials; no inbound Node listener or SSH access is required.

`list` and `read` resolve relative paths using `os.Root`. Paths outside the
prepared workspace, symlink escapes, non-regular files, binary content and text
files larger than 1 MiB are rejected. Directory listings are limited to 1,000
entries and hide `.git` and symlinks. `diff` returns the current tracked Git
changes relative to HEAD, limited to 2 MiB, with external diff helpers disabled.
It is not a per-run patch and may include changes made before the run.

The Node persists its run-to-prepared-directory mapping under the configured
workspace root. Deleted or ephemeral workspaces cannot be inspected. Runs made
before this feature was deployed need another execution to register their
workspace. Steer forwards this API only after checking its own workspace's run
link.

Deploy updated Relay Server and Relay Nodes together. The extension is additive;
older Nodes do not process inspection requests, which time out after 15 seconds.
The broker is transient and single-server: Server restart fails in-flight reads;
the user can retry. A multi-replica deployment requires a shared RPC broker or
sticky routing and is not supported by this initial transport.
