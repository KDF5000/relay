# Relay 分布式 Agent 执行组件设计

状态：最小可发布版本，核心可靠性契约已建立，2026 年 9 月。

当前组件边界和可靠性限制以 [组件契约](component-contract.md) 为准。本文保留初版架构
设计，不应把能力清单视为所有真实故障场景已经通过生产验证。

## 1. 产品定位

Relay 是一个可被不同业务项目复用的分布式 Agent 执行基础组件，提供 SDK 与可独立部署的
Server / Node。Web Playground 只是验证接口的参考应用，不定义核心 Agent 或聊天模型。

业务项目只需要关注：

- 什么任务需要执行；
- 任务需要哪些业务上下文；
- Agent 可以调用哪些业务能力；
- 如何消费执行事件和最终结果。

Relay 负责：

- 管理执行机器；
- 管理 Codex、Trae 和自定义 Runtime；
- 调度、并发、Lease、取消和重试；
- 工作目录和指令生成；
- Agent 进程监管；
- Capability 授权、路由和审计；
- Run、Attempt、Event 与 Artifact。

Relay 不是 Todo 系统，也不理解 Issue、Chat、Inbox 或 Project 的业务含义。

## 2. 总体架构

```text
Multica / CI / IDE / 客服系统
  └── Relay Host SDK
        │
        │ Submit / Cancel / Events / Result
        ▼
Relay Control Plane
  ├── Run / Attempt 状态机
  ├── Node Registry 与心跳
  ├── Runtime / Capability Inventory
  ├── 调度、容量与 Lease
  ├── Event / Artifact 元数据
  └── Host API
        │
        │ Node Protocol
        ▼
Relay Node（每台执行机器）
  ├── Runtime 探测与进程监管
  ├── Workspace / Worktree 管理
  ├── Instruction 编译与生成
  ├── 本地 Tool Bridge
  └── Capability Binding Registry
        ├── Exec：用户安装的 CLI
        ├── HTTP：业务 API
        ├── RPC：gRPC / Connect / 自定义 RPC Client
        ├── Go：进程内实现
        └── MCP：未来可选适配器
```

## 3. 组件职责

### 3.0 Node 分发

Relay Node 通过 GitHub Release 提供 darwin/linux、amd64/arm64 的静态二进制包。仓库根目录
的 `install.sh` 负责平台识别、下载、SHA-256 校验、Runtime CLI 自动发现和 Node 配置生成。
Server 地址属于部署配置，通过 `--server` 或 `RELAY_SERVER_URL` 注入，不编译进 Node。
安装器默认只写用户目录，并在覆盖已有配置前创建备份。

### 3.1 Host SDK

Host SDK 是业务项目主要使用的接口。它隐藏底层 HTTP 或 RPC，使业务代码保持简单：

```go
client := sdk.New(transport)

run, err := client.Submit(ctx, relay.Request{
    AgentID:        "engineering-agent",
    IdempotencyKey: "issue-42-run-1",
    Runtime: relay.RuntimeRequirement{
        Provider: "codex",
        Labels:   map[string]string{"pool": "engineering"},
    },
    Input: relay.Input{Prompt: "处理这个 Issue"},
})
```

SDK 已提供提交、结果、流式订阅、取消、节点、产物和交互接口；业务应按需要依赖窄接口，
不必实现完整 Backend，也不必采用 Playground 的数据模型。

### 3.2 Control Plane

Control Plane 是整个机器集群的事实来源，负责：

- Node 注册、身份认证、心跳和离线检测；
- Node Label、容量、Runtime 与 Capability Inventory；
- 幂等创建 Run；
- 创建和分配 Attempt；
- 支持固定 Runtime 实例，或根据 Provider、Label 和 Capability 自动调度；
- 发放有时效的 Lease；
- 接收开始、事件、完成和失败上报；
- 保存 Artifact 元数据；
- 处理 Lease 过期与重新调度。

Control Plane 不启动 Agent 子进程，也不保存宿主业务凭证。

Server 与 Node 使用独立于产品发布版本的协议版本。Node 注册必须携带协议版本，Server
仅接受完全一致的版本并对不兼容注册返回 HTTP 426，避免不兼容 Node 领取任务。产品版本
只用于 `/version`、CLI、Runtime Inventory 和 Playground 中的诊断展示，不参与调度。

### 3.3 Relay Node

每台执行机器运行一个 Relay Node。Node 负责：

