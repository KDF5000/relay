# Relay

[English](README.md) | 简体中文

Relay 是面向跨机器 AI Agent Runtime 的 SDK 和分布式执行基础组件。业务系统拥有 Agent、工作流和业务逻辑；Relay 提供 Runtime 发现、调度、执行、工作区准备、持久化事件和结果。仓库内的 Web Playground 是验证接入可行性的参考应用。

参见 [组件边界与执行契约](docs/component-contract.md)，了解当前保证和限制。

```text
业务系统 / Host
        │ Go SDK 或 HTTP API
        ▼
Relay Server ── PostgreSQL / Artifact Store
        │ Node Protocol
        ▼
Relay Node ── Codex / Trae / 自定义 Runtime
        │
        └── Capability Binding: CLI / HTTP / RPC / Go
```

## 核心能力

- 多机器 Node 注册、心跳、容量和 Runtime Inventory
- 严格的 Server/Node 协议握手，以及独立展示的产品版本
- 固定到指定 Runtime 实例，或根据 Provider 和 Capability 自动调度
- 原生 Codex、Trae Runtime 适配，支持流式输出和模型发现
- 持久化 Run、Attempt、Lease、重试、取消、超时和有序 SSE 事件
- 本地目录、临时目录、Git mirror 和 Git worktree Workspace Provider
- 本地卷或 S3 兼容对象存储 Artifact
- 通过进程、CLI、HTTP、RPC 或进程内方式接入业务 Capability
- Host/Node Token 分离，以及 Tenant/Project 隔离
- 内嵌 Web Agent Playground 参考客户端和 `relayctl` 终端客户端
- 基于 PostgreSQL 的多 Node 协调和 Server 重启恢复

## 快速开始

### 1. 启动 Relay Server

Docker Compose 会启动 Relay Server 和 PostgreSQL。Server 启动时会自动执行数据库迁移。

```bash
git clone https://github.com/KDF5000/relay.git
cd relay
cp .env.example .env
```

在 `.env` 中设置数据库密码和两个相互独立的 Token：

```dotenv
POSTGRES_PASSWORD=replace-with-a-long-random-password
RELAY_HOST_TOKEN=replace-with-a-long-random-host-token
RELAY_NODE_TOKEN=replace-with-a-long-random-node-token
```

启动并验证：

```bash
docker compose up -d --build --wait
curl http://127.0.0.1:8787/health
curl http://127.0.0.1:8787/version
```

预期响应：

```json
{"status":"ok"}
{"version":"dev","protocol_version":"1"}
```

打开 Web Playground：<http://127.0.0.1:8787/console/>，首次访问时输入 `RELAY_HOST_TOKEN`。

### 2. 接入 Node

在已经安装 Codex、`traex` 或 `trae-cli` 的机器上执行。Server 地址必须能从这台机器访问；远程 Server 不能使用 `127.0.0.1`。

```bash
curl -fsSL https://raw.githubusercontent.com/KDF5000/relay/main/install.sh \
  | RELAY_NODE_TOKEN='与Server相同的Node Token' \
    sh -s -- --server https://relay.example.com --install-service
```

安装脚本支持 macOS/Linux 的 AMD64 和 ARM64。它会校验 Release 文件、将 `relay-node`、`relay-tool` 和 `relayctl` 安装到 `~/.local/bin`，从 `PATH` 自动发现 Runtime CLI，并把 Node 配置写入 `~/.config/relay/node.json`。

`--install-service` 会安装并启动用户级系统服务：

- Linux：systemd user service
- macOS：LaunchAgent

接入成功后，Node 及其 Runtime 会显示在 Playground 的 **Runtimes** 页面。

### 3. 创建 Agent

打开 Playground 的 **Agents** 页面，选择 Runtime 和模型，按需设置工作区，然后开始对话。Agent 可以：

- 固定到某个 Node 上的一个具体 Runtime 实例；或
- 在所有兼容 Runtime 实例之间自动调度。

Playground 中的 Agent Profile、对话索引和聊天交互只是保存在浏览器中的示例业务状态，
不是 Relay Core 实体或持久化契约。生产业务应自行管理 Agent 定义、对话、权限和工作流，
再把执行 Request 提交给 Relay。

