# Relay 组件边界与执行契约

Relay 是可嵌入、也可通过 Server / Node 独立部署的分布式 Agent 执行基础组件。
SDK 是业务接入入口；Web Playground 是参考应用，不是核心产品模型或控制台契约。

## 职责

核心提供 Runtime 发现、执行、节点匹配、容量、Lease、取消、超时、事件、结果和产物。
工作区提供准备和清理机制；Capability 提供调用接口、Grant 检查和绑定机制。

Agent 管理、任务拆解、多 Agent 协作、业务工作流、聊天历史、长期记忆、Issue 和项目
属于宿主。宿主决定工作区选取、共享和保留策略，并对业务对象进行最终授权。
Runtime sandbox 与工作目录分离不等于针对不可信租户的操作系统级安全隔离。

`AgentID` 是宿主提供的关联标识，当前提交校验仍要求它存在，不代表核心拥有 Agent
实体。`SessionID` 只关联 Run，不保证 Runtime 线程或工作区延续。`Source` 是业务来源
元数据。后续是否移除字段或必填约束，应通过独立接入场景验证，不先引入新的实体。

## 接入方式

只提交任务的业务接受 `sdk.Submitter`；需要结果与取消时接受 `sdk.Runs`；事件、产物、
交互分别使用 `sdk.Events`、`sdk.Artifacts`、`sdk.Interactions`。HTTP Client 和完整 SDK
Client 均实现这些接口。业务可以直接依赖窄接口，不必包装完整 Backend。

`sdk.New` 仍接收完整 `Backend`，作为全功能便捷客户端；这不是业务适配器必须实现的接口。
自定义 Runtime 实现 `relay.Executor`，尊重 context 取消，在返回前停止并等待其所有
事件生产者。原生恢复、模型发现和输入请求属于可选能力，不要求每种 Runtime 支持。

## 版本与协议握手

Relay 把产品版本和 Node 协议版本分开管理。产品版本用于部署诊断和界面展示，不参与
调度；协议版本描述 Server 与 Node 的注册、领取、续租、事件和完成交互契约。

`relay-node` 注册时必须提交当前协议版本。Server 只接受与自身完全一致的版本，不做
隐式降级，版本不一致返回 HTTP `426 Upgrade Required`，使问题在领取任务之前暴露。
Node 每次启动和心跳注册都会刷新自身产品版本与协议版本。Server 的 `/version` 端点及
三个二进制的 `--version` 输出可用于诊断，Web Playground 与 `relayctl runtime list` 也会
展示版本信息。

`relayctl doctor` 默认执行只读检查；只有显式传入 `--execute` 时才提交真实 Run 并验证
Artifact 链路。执行探针可能消耗 Runtime Provider 用量。

## 当前可靠性保证与限制

- Run / Attempt 的最终状态以控制平面为准，界面收到一条完整消息并不代表执行结束。
- App Server Runtime 只有收到明确的 `turn/completed` 才能成功；协议流提前结束时保留已
  提交事件并将 Run 标记为失败。Node 的 Lease 续租持续到产物上传和最终状态得到确认。
- 完成与失败上报是内容幂等的：相同 Attempt 重复提交相同结果返回成功且不重复产生
  终态事件，提交不同内容则冲突。Node 对未知网络结果进行有界重试。
- 数据库保存后的事件按 Run 序号读取，客户端可通过 SSE 游标恢复。
- SSE 服务端在终态关闭前补发最后一批事件；Go 客户端只有读到 `run.succeeded`、
  `run.failed` 或 `run.cancelled` 才正常返回，提前 EOF 会报告连接中断。
- Node 同步、串行上报事件，每个事件携带 Attempt / Lease 范围的稳定身份。服务端在
  状态变更事务内去重，同一身份不同内容会被拒绝。最多保留一个 1 MiB 的待确认事件，
  上报失败在 5 秒期限内退避重试，并对 Runtime 施加背压，续租独立运行。重传失败会
  取消 Runtime，并阻止成功完成；错误上报
  失败也返回给调用方。若控制平面不可达，最终状态可能需要等待 Lease 过期后的协调。
- relay-node 默认启用磁盘 outbox。事件经文件同步、原子发布和目录同步后才上报，
  确认后删除。默认总容量 64 MiB，单事件 1 MiB。超限的 Runtime 原始事件会被替换为
  包含 `truncated`、`original_bytes` 和有界 `preview` 的诊断事件，不会中断 Runtime；完整最终输出仍通过
  Run Result 和 Runtime 声明的 Artifact 传递。
  文件权限 0600、目录 0700，目录有进程锁；仅保存事件与 Lease，不保存业务 Request。
- 启动时先补传遗留事件，再领取新任务。服务端原子校验 Lease；有效时去重补传并将旧
  Attempt 标记为 Node 重启中断；失效或已终结时清理并记录原因。网络结果未知时保留
  文件并退出启动流程，服务管理器可重启重试。不会恢复 Runtime 进程或重新执行业务任务。
- 磁盘损坏、事件尚未落盘以及 Lease 失效后的遗留数据仍可能无法恢复；不能承诺所有
  故障下事件绝不丢失。嵌入 Worker 时需显式设置 Outbox，并在启动任务前调用 Recover。
- Lease fencing 阻止过期 Attempt 修改控制平面，不能撤销已经执行的外部副作用。
- Run 重试是重新执行，不是进程恢复；Capability 的幂等保护只覆盖通过该接口的调用，
  不覆盖 Runtime 自行执行的 shell、网络请求或文件修改。
- 取消是协作式执行契约。适配器必须终止其子进程；忽略 context 的第三方适配器无法
  由 Go 接口强制中断。

## 已完成的稳定化验证

1. 已实现 Attempt 范围的事件身份、服务端内容校验和单事件有界缓冲重传。
2. 已实现磁盘 outbox、Node 重启补传与过期 Lease 清理，并用强制杀进程测试验证。
3. 已覆盖“提交成功但响应丢失”、Node 重启、断网恢复、旧 Lease 重放等故障测试。
4. 已覆盖 Runtime 部分输出后断流、长时间产物上传、终态响应丢失和 SSE 尾部事件。
5. 已覆盖 Node 协议版本拒绝、Server 版本发现和 `relayctl doctor` 基础诊断流程。
6. 下一阶段由独立产品验证公共接口；原生会话恢复等能力只在明确场景需要时加入。

不在这一轮增加核心 Agent CRUD、聊天存储、长期记忆、工作流引擎或 Web 管理功能。