- 上报本机 Runtime、版本、Capability Binding 和容量；
- 领取与本机能力匹配的任务；
- 编译指令并准备执行环境；
- 启动、观察和终止 Agent Runtime；
- 为 Agent 创建当前 Run 专属的 Tool Bridge；
- 校验并路由 Capability 调用；
- 回传事件、结果和 Artifact。

Node 不理解 `multica.Issue` 等业务模型，只处理通用 Request。

### 3.4 Execution Kernel

仓库根包保留一个可嵌入执行内核，用于：

- 指令分层和编译；
- Capability Grant 与资源范围检查；
- Capability 并发幂等；
- 通用 Executor 契约；
- 本地运行和测试。

分布式模式下，Node 使用相同的类型和内核能力，Control Plane 则持有最终 Run 状态。

## 4. 调度模型

Node 注册时上报：

```json
{
  "id": "node-macos-01",
  "version": "v0.3.0",
  "protocol_version": "1",
  "labels": {
    "os": "darwin",
    "pool": "engineering"
  },
  "runtimes": [
    {"id": "node-macos-01/codex", "provider": "codex", "version": "1.2.0", "state": "healthy"}
  ],
  "capabilities": [
    {"name": "issue.read", "version": "1", "kind": "exec"}
  ],
  "capacity": 4
}
```

任务声明所需 Runtime、Label 和 Capability：

```json
{
  "runtime": {
    "provider": "codex",
    "version": "^1.2.0",
    "labels": {"pool": "engineering"}
  },
  "capabilities": [
    {"name": "issue.read", "version": "1"}
  ]
}
```

调度器只有在以下条件全部满足时才允许 Node Claim：

1. Node 支持指定 Runtime Provider；
2. Node Label 满足任务约束；
3. Node 拥有任务要求的全部 Capability Binding；
4. Node 当前 Active 数小于 Capacity；

Runtime 实例具有稳定 ID。Node 配置没有显式填写时，Relay 使用
`<node-id>/<provider>` 自动生成。任务只声明 `provider` 时采用动态调度，任意兼容实例都可
领取；同时声明 `id` 时则固定到该实例。固定实例暂时离线或满载时任务保持排队，不做隐式
故障转移，从而保证 Agent 与机器本地环境、凭据和工作区之间的绑定不会被破坏。
5. Node 处于健康状态。

Node 使用主动 Claim。未来可以替换为更复杂的调度器，但不能改变 Request、
Assignment 和 Lease 的基础语义。

## 5. Run、Attempt 与 Lease

`Run` 是一次业务执行的稳定身份，`Attempt` 是它的一次具体执行。

```text
Run queued
  └── Attempt queued
        ├── leased(node-a)
        ├── running
        └── succeeded / failed / lost
```

取消使用两阶段状态：Control Plane 先把 Run 标记为 `cancelling`，Node 在续租响应中收到
取消指令并终止 Runtime，随后确认 `attempt.cancelled` 和 `run.cancelled`。如果 Node 已经
失联，Reconciler 会在 Lease 过期后完成取消。Request 的 `timeout` 会生成持久化 Deadline，
到期后进入完全相同的取消流程。

分开建模可以支持：

- Node 离线后的重新执行；
- Runtime 故障切换；
- 超时和自动重试；
- 每次执行独立的日志和 Artifact；
- 完整保留历史，而不是覆盖状态。

Node 对 Attempt 的所有修改都必须携带 Lease Token。Node 在 Runtime 执行期间独立续租，
不依赖领取任务的主循环，并持续续租到产物上传及最终状态确认。完成与失败上报按内容
幂等，未知网络结果可安全重试。未开始的 Lease 过期后可以重新领取同一 Attempt；运行中 Lease
过期后，Reconciler 将旧 Attempt 标记为 `lost`，再根据 `max_attempts` 和 `backoff` 创建
新 Attempt。Run 指向当前 Attempt，历史 Attempt 与 Event 保留在 PostgreSQL 中。旧 Node
恢复后携带的 Lease Token 无法修改当前 Attempt，这就是 fencing 边界。

Run Event 通过 SSE 对外订阅。客户端使用 Event Sequence 作为游标，并通过 `after` 或
`Last-Event-ID` 恢复连接。Control Plane 只查询 `sequence > after` 的增量记录；Event 在
发送前已经提交到 PostgreSQL，因此跨进程重启和跨 Control Plane 实例都能断点续传。
SSE 在终态关闭前会再次读取尾部事件；客户端未读到 Run 终态时不能把 EOF 当作成功。

Node 每个 Capacity Slot 有独立的 Claim Loop。退出时先停止领取并等待在途 Runtime drain；
超时或第二个退出信号会取消执行。Unix 上 Runtime、Binding 与 Git 子进程均运行在独立
进程组，取消时先 TERM 整个进程组，再升级为 KILL，避免遗留孤儿进程。