## 使用 Go SDK 嵌入

业务可以直接使用 HTTP Transport，也可以通过便捷 SDK Client 调用：

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
        Input:          relay.Input{Type: "task", Version: "1", Prompt: "审查变更 42"},
    })
    if err != nil {
        log.Fatal(err)
    }
    log.Printf("queued run %s", run.ID)
}
```

业务代码应优先依赖最小接口：`sdk.Submitter`、`sdk.Runs`、`sdk.Events`、
`sdk.Artifacts` 或 `sdk.Interactions`。`sdk.Backend` 只用于组合便捷 Client 的完整能力，
业务 Adapter 不需要依赖无关接口。

## 部署与运维

### Railway

Railway 是临时将 Relay Control Plane 暴露到公网最简单的方式。新账号可以使用 Railway 的试用额度；由于 Relay Node 会持续发送心跳，Server 和 PostgreSQL 会保持活跃，请留意额度消耗。

1. 创建一个空的 Railway Project。
2. 通过 **New → Database → PostgreSQL** 添加 PostgreSQL。
3. 通过 **New → GitHub Repo** 添加另一个 Service，选择 `KDF5000/relay`。Railway 会自动识别仓库根目录的 `Dockerfile`。
4. 在 Relay Service 中设置以下变量：

   ```dotenv
   RELAY_DATABASE_URL=${{Postgres.DATABASE_URL}}
   RELAY_HOST_TOKEN=replace-with-a-long-random-host-token
   RELAY_NODE_TOKEN=replace-with-a-different-long-random-node-token
   RELAY_TENANT_ID=default
   RELAY_PROJECT_ID=default
   RELAY_ARTIFACT_BACKEND=file
   RELAY_ARTIFACT_ROOT=/tmp/relay-artifacts
   ```

   Railway 会注入 `PORT`，Relay 将自动监听该端口。如果数据库 Service 不是 `Postgres`，需要相应修改引用变量中的名称。

5. 将 Health Check Path 设置为 `/health`，不要启用 Serverless/App Sleeping，然后在 **Settings → Networking** 中生成公网域名。
6. 验证服务并打开 Playground：

   ```bash
   curl https://<service>.up.railway.app/health
   ```

   ```text
   https://<service>.up.railway.app/console/
   ```

临时验证时，文件 Artifact 会写入临时存储，在重新部署或重启后丢失。需要持久化 Artifact 时，请改用 `RELAY_ARTIFACT_BACKEND=s3`；PostgreSQL 数据会独立持久化。

使用公网地址接入远程 Node：

```bash
curl -fsSL https://raw.githubusercontent.com/KDF5000/relay/main/install.sh \
  | RELAY_NODE_TOKEN='与Server相同的Node Token' \
    sh -s -- --server https://<service>.up.railway.app --install-service