## 6. Capability 与 Binding 分离

Capability 表示 Agent 被允许做什么：

```text
issue.read@v1
issue.comment.create@v1
repository.search@v2
```

Binding 表示能力如何执行：

```text
issue.read@v1
  ├── Exec Binding: multica-cli
  ├── HTTP Binding: Multica API
  ├── RPC Binding: Multica CapabilityService
  └── Go Binding: in-process handler
```

Grant 与 Binding 必须分开：

- Definition：能力的名称、版本、Effect 和 Schema；
- Grant：当前 Run 可以调用哪些能力和资源；
- Binding：当前 Node 使用什么实现完成调用。

Agent 只依赖稳定的 Capability 名称，不需要知道背后是 CLI 还是 HTTP。

每次调用在执行 Binding 前向 Control Plane 申请 `(run_id, idempotency_key)` reservation。
PostgreSQL 保存请求摘要、状态、结果和错误。新 Attempt 重放已完成调用时直接返回持久化
结果；相同幂等键对应不同请求时拒绝；`pending` 结果未知时也不会盲目重复副作用。

## 7. 用户自定义 CLI

用户可以在 Agent 所在机器安装自己的 CLI，并注册为 Exec Binding：

```json
{
  "name": "issue.read",
  "version": "1",
  "kind": "exec",
  "command": "multica",
  "args": ["capability", "invoke"],
  "inherit_env": true
}
```

Relay 直接启动二进制，不经过 shell。协议规定：

- stdin：`CapabilityRequest` JSON；
- stdout：`{"output": ...}`；
- stderr：日志；
- exit code 0：成功；
- 非 0：失败；
- SIGTERM：取消；
- 调用必须是非交互式的。

建议业务 CLI 提供：

```bash
multica capability list --format json
multica capability invoke --protocol relay-v1
```

首版由用户或机器管理员安装 CLI。Relay 只检测、执行并上报版本，不自动下载任意二进制，
避免过早引入供应链签名、升级回滚和多租户安装权限问题。

## 8. HTTP、RPC 与 MCP

HTTP Binding 向配置的 Endpoint 发送统一 `CapabilityRequest`，服务返回：

```json
{
  "output": {
    "id": "MUL-42",
    "title": "Example"
  }
}
```

HTTP Token 通过 Node 环境变量引用，不写入任务或 Prompt。

RPC Binding 暴露一个小型 `RPCClient` 接口，生成的 gRPC、Connect 或企业内部 RPC
Client 都可以适配。Relay Core 不绑定特定 RPC 框架。

MCP 可以在未来作为另一种 Binding，但它与 CLI、HTTP、RPC 平级，不是 Control Plane
和 Node 的核心协议。

## 9. Agent Tool Bridge

不能把 `multica-cli` 等完整工具直接暴露给 Agent，否则 Agent 可能绕过 Grant、资源范围、
幂等和审计。

Runtime 启动时，Relay Node 会建立短生命周期 Tool Bridge。Bridge 传输由 Runtime Adapter
选择，不属于 Capability 协议本身：

- Codex 默认使用工作目录内、权限为 `0700` 的文件邮箱 IPC；这是因为 Codex sandbox
  可以禁止包括 loopback 在内的网络访问；
- 不受该限制的通用 Command Runtime 可以使用只监听本机回环地址的 HTTP；
- 两种方式都只向 Agent 暴露同一个 `relay-tool` 命令。

Node 向 Runtime 子进程注入其中一组地址和一个当前 Run 专属的 Token：

```text
RELAY_TOOL_DIR 或 RELAY_TOOL_URL
RELAY_TOOL_TOKEN
```

Agent 使用统一命令：

```bash
relay-tool call \
  --version 1 \
  --resource MUL-42 \
  --idempotency read-primary-issue \
  issue.read
```

调用链为：

```text
Agent
  → relay-tool
  → Node 本地 Tool Bridge
  → Capability Grant / Scope / Idempotency 检查
  → Binding Registry
  → multica-cli / HTTP API / RPC
```

Tool Bridge Token 只对当前 Runtime 子进程和当前 Run 有效，执行结束后监听器或文件邮箱
立即关闭。文件请求和响应使用随机 ID 与原子 rename 发布，Node 在调用 Binding 前仍会
执行 Grant、资源范围与幂等校验。

Runtime 进程默认不继承 Node 的完整环境。Codex Adapter 只透传 PATH、用户目录、区域、
代理、证书以及 Codex/OpenAI 认证所需变量；额外变量必须通过 Runtime 的 `pass_env` 或
`env` 显式配置。Binding 拥有独立环境，因此 Multica Token 等业务凭据不会进入 Agent
进程。

Multica 作为首个宿主实现 `multica capability invoke --protocol relay-v1`。它从 stdin
读取标准 `CapabilityRequest`，把 `issue.read@1` 映射到 Multica API，再向 stdout 返回
标准结果。这个适配命令属于 Multica，不属于 Relay，因此 Relay Core 不包含 issue 或
workspace 语义。

## 10. Codex-like Runtime

### 10.1 Codex

Relay 已实现真实 Codex Runtime Adapter，使用 OpenAI 官方稳定的 `codex exec` 非交互
模式。当前适配器支持：

- 从 stdin 传入 Prompt；
- 使用 `--json` 接收 JSONL 事件并转为 Relay Event；
- 使用 `--output-last-message` 获取最终回答；
- 提取 Codex Thread ID；
- 不覆盖 Runtime 自身的 sandbox 或 approval 配置；
- Model、Profile、Reasoning Effort 和 Service Tier 配置；
- `--ephemeral` 执行；
- 生成 `AGENTS.md`；
- 向 Codex 进程注入当前 Run 的 Tool Bridge；
- Context 取消后终止 Codex 进程；