```

### Server 管理

```bash
docker compose ps
docker compose logs -f relay-server
docker compose down
```

`docker compose down` 不会删除 PostgreSQL 和 Artifact 数据卷。更新源码构建的部署：

```bash
git pull
docker compose up -d --build --wait relay-server
```

默认情况下，Relay Server 对外映射 `8787`，PostgreSQL 只绑定到 `127.0.0.1:55432`。生产环境建议在 `8787` 前配置 TLS 反向代理，不要把 PostgreSQL 暴露到公网。

Artifact 默认使用持久化 Docker 卷。也可以设置 `RELAY_ARTIFACT_BACKEND=s3` 和 `RELAY_S3_*` 环境变量，切换到 S3 兼容对象存储。

### Node 服务管理

Node 默认会先把未确认事件持久化到磁盘再上报。重启后，它会在领取新任务前补传有效
Lease 下的事件，并把被中断的旧 Attempt 标记为失败；这不会恢复 Agent 进程。过期事件会
清理并记录原因，网络结果未知时则保留文件并阻止启动领取新任务。

默认目录是系统用户配置目录下的 `relay/outbox/<server-node-hash>`。Node JSON 可用
`outbox_root` 设置基目录（身份子目录仍会自动追加），用 `outbox_max_bytes` 设置容量
（默认 67108864，即 64 MiB）。容量耗尽会终止当前执行，避免磁盘无限增长。升级 Node 时
应保留此目录；其中包含 Lease 凭据和私有事件数据。

Linux：

```bash
systemctl --user status relay-node
systemctl --user restart relay-node
journalctl --user -u relay-node -f
sudo loginctl enable-linger "$USER"
```

启用 linger 后，退出登录不会停止用户服务，并且无需交互登录也能随系统启动。

macOS：

```bash
launchctl print gui/$(id -u)/dev.relay.node
tail -f ~/.cache/relay/logs/node.log
```

不传 `--install-service` 时，可以让 Node 在前台运行：

```bash
~/.local/bin/relay-node -config ~/.config/relay/node.json
```

常用安装参数包括 `--node-id`、`--capacity`、`--runtime auto|codex|trae|both`、`--version`、`--install-dir`、`--config` 和 `--force`。默认保留已有配置；传入 `--force` 时会先创建带时间戳的备份。

## 终端客户端

可以从源码构建 `relayctl`，也可以直接使用 `install.sh` 安装的二进制：

```bash
make build-relayctl
./bin/relayctl --version
./bin/relayctl doctor
./bin/relayctl runtime list
./bin/relayctl run submit --provider codex --prompt "检查当前代码仓库"
./bin/relayctl run submit --provider codex --runtime-id developer-node/codex --prompt "固定到指定 Runtime"
./bin/relayctl run submit --provider codex --model model-a --prompt "使用指定模型"
./bin/relayctl run submit --provider codex --max-attempts 2 --retry-backoff 2s --prompt "执行可恢复任务"
./bin/relayctl run watch <run-id>
./bin/relayctl run cancel <run-id> --reason "不再需要"
./bin/relayctl run list
./bin/relayctl run attempts <run-id>
./bin/relayctl run artifacts <run-id>
./bin/relayctl artifact download <artifact-id> ./result.bin
./bin/relayctl run interactions <run-id>
./bin/relayctl interaction resolve <interaction-id> '{"approved":true}'
```

通过 `RELAY_SERVER_URL` 或全局 `--server` 参数连接其他 Control Plane。`run submit` 默认持续接收有序 SSE 事件；使用 `--watch=false` 可以在提交后立即返回。

`relayctl doctor` 默认只读，会依次检查 Server 健康状态和版本、Host 鉴权、协议兼容性、
在线 Runtime Inventory 以及模型发现。需要使用真实 Runtime 验证完整 Run 和 Artifact 链路时，
必须显式启用执行探针：

```bash
relayctl doctor --execute --provider codex
relayctl doctor --execute --runtime-id developer-node/codex --timeout 5m
```

执行探针会调用选中的 AI Provider，可能产生用量。

## 核心概念

### Run 与 Lease

任务提交后成为 Run。兼容的 Node 会原子领取 Attempt 并获得可续期 Lease。Lease fencing 会阻止旧 Worker 完成已经重新分配的任务。Node 失联后，Reconciler 会把 Attempt 标记为 `lost`，并按照 Run 的重试策略处理。

取消和超时使用同一条持久化路径：Server 记录请求，Node 在续租时收到请求，终止 Runtime 进程并确认最终状态。

### Workspace

Agent 可以使用临时目录、已有本地目录、Git mirror 或隔离 worktree。固定到远程 Node 的 Runtime 会在该 Node 上解析本地工作区路径，而不是在 Server 上解析。

Workspace 是基础设施协议，不是项目管理模型。Relay 不理解 Conversation、Issue、业务系统中的
Repository 或 Review 流程。多个 Run 需要共享一个 Git worktree 时，调用方可以提供不透明的
`reuse_key` 并设置 `lifecycle=reusable`。Relay 只负责按 Git Source 隔离该键、串行使用同一个
worktree，以及执行 Workspace 生命周期操作；复用键的业务含义仍由调用方负责。`branch` 只是
Git Workspace Provider 的可选参数，在 Relay 内没有业务工作流语义。

### Capability

业务系统可以暴露领域操作，而不需要把领域语义加入 Relay Core。Runtime 调用 `relay-tool`，Relay 校验 Run Grant 和资源范围，再调用配置的 Binding。

支持的 Binding 方式包括：

- 业务方提供的 CLI 进程；
- HTTP 接口；
- RPC 适配器；
- 进程内 Go Provider。

`relay-tool` 依赖 Node 注入的短生命周期 Run 级环境变量。业务凭据只保留在对应 Binding 内，不直接暴露给 Agent Runtime。

### Event 与 Interaction

事件会先持久化并分配有序 Sequence，再发送给客户端。SSE 接口同时支持 `after={sequence}` 和标准 `Last-Event-ID` 请求头，因此断线重连不会丢失或重复事件。

只有收到 `run.succeeded`、`run.failed` 或 `run.cancelled` 终态事件，事件流才算完成。
连接提前结束时，Go HTTP Client 会返回流中断错误，Host 可以从最后处理的 Sequence 重连。
一条完整的 Assistant Message 本身不代表 Run 已经结束。

长时间运行的 Runtime 可以创建审批或输入 Interaction，暂停执行，并在 Host 处理后恢复。

## Runtime 说明

- Relay 分别管理产品版本和 Node 协议版本。`relay-server --version`、`relay-node --version`
  和 `relayctl --version` 会同时输出两者。协议版本不一致的 Node 注册会收到 HTTP
  `426 Upgrade Required`；产品版本仅用于诊断，不参与调度。
- Codex 和 Trae 使用 `"protocol": "app-server"` 产生增量 `assistant.message.delta` 事件并自动发现模型。
- App Server 只有收到 Runtime 的 `turn/completed` 才判定成功；协议提前结束时保留部分输出事件，但 Run 会失败。
- 旧的 `exec` 协议仍可兼容非交互式 CLI，但只能返回完整消息。
- Trae exec 模式不能使用 `permission_mode=default`，因为无头进程无法请求审批。可以省略该配置使用 headless 默认值，或使用 `bypass_permissions`/兼容无头执行的 `custom` 策略。
- Runtime 子进程默认只继承受限环境。额外变量需要在 Node 配置中通过 `pass_env` 或 `env` 显式声明。

## 开发与验证

开发环境需要 Go 1.26，以及支持 Compose 的 Docker。

```bash
make verify
make demo
make codex-smoke
make trae-smoke
make codex-capability-smoke
```

`codex-smoke`、`trae-smoke` 和 `codex-capability-smoke` 会调用本机已经登录的 AI Runtime，可能产生 Provider 用量。

使用 PostgreSQL 从源码启动 Server：

```bash
make db-up
export RELAY_DATABASE_URL='postgres://relay:relay@127.0.0.1:55432/relay?sslmode=disable'
go run ./cmd/relay-server -listen 127.0.0.1:8787
make postgres-test
```

`-memory` 仅用于测试和临时 Demo；生产部署需要 PostgreSQL。

启用鉴权时必须同时配置 `RELAY_HOST_TOKEN` 和 `RELAY_NODE_TOKEN`。`relayctl` 读取 `RELAY_HOST_TOKEN`；Node 从配置文件或 `RELAY_NODE_TOKEN` 读取 Token。

Web Agent Playground 内嵌在 Server 二进制中。修改 `transport/httpapi/console/` 后需要重新构建或重启 `relay-server`，让 Go 重新嵌入静态资源。Run、Event、Result 和 Artifact 由 Relay API 持久化。

推送 `v*` Tag 会运行 [Release workflow](.github/workflows/release.yml)，验证项目并发布 Linux/macOS、AMD64/ARM64 的带校验和压缩包。

## 文档

- [组件边界与执行契约](docs/component-contract.md)
- [架构与设计](docs/design.md)
- [Node 配置示例](examples/relay-node.example.json)
- [Releases](https://github.com/KDF5000/relay/releases)

Host 产品可以通过 Binding 暴露自己的 Capability，而 Relay Core 不理解具体业务语义。
下一款用于产品级验证的应用会与 Relay Core 分开设计。