实现遵循 [OpenAI Codex Developer Commands](https://developers.openai.com/codex/cli/reference)
中 `codex exec` 的非交互、JSONL 和最终消息文件契约。

当前也已支持 `app-server` 协议的增量输出、模型发现和原生 Session Resume。非 ephemeral
执行成功后，Control Plane 会按 tenant、project、session、Agent、Runtime 和 Node 记录
Runtime Thread ID；同一会话的下一轮只提交当前消息，由 Runtime 保留并压缩自己的上下文。
Thread 丢失、无法恢复或任务迁移到其他 Node 时，Node 使用 Host 随请求提供的有界恢复
Prompt 新建线程，成功后用新的 Thread ID 原子替换旧映射。通用 Interaction 接口存在并不
意味着每个 Runtime 已接通原生审批。

Codex-like Runtime 可以在结束前写入 `.relay/artifacts.json`，声明需要独立上传的业务交付
文件。每项包含相对工作目录的 `path`、语义化 `type`，以及可选的 `name` 和
`content_type`。Node 会在每次执行前清理旧清单，执行后校验文件存在、为普通文件且解析
后的真实路径仍位于工作目录内，再将它们作为独立 Artifact 上传。`.relay/` 内部文件不可
被声明为业务交付物；Runtime 最终消息仍是 Run Summary，不自动等价于 Artifact。

### 10.2 TraeCode

TraeCode CLI fork 自 Codex，并保留 `exec`、stdin Prompt、JSONL Event、最终消息文件、
model/profile 和 ephemeral 等核心契约。Relay 复用 Codex-like 执行内核，但对外
保持独立边界：

- Runtime Provider 使用 `trae`；
- 优先发现 `traex`，找不到时回退 `trae-cli`；
- 版本输出统一提取为语义版本，例如 `0.202.3`；
- Event 使用 `runtime.trae.*`，最终回答 Artifact 使用 `trae_final_message`；
- 只额外透传显式允许的 `TRAE_HOME`、`TRAE_API_KEY`、`TRAE_BASE_URL`；
- 权限策略由 Trae 自身配置，Relay 不再附加 `permission_mode`；
- 继续使用 Relay 的进程组监管、Workspace、文件 Tool Bridge 和 Artifact 上传链路。

Node 配置的 `kind` 可使用 `trae`、`traex` 或 `trae-cli`，建议稳定的业务 Provider 固定为
`trae`，不要让二进制别名泄漏到任务模型。

## 11. 指令模型

指令按作用域分为：

1. `runtime`：Relay 执行不变量；
2. `host`：业务系统全局语义；
3. `workspace`：仓库或项目规则；
4. `agent`：Agent 长期角色；
5. `turn`：当前任务临时要求。

前四层编译为 Stable Instructions，可生成 `AGENTS.md`、`CLAUDE.md`、`QWEN.md`
或 `CODEBUDDY.md`。Relay 只管理带标记的区块，保留用户已有内容。Turn 指令只进入
Prompt，不污染持久化项目文件。

## 12. 安全边界

当前实现提供：

- Host Token 与 Node Token 两类 Bearer 身份，协议端点相互隔离；
- Host Token 绑定 Tenant 与 Project，所有 Run 派生资源按作用域校验；
- Lease TTL、续租和 fencing token；
- 每个 Run 的最小权限 Capability Grant；
- HTTP Token、CLI 环境变量和 Runtime 环境变量隔离；
- Command 不经过 shell，固定参数使用数组；
- stdout/stderr 大小限制；
- Workspace 和子进程隔离；
- 写 Capability 在宿主端再次验证 Principal；
- Node 二进制和 Runtime 版本上报；
- Artifact 访问控制、大小限制、SHA-256 与外部 Blob Store。

内置静态 Token 适合单实例部署；嵌入宿主时可向 HTTP Handler 注入 `Authenticator` 对接
Multica 身份系统或企业 IAM。生产环境仍应在 Relay 前配置 TLS/mTLS、Token 轮换和
Artifact 内容扫描。

Relay 负责执行授权边界，但 Multica 仍然是 Issue 等业务对象的最终授权方。

## 13. 仓库结构

```text
relay/
├── engine.go                 嵌入式 Execution Kernel
├── capability.go             Grant、Scope 和幂等
├── instructions.go           指令编译与文件生成
├── binding/
│   ├── registry.go           Binding Registry
│   ├── exec.go               用户 CLI
│   ├── http.go               HTTP API
│   └── rpc.go                RPC Client 接口
├── controlplane/             Node、调度、Run、Attempt、Lease、Blob Store
│   └── postgres/             PostgreSQL Store 与迁移
├── node/                     每机 Worker
├── workspace/                temp、local、Git mirror/worktree
├── adapter/multica/          Multica 源码级 Host/Capability 适配
├── runtime/command/          通用 Runtime CLI 与 Tool Bridge
├── runtime/codex/            真实 Codex CLI Adapter
├── runtime/trae/             真实 TraeCode CLI Adapter
├── sdk/                      业务项目 SDK
├── transport/httpapi/        当前 Host/Node 网络协议
├── cmd/relay-server/         Control Plane 进程
├── cmd/relay-node/           Node 进程
├── cmd/relay-tool/           Agent 统一工具入口
└── examples/multica/         两节点端到端示例
```

## 14. Multica 接入方式

Relay 提供 `adapter/multica` 源码级适配器。Multica 在业务服务内调用 `DispatchIssue`，
适配器负责把 Issue、Agent、Session 和 Principal 映射成通用 Request；这条 Host 路径不需要
MCP，也不需要为业务语义增加 Connector。Node 侧用 `CapabilityBinding` 调用已安装的
`multica capability invoke --protocol relay-v1`，业务凭据仍留在 Multica CLI。

切流时保留 Multica 当前 daemon 作为回退路径，按 Workspace 或任务类型选择 Relay；完成
行为对比后，再删除 Multica 中已经被 Relay 覆盖的调度、Workspace 和进程监管代码。

## 15. 当前 MVP 验收范围

- Host SDK 可以提交、列出、取消 Run，并读取 Attempt、Event、Interaction 和 Artifact；
- 多个 Node 按 Runtime Provider、语义版本、健康状态、Label、Capability 和 Capacity 调度；
- Lease 续租、Deadline、取消、fencing、重试和跨 Node 宕机恢复均持久化；
- Node 支持并发执行、优雅 drain 和 Runtime 进程树回收；
- Workspace 支持 temp、local 和 Git mirror/worktree，并在上传 Artifact 后清理；
- Capability 支持 Exec、HTTP、RPC、Go Binding，以及数据库幂等 reservation 和审计；
- Artifact 正文进入本地或 S3 兼容 Blob Store，元数据进入 PostgreSQL；
- Host/Node 身份不能越过协议边界，Tenant/Project 不能交叉读取；
- Runtime 可以发起 Input/Approval Interaction，由 Host 回应后恢复；
- 真实 Codex 可以通过 Control Plane、Node 和文件 Tool Bridge 完成任务；
- PostgreSQL + HTTP E2E 覆盖两个 Node 之间的失联恢复和 Capability 结果重放。

## 16. 按真实场景再引入的能力

- 其他 Runtime Adapter；
- Codex App Server 原生 Session Resume（当前已有通用 Session ID 与 Interaction）；
- Capability JSON Schema 校验；
- CLI 包签名、安全安装和自动升级；
- MCP Binding；
- Playground 已实现，作为参考应用维护；不扩展为核心业务控制台。

这些能力不阻塞当前多机器 Runtime 执行闭环，也不改变 Control Plane、Node、SDK、
Workspace、Blob Store 和 Binding 的边界。
