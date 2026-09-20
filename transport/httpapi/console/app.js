const STORAGE = {
  agents: "relay.playground.agents.v1",
  sessions: "relay.playground.sessions.v1",
  messages: "relay.playground.messages.v1",
  selection: "relay.playground.selection.v1",
};

const state = {
  agents: readStorage(STORAGE.agents, []),
  sessions: readStorage(STORAGE.sessions, []),
  messages: readStorage(STORAGE.messages, {}),
  selection: readStorage(STORAGE.selection, {}),
  nodes: [],
  buildInfo: null,
  nodesLoaded: false,
  nodesError: "",
  refreshingNodes: false,
  currentRun: null,
  events: [],
  busy: false,
  cancelRequested: false,
  editingAgentID: null,
  currentEventSource: null,
  lastActivity: "",
  editingSessionID: null,
  restoredRun: false,
  agentPage: 1,
};

const $ = (selector) => document.querySelector(selector);
const enhancedSelects = new WeakMap();
let openSelectControl = null;
const renderedMessageIDs = new Set();
const pendingMessageUpdates = new Map();
const terminalMessageStatuses = new Set(["succeeded", "failed", "cancelled"]);
let messageUpdateFrame = 0;
let fallbackIDCounter = 0;
const elements = {
  sidebar: $("#sidebar"),
  sidebarBackdrop: $("#sidebar-backdrop"),
  sessionList: $("#session-list"),
  messages: $("#messages"),
  emptyState: $("#empty-state"),
  chatScroll: $("#chat-scroll"),
  chatEndAnchor: $("#chat-end-anchor"),
  prompt: $("#prompt"),
  send: $("#send"),
  composer: $("#composer"),
  composerHint: $("#composer-hint"),
  agentName: $("#agent-name"),
  agentRuntime: $("#agent-runtime"),
  agentAvatar: $("#agent-avatar"),
  runtimeCount: $("#runtime-count"),
  runtimeDot: $("#runtime-dot"),
  detailsPanel: $("#details-panel"),
  detailsContent: $("#details-content"),
  agentDialog: $("#agent-dialog"),
  agentPickerDialog: $("#agent-picker-dialog"),
  runtimeDialog: $("#runtime-dialog"),
  authDialog: $("#auth-dialog"),
  agentPickerList: $("#agent-picker-list"),
  runtimeList: $("#runtime-list"),
  providerInput: $("#agent-provider-input"),
  workspaceKindInput: $("#workspace-kind-input"),
  workspaceSourceInput: $("#workspace-source-input"),
  workspaceSourceLabel: $("#workspace-source-label"),
  agentsView: $("#agents-view"),
  runtimesView: $("#runtimes-view"),
  agentsGrid: $("#agents-grid"),
  nodesGrid: $("#nodes-grid"),
  runtimeMetrics: $("#runtime-metrics"),
  registerDialog: $("#register-dialog"),
  modelInput: $("#agent-model-input"),
};

function readStorage(key, fallback) {
  try {
    const value = JSON.parse(localStorage.getItem(key));
    return value ?? fallback;
  } catch {
    return fallback;
  }
}

function persist() {
  localStorage.setItem(STORAGE.agents, JSON.stringify(state.agents));
  localStorage.setItem(STORAGE.sessions, JSON.stringify(state.sessions));
  localStorage.setItem(STORAGE.messages, JSON.stringify(state.messages));
  localStorage.setItem(STORAGE.selection, JSON.stringify(state.selection));
}

function randomID() {
  if (typeof globalThis.crypto?.randomUUID === "function") return globalThis.crypto.randomUUID();
  if (typeof globalThis.crypto?.getRandomValues === "function") {
    const bytes = new Uint8Array(16);
    globalThis.crypto.getRandomValues(bytes);
    bytes[6] = (bytes[6] & 0x0f) | 0x40;
    bytes[8] = (bytes[8] & 0x3f) | 0x80;
    const hex = Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0"));
    return `${hex.slice(0, 4).join("")}-${hex.slice(4, 6).join("")}-${hex.slice(6, 8).join("")}-${hex.slice(8, 10).join("")}-${hex.slice(10).join("")}`;
  }
  fallbackIDCounter += 1;
  return `${Date.now().toString(36)}-${fallbackIDCounter.toString(36)}-${Math.random().toString(36).slice(2)}`;
}

function uid(prefix) {
  return `${prefix}_${randomID()}`;
}

function currentAgent() {
  return state.agents.find((agent) => agent.id === state.selection.agentID) || null;
}

function currentSession() {
  return state.sessions.find((session) => session.id === state.selection.sessionID) || null;
}

function currentMessages() {
  const session = currentSession();
  return session ? state.messages[session.id] || [] : [];
}

async function api(path, options = {}) {
  const response = await fetch(path, {
    credentials: "same-origin",
    ...options,
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
  });
  if (response.status === 401) {
    showAuth();
    throw new Error("需要 Host Token");
  }
  if (!response.ok) {
    let message = `请求失败 (${response.status})`;
    try {
      const body = await response.json();
      message = body.error || message;
    } catch { /* response has no JSON error */ }
    throw new Error(message);
  }
  if (response.status === 204) return null;
  return response.json();
}

function showAuth() {
  if (!elements.authDialog.open) elements.authDialog.showModal();
}

function showDialog(dialog) {
  if (!dialog.open) dialog.showModal();
}

function closeDialog(dialog) {
  if (dialog.open) dialog.close();
}

function toast(message) {
  const item = document.createElement("div");
  item.className = "toast";
  item.textContent = message;
  $("#toast-region").append(item);
  window.setTimeout(() => item.remove(), 3200);
}

function initials(value) {
  return (value || "R").trim().slice(0, 1).toUpperCase();
}

function formatRelative(value) {
  if (!value || !Number.isFinite(new Date(value).getTime())) return "未知";
  const elapsed = Math.max(0, Date.now() - new Date(value).getTime());
  const minute = 60_000;
  if (elapsed < minute) return "刚刚";
  if (elapsed < 60 * minute) return `${Math.floor(elapsed / minute)} 分钟前`;
  if (elapsed < 24 * 60 * minute) return `${Math.floor(elapsed / (60 * minute))} 小时前`;
  return new Intl.DateTimeFormat("zh-CN", { month: "short", day: "numeric" }).format(new Date(value));
}

function statusLabel(status) {
  return ({ queued: "等待中", running: "执行中", cancelling: "取消中", cancelled: "已取消", succeeded: "已完成", failed: "失败" })[status] || status;
}

function runtimeProviders() {
  const providers = [];
  for (const node of state.nodes) {
    for (const runtime of node.runtimes || []) {
      if (!providers.some((item) => item.provider === runtime.provider)) providers.push(runtime);
    }
  }
  if (!providers.length) return [{ provider: "codex" }, { provider: "trae" }];
  return providers;
}

function runtimeInstanceID(node, runtime) {
  return runtime.id || `${node.id}/${runtime.provider}`;
}

function runtimeInstances() {
  return state.nodes.flatMap((node) => (node.runtimes || []).map((runtime) => ({ node, runtime, id: runtimeInstanceID(node, runtime) })));
}

function findRuntimeInstance(id) {
  return runtimeInstances().find((item) => item.id === id) || null;
}

function modelsForRuntime(provider, runtimeID = "") {
  const models = new Map();
  for (const node of state.nodes) {
    for (const runtime of node.runtimes || []) {
      if (runtime.provider !== provider) continue;
      if (runtimeID && runtimeInstanceID(node, runtime) !== runtimeID) continue;
      for (const model of runtime.model_catalog || []) {
        if (model.id && !models.has(model.id)) models.set(model.id, model);
      }
      for (const model of [runtime.default_model, ...(runtime.models || [])]) {
        if (model && !models.has(model)) models.set(model, { id: model, display_name: model, default: model === runtime.default_model });
      }
    }
  }
  return [...models.values()].sort((left, right) => Number(right.default) - Number(left.default));
}

function selectLabel(select) {
  if (select.getAttribute("aria-label")) return select.getAttribute("aria-label");
  const label = select.closest("label");
  return label?.childNodes[0]?.textContent?.trim() || select.name || "选择选项";
}

function closeSelect(control, restoreFocus = false) {
  if (!control) return;
  control.wrapper.classList.remove("open");
  control.trigger.setAttribute("aria-expanded", "false");
  control.menu.hidden = true;
  if (openSelectControl === control) openSelectControl = null;
  if (restoreFocus) control.trigger.focus();
}

function openSelect(control) {
  if (openSelectControl && openSelectControl !== control) closeSelect(openSelectControl);
  refreshEnhancedSelect(control.select);
  control.wrapper.classList.add("open");
  control.trigger.setAttribute("aria-expanded", "true");
  control.menu.hidden = false;
  openSelectControl = control;
  const selected = control.menu.querySelector('[aria-selected="true"]') || control.menu.querySelector("button");
  selected?.focus();
}

function chooseSelectOption(control, value) {
  control.select.value = value;
  control.select.dispatchEvent(new Event("change", { bubbles: true }));
  refreshEnhancedSelect(control.select);
  closeSelect(control, true);
}

function refreshEnhancedSelect(select) {
  const control = enhancedSelects.get(select);
  if (!control) return;
  const selected = select.selectedOptions[0] || select.options[0];
  control.value.textContent = selected?.textContent || "请选择";
  control.trigger.disabled = select.disabled;
  control.menu.replaceChildren();
  for (const option of select.options) {
    const item = document.createElement("button");
    item.type = "button";
    item.className = "custom-select-option";
    item.setAttribute("role", "option");
    item.setAttribute("aria-selected", String(option.selected));
    item.disabled = option.disabled;
    const copy = document.createElement("span");
    const title = document.createElement("strong"); title.textContent = option.textContent;
    copy.append(title);
    if (option.dataset.description) {
      const description = document.createElement("small"); description.textContent = option.dataset.description; copy.append(description);
    }
    const check = document.createElement("span"); check.className = "custom-select-check"; check.textContent = "✓"; check.setAttribute("aria-hidden", "true");
    item.append(copy, check);
    item.addEventListener("click", () => chooseSelectOption(control, option.value));
    item.addEventListener("keydown", (event) => {
      const items = [...control.menu.querySelectorAll("button:not(:disabled)")];
      const index = items.indexOf(item);
      if (event.key === "ArrowDown" || event.key === "ArrowUp") {
        event.preventDefault();
        items[(index + (event.key === "ArrowDown" ? 1 : -1) + items.length) % items.length]?.focus();
      } else if (event.key === "Home" || event.key === "End") {
        event.preventDefault(); items[event.key === "Home" ? 0 : items.length - 1]?.focus();
      } else if (event.key === "Escape" || event.key === "Tab") {
        closeSelect(control, event.key === "Escape");
      }
    });
    control.menu.append(item);
  }
}

function enhanceSelect(select) {
  if (enhancedSelects.has(select)) return;
  const wrapper = document.createElement("div"); wrapper.className = "custom-select";
  select.before(wrapper); wrapper.append(select); select.classList.add("native-select"); select.tabIndex = -1; select.setAttribute("aria-hidden", "true");
  const trigger = document.createElement("button"); trigger.type = "button"; trigger.className = "custom-select-trigger"; trigger.setAttribute("role", "combobox"); trigger.setAttribute("aria-haspopup", "listbox"); trigger.setAttribute("aria-expanded", "false"); trigger.setAttribute("aria-label", selectLabel(select));
  const value = document.createElement("span"); value.className = "custom-select-value";
  const chevron = document.createElement("span"); chevron.className = "custom-select-chevron"; chevron.textContent = "⌄"; chevron.setAttribute("aria-hidden", "true");
  trigger.append(value, chevron);
  const menu = document.createElement("div"); menu.className = "custom-select-menu"; menu.id = `${select.id || select.name}-listbox`; menu.setAttribute("role", "listbox"); menu.hidden = true; trigger.setAttribute("aria-controls", menu.id);
  wrapper.append(trigger, menu);
  const control = { select, wrapper, trigger, value, menu };
  enhancedSelects.set(select, control);
  trigger.addEventListener("click", () => control.menu.hidden ? openSelect(control) : closeSelect(control));
  trigger.addEventListener("keydown", (event) => {
    if (["ArrowDown", "ArrowUp", "Enter", " "].includes(event.key)) { event.preventDefault(); openSelect(control); }
    else if (event.key === "Escape") closeSelect(control);
  });
  select.addEventListener("change", () => refreshEnhancedSelect(select));
  new MutationObserver(() => refreshEnhancedSelect(select)).observe(select, { childList: true, subtree: true, attributes: true });
  refreshEnhancedSelect(select);
}

function enhanceSelects() {
  document.querySelectorAll("select").forEach(enhanceSelect);
  document.addEventListener("pointerdown", (event) => {
    if (openSelectControl && !openSelectControl.wrapper.contains(event.target)) closeSelect(openSelectControl);
  });
}

function agentRuntimeLabel(agent) {
  const target = agent.runtimeID ? findRuntimeInstance(agent.runtimeID) : null;
  const runtime = target
    ? `${target.node.id} · ${agent.provider}`
    : agent.runtimeID
      ? `${agent.provider} · 固定实例不可用`
      : `${agent.provider} · 自动调度`;
  return agent.model ? `${runtime} · ${agent.model}` : `${runtime} · 默认模型`;
}

function runtimeSupportsAgent(node, runtime, agent) {
  if (agent.runtimeID && runtimeInstanceID(node, runtime) !== agent.runtimeID) return false;
  if (runtime.provider !== agent.provider || runtime.state === "unhealthy") return false;
  if (!agent.model || !(runtime.models || []).length) return true;
  return runtime.default_model === agent.model || runtime.models.includes(agent.model);
}

function agentIsAvailable(agent) {
  return state.nodes.some((node) => node.state === "online" && node.runtimes?.some((runtime) => runtimeSupportsAgent(node, runtime, agent)));
}

function render() {
  renderRoute();
  renderHeader();
  renderSessions();
  renderMessages();
  renderAgentPicker();
  renderRuntimes();
  renderAgentManagement();
  renderRuntimeManagement();
  renderDetails();
  updateComposerAction();
}

function routeName() {
  const value = location.hash.replace("#", "").split("/")[0];
  return ["agents", "runtimes"].includes(value) ? value : "chat";
}

function renderRoute() {
  const route = routeName();
  document.querySelectorAll("[data-route]").forEach((link) => {
    link.classList.toggle("active", link.dataset.route === route);
    if (link.dataset.route === route) link.setAttribute("aria-current", "page");
    else link.removeAttribute("aria-current");
  });
  $("#management-topbar").classList.toggle("hidden", route === "chat");
  $("#breadcrumb-page").textContent = route === "agents" ? "Agents" : "Runtimes";
  document.title = `${({chat:"对话", agents:"Agents", runtimes:"Runtimes"})[route]} · Relay`;
  document.querySelectorAll(".chat-only").forEach((item) => item.classList.toggle("hidden", route !== "chat"));
  elements.agentsView.classList.toggle("hidden", route !== "agents");
  elements.runtimesView.classList.toggle("hidden", route !== "runtimes");
  $("#main-content").classList.toggle("management", route !== "chat");
  $("#agent-count").textContent = state.agents.length;
  $("#nav-runtime-count").textContent = state.nodes.flatMap((node) => node.runtimes || []).length;
}

function renderHeader() {
  const agent = currentAgent();
  if (agent) {
    elements.agentName.textContent = agent.name;
    elements.agentRuntime.textContent = `${agentRuntimeLabel(agent)} · ${workspaceLabel(agent.workspace)}`;
    elements.agentAvatar.textContent = initials(agent.name);
  } else {
    elements.agentName.textContent = "选择 Agent";
    elements.agentRuntime.textContent = "创建一个配置后开始对话";
    elements.agentAvatar.textContent = "R";
  }
  const onlineNodes = state.nodes.filter((node) => node.state === "online");
  const available = onlineNodes.flatMap((node) => node.runtimes || []).filter((runtime) => runtime.state !== "unhealthy");
  elements.runtimeCount.textContent = state.nodesError ? "连接已中断" : !state.nodesLoaded ? "正在连接…" : `${available.length} 个 Runtime 可用`;
  elements.runtimeDot.className = `status-dot ${state.nodesError ? "offline" : available.length ? "online" : ""}`;
  const connection = $("#connection-status");
  connection.replaceChildren();
  const dot = document.createElement("span");
  dot.className = `status-dot ${state.nodesError ? "offline" : state.nodesLoaded ? "online" : ""}`;
  const connectedLabel = state.buildInfo
    ? `Relay ${state.buildInfo.version} · 协议 ${state.buildInfo.protocol_version}`
    : "已连接控制平面";
  connection.append(dot, document.createTextNode(state.nodesError ? "连接中断" : state.nodesLoaded ? connectedLabel : "正在连接…"));
}

function workspaceLabel(workspace) {
  if (!workspace?.kind) return "临时工作区";
  return ({ local: "本地目录", git: "Git 工作区", temp: "临时工作区" })[workspace.kind] || workspace.kind;
}

function renderSessions() {
  elements.sessionList.replaceChildren();
  const sessions = [...state.sessions].sort((a, b) => new Date(b.updatedAt) - new Date(a.updatedAt)).slice(0, 50);
  if (!sessions.length) {
    const empty = document.createElement("div");
    empty.className = "session-item";
    empty.innerHTML = "<small>还没有对话</small>";
    elements.sessionList.append(empty);
    return;
  }
  for (const session of sessions) {
    const agent = state.agents.find((item) => item.id === session.agentID);
    const row = document.createElement("div");
    row.className = `session-row ${session.id === state.selection.sessionID ? "active" : ""}`;
    const button = document.createElement("button");
    button.className = "session-item";
    const title = document.createElement("strong");
    title.textContent = session.title || "新对话";
    const meta = document.createElement("small");
    meta.textContent = `${agent?.name || "已删除的 Agent"} · ${formatRelative(session.updatedAt)}`;
    button.append(title, meta);
    button.addEventListener("click", () => selectSession(session.id));
    const menu = document.createElement("button");
    menu.className = "icon-button session-menu";
    menu.setAttribute("aria-label", `管理对话：${session.title}`);
    menu.textContent = "•••";
    menu.addEventListener("click", () => manageSession(session));
    row.append(button, menu);
    elements.sessionList.append(row);
  }
}

function renderMessages() {
  const messages = currentMessages();
  const distanceFromBottom = elements.chatScroll.scrollHeight - elements.chatScroll.scrollTop - elements.chatScroll.clientHeight;
  const keepPinnedToBottom = distanceFromBottom < 96;
  elements.messages.replaceChildren();
  const showWelcome = !currentAgent() || messages.length === 0;
  elements.emptyState.classList.toggle("hidden", !showWelcome);
  if (showWelcome) {
    const title = elements.emptyState.querySelector("h1");
    const copy = elements.emptyState.querySelector("p");
    const button = $("#empty-create-agent");
    $("#prompt-suggestions").classList.toggle("hidden", !currentAgent());
    if (currentAgent()) {
      title.textContent = `和 ${currentAgent().name} 开始工作`;
      copy.textContent = "描述目标和约束，Relay 会把任务交给选定的 Runtime 执行。";
      button.classList.add("hidden");
    } else {
      title.textContent = "把任务交给你的 Agent";
      copy.textContent = "选择 Runtime，写下工作方式，然后像聊天一样开始一次真实执行。";
      button.classList.remove("hidden");
    }
  }
  for (const message of messages) elements.messages.append(createMessageNode(message));
  if (messages.length && keepPinnedToBottom) scrollToBottom(false);
}

function createMessageNode(message) {
  const article = document.createElement("article");
  article.className = `message ${message.role}`;
  article.dataset.messageId = message.id;
  if (!renderedMessageIDs.has(message.id)) {
    article.classList.add("entering");
    renderedMessageIDs.add(message.id);
  }
  const meta = document.createElement("div");
  meta.className = "message-meta";
  const role = document.createElement("span");
  role.className = "message-role";
  role.textContent = message.role === "user" ? "你" : initials(currentAgent()?.name);
  const label = document.createElement("span");
  label.textContent = message.role === "user" ? "你" : currentAgent()?.name || "Agent";
  meta.append(role, label);
  const body = document.createElement("div");
  populateMessageBody(body, message);
  article.append(meta, body);
  if (message.role === "assistant" && terminalMessageStatuses.has(message.status)) {
    const actions = document.createElement("div");
    actions.className = "message-actions";
    const copy = messageAction("copy", "复制回复");
    copy.addEventListener("click", async () => {
      try {
        await copyText(message.content || "");
        copy.replaceChildren(icon("check"));
        copy.setAttribute("aria-label", "已复制");
        copy.title = "已复制";
        toast("回复已复制");
        window.setTimeout(() => {
          copy.replaceChildren(icon("copy"));
          copy.setAttribute("aria-label", "复制回复");
          copy.title = "复制回复";
        }, 1600);
      } catch {
        toast("复制失败，请手动选择回复内容。");
      }
    });
    const retry = messageAction("retry", "重新运行"); retry.disabled = state.busy;
    retry.addEventListener("click", () => retryMessage(message));
    const downloadLabel = message.artifacts?.length ? `下载${message.artifacts.length > 1 ? ` ${message.artifacts.length} 个产物` : ` ${message.artifacts[0].name || "产物"}`}` : "下载回复";
    const download = messageAction("download", downloadLabel);
    download.addEventListener("click", async () => {
      download.disabled = true;
      try {
        await downloadMessage(message);
      } catch (error) {
        toast(`下载失败：${error.message}`);
      } finally {
        download.disabled = false;
      }
    });
    actions.append(copy, retry, download); article.append(actions);
  }
  return article;
}

function messageAction(iconName, label) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "message-action";
  button.setAttribute("aria-label", label);
  button.title = label;
  button.append(icon(iconName));
  return button;
}

async function copyText(value) {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(value);
      return;
    } catch { /* fall back for non-secure or denied clipboard access */ }
  }
  const input = document.createElement("textarea");
  input.value = value;
  input.setAttribute("readonly", "");
  input.style.position = "fixed";
  input.style.opacity = "0";
  document.body.append(input);
  input.select();
  const copied = document.execCommand("copy");
  input.remove();
  if (!copied) throw new Error("clipboard unavailable");
}

async function downloadMessage(message) {
  if (!message.artifacts?.length) {
    saveBlob(new Blob([message.content || ""], { type: "text/markdown;charset=utf-8" }), "agent-response.md");
    toast("回复文件已下载");
    return;
  }
  for (const artifact of message.artifacts) {
    const response = await fetch(`/v1/artifacts/${encodeURIComponent(artifact.id)}`, { credentials: "same-origin" });
    if (response.status === 401) {
      showAuth();
      throw new Error("需要 Host Token");
    }
    if (!response.ok) throw new Error(`请求失败 (${response.status})`);
    saveBlob(await response.blob(), safeFileName(artifact.name || artifact.type || "artifact"));
  }
  toast(message.artifacts.length > 1 ? `${message.artifacts.length} 个产物已下载` : "产物已下载");
}

function saveBlob(blob, name) {
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  document.body.append(link);
  link.click();
  link.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), 1000);
}

function safeFileName(value) {
  return String(value).replace(/[\\/:*?"<>|\u0000-\u001f]/g, "-").trim() || "artifact";
}

function populateMessageBody(body, message, streamingPlainText = false) {
  body.className = "message-body";
  body.replaceChildren();
  if (message.status === "pending") {
    const pending = document.createElement("span");
    pending.className = "thinking";
    pending.append("Agent 正在工作", ...[1, 2, 3].map(() => document.createElement("i")));
    body.append(pending);
  } else if (streamingPlainText && message.status === "streaming") {
    body.textContent = message.content;
  } else {
    if (message.content) renderRichText(body, message.content);
    else if (!message.error) renderRichText(body, message.status === "cancelled" ? "本次执行已取消。" : "没有返回文本结果。");
    if (message.error) {
      const error = document.createElement("p");
      error.className = "message-error";
      error.textContent = `执行未完成：${message.error}`;
      body.append(error);
    }
  }
  const isCurrentRun = state.busy && message.runID && message.runID === state.currentRun?.id;
  if (isCurrentRun && ["pending", "streaming"].includes(message.status) && state.lastActivity) {
    const activity = document.createElement("span");
    activity.className = "activity-line";
    const dot = document.createElement("span"); dot.className = "status-dot online";
    activity.append(dot, state.lastActivity);
    body.append(activity);
  }
}

function scheduleMessageUpdate(message, richText = false) {
  const queued = pendingMessageUpdates.get(message.id);
  pendingMessageUpdates.set(message.id, { message, richText: richText || queued?.richText });
  if (messageUpdateFrame) return;
  messageUpdateFrame = requestAnimationFrame(() => {
    messageUpdateFrame = 0;
    for (const { message: pending, richText: renderAsRichText } of pendingMessageUpdates.values()) {
      const article = [...elements.messages.children].find((item) => item.dataset.messageId === pending.id);
      const body = article?.querySelector(".message-body");
      if (body) populateMessageBody(body, pending, !renderAsRichText);
      else renderMessages();
    }
    pendingMessageUpdates.clear();
    scrollToBottom(false);
  });
}

function renderRichText(container, content) {
  const blocks = String(content).split(/```/);
  blocks.forEach((block, index) => {
    if (index % 2) {
      const pre = document.createElement("pre");
      const code = document.createElement("code");
      code.textContent = block.replace(/^\w+\n/, "").trimEnd();
      pre.append(code); container.append(pre); return;
    }
    for (const paragraph of block.split(/\n{2,}/)) {
      if (!paragraph) continue;
      const lines = paragraph.split("\n");
      const unordered = lines.every((line) => /^\s*[-*]\s+/.test(line));
      const ordered = lines.every((line) => /^\s*\d+\.\s+/.test(line));
      const table = parseTable(lines);
      if (table) {
        container.append(table); continue;
      }
      if (unordered || ordered) {
        const list = document.createElement(ordered ? "ol" : "ul");
        for (const line of lines) {
          const item = document.createElement("li");
          appendInline(item, line.replace(/^\s*(?:[-*]|\d+\.)\s+/, ""));
          list.append(item);
        }
        container.append(list); continue;
      }
      const heading = paragraph.match(/^(#{1,3})\s+(.+)$/s);
      if (heading) {
        const node = document.createElement(`h${Math.min(heading[1].length + 2, 5)}`);
        appendInline(node, heading[2]); container.append(node); continue;
      }
      const node = document.createElement("p"); appendInline(node, paragraph); container.append(node);
    }
  });
}

function parseTable(lines) {
  if (lines.length < 2) return null;
  const header = splitTableRow(lines[0]);
  const separators = splitTableRow(lines[1]);
  if (!header || !separators || header.length !== separators.length) return null;
  if (!separators.every((cell) => /^:?-{3,}:?$/.test(cell.replace(/\s/g, "")))) return null;
  const rows = [];
  for (const line of lines.slice(2)) {
    const cells = splitTableRow(line);
    if (!cells) return null;
    rows.push(cells);
  }

  const wrapper = document.createElement("div");
  wrapper.className = "markdown-table-wrap";
  const table = document.createElement("table");
  table.className = "markdown-table";
  const head = document.createElement("thead");
  const headerRow = document.createElement("tr");
  const alignments = separators.map((separator) => {
    const value = separator.replace(/\s/g, "");
    if (value.startsWith(":") && value.endsWith(":")) return "center";
    if (value.endsWith(":")) return "right";
    return "left";
  });
  header.forEach((value, index) => {
    const cell = document.createElement("th");
    cell.scope = "col";
    cell.style.textAlign = alignments[index];
    appendInline(cell, value);
    headerRow.append(cell);
  });
  head.append(headerRow);
  table.append(head);

  if (rows.length) {
    const body = document.createElement("tbody");
    for (const values of rows) {
      const row = document.createElement("tr");
      for (let index = 0; index < header.length; index += 1) {
        const cell = document.createElement("td");
        cell.style.textAlign = alignments[index];
        appendInline(cell, values[index] || "");
        row.append(cell);
      }
      body.append(row);
    }
    table.append(body);
  }
  wrapper.append(table);
  return wrapper;
}

function splitTableRow(line) {
  const value = line.trim();
  if (!value.includes("|")) return null;
  const cells = [];
  let cell = "";
  let inCode = false;
  for (let index = 0; index < value.length; index += 1) {
    const character = value[index];
    if (character === "\\" && value[index + 1] === "|") {
      cell += "|";
      index += 1;
    } else if (character === "`") {
      inCode = !inCode;
      cell += character;
    } else if (character === "|" && !inCode) {
      cells.push(cell.trim());
      cell = "";
    } else {
      cell += character;
    }
  }
  cells.push(cell.trim());
  if (value.startsWith("|") && cells[0] === "") cells.shift();
  if (cells[cells.length - 1] === "") cells.pop();
  return cells.length >= 2 ? cells : null;
}

function appendInline(container, text) {
  const pattern = /(\*\*[^*]+\*\*|`[^`]+`|\[[^\]]+\]\(https?:\/\/[^\s)]+\))/g;
  let offset = 0;
  for (const match of text.matchAll(pattern)) {
    container.append(document.createTextNode(text.slice(offset, match.index)));
    const token = match[0];
    if (token.startsWith("**")) {
      const strong = document.createElement("strong"); strong.textContent = token.slice(2, -2); container.append(strong);
    } else if (token.startsWith("`")) {
      const code = document.createElement("code"); code.textContent = token.slice(1, -1); container.append(code);
    } else {
      const parts = token.match(/^\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)$/);
      const link = document.createElement("a"); link.textContent = parts[1]; link.href = parts[2]; link.target = "_blank"; link.rel = "noreferrer noopener"; container.append(link);
    }
    offset = match.index + token.length;
  }
  container.append(document.createTextNode(text.slice(offset)));
}

function retryMessage(message) {
  const values = currentMessages();
  const index = values.findIndex((item) => item.id === message.id);
  const user = values.slice(0, index).reverse().find((item) => item.role === "user");
  if (!user || state.busy) return;
  elements.prompt.value = user.content;
  resizePrompt();
  elements.composer.requestSubmit();
}

function renderAgentPicker() {
  elements.agentPickerList.replaceChildren();
  if (!state.agents.length) {
    const empty = document.createElement("div");
    empty.className = "details-placeholder";
    empty.style.marginTop = "0";
    empty.textContent = "还没有 Agent 配置";
    elements.agentPickerList.append(empty);
    return;
  }
  for (const agent of state.agents) {
    const row = document.createElement("div");
    row.className = "agent-option";
    const select = document.createElement("button");
    select.className = "agent-option";
    select.style.border = "0";
    select.style.padding = "0";
    const avatar = document.createElement("span");
    avatar.className = "agent-avatar";
    avatar.textContent = initials(agent.name);
    const copy = document.createElement("span");
    copy.className = "agent-option-copy";
    const name = document.createElement("strong");
    name.textContent = agent.name;
    const runtime = document.createElement("small");
    runtime.textContent = `${agentRuntimeLabel(agent)} · ${workspaceLabel(agent.workspace)}`;
    copy.append(name, runtime);
    select.append(avatar, copy);
    select.addEventListener("click", () => {
      state.selection.agentID = agent.id;
      state.selection.sessionID = "";
      persist();
      closeDialog(elements.agentPickerDialog);
      render();
    });
    const edit = document.createElement("button");
    edit.className = "icon-button agent-edit";
    edit.setAttribute("aria-label", `编辑 ${agent.name}`);
    edit.textContent = "•••";
    edit.addEventListener("click", () => openAgentEditor(agent));
    row.append(select, edit);
    elements.agentPickerList.append(row);
  }
}

function renderRuntimes() {
  elements.runtimeList.replaceChildren();
  if (!state.nodes.length) {
    const empty = document.createElement("div");
    empty.className = "details-placeholder";
    empty.style.marginTop = "0";
    empty.textContent = "没有发现在线节点。请先启动 relay-node。";
    elements.runtimeList.append(empty);
    return;
  }
  for (const node of state.nodes) {
    for (const runtime of node.runtimes || []) {
      const card = document.createElement("div");
      card.className = "runtime-card";
      const dot = document.createElement("span");
      dot.className = `status-dot ${node.state === "online" && runtime.state !== "unhealthy" ? "online" : "offline"}`;
      const copy = document.createElement("span");
      copy.className = "runtime-card-copy";
      const name = document.createElement("strong");
      name.textContent = runtime.provider;
      const info = document.createElement("small");
      info.textContent = `${node.id} · Node ${node.version || "dev"} · 协议 ${node.protocol_version || "未知"} · Runtime ${runtime.version || "版本未知"} · ${node.active || 0}/${node.capacity}`;
      copy.append(name, info);
      const badge = document.createElement("span");
      badge.className = "runtime-badge";
      badge.textContent = runtime.state || node.state;
      card.append(dot, copy, badge);
      elements.runtimeList.append(card);
    }
  }
}

function renderAgentManagement() {
  elements.agentsGrid.replaceChildren();
  const healthy = state.agents.filter(agentIsAvailable).length;
  $("#agent-summary").innerHTML = `<strong>${state.agents.length}</strong><span>个 Agent</span><span>·</span><span>${healthy} 个可执行</span>`;
  if (!state.agents.length) {
    renderAgentEmpty("创建你的第一个 Agent", "选择 Runtime，设定工作方式，把常用任务变成默契配合。", "创建 Agent", () => openAgentEditor());
    $("#agent-pagination").classList.add("hidden");
    return;
  }
  const query = $("#agent-search").value.trim().toLowerCase();
  const sort = $("#agent-sort").value;
  const lastUsedAt = (agent) => state.sessions.filter((session) => session.agentID === agent.id).reduce((latest, session) => Math.max(latest, new Date(session.updatedAt).getTime() || 0), new Date(agent.createdAt).getTime() || 0);
  const agents = state.agents.filter((agent) => `${agent.name} ${agentRuntimeLabel(agent)} ${agent.model || ""} ${workspaceLabel(agent.workspace)} ${agent.instructions || ""}`.toLowerCase().includes(query));
  agents.sort((left, right) => sort === "name"
    ? left.name.localeCompare(right.name, "zh-CN")
    : sort === "created"
      ? (new Date(right.createdAt).getTime() || 0) - (new Date(left.createdAt).getTime() || 0)
      : lastUsedAt(right) - lastUsedAt(left));
  if (!agents.length) {
    renderAgentEmpty("没有找到匹配的 Agent", "试试其他名称、Runtime 或工作区。", "清除搜索", () => { $("#agent-search").value = ""; state.agentPage = 1; updateFilters(); renderAgentManagement(); });
    $("#agent-pagination").classList.add("hidden");
    return;
  }
  const pageSize = 20;
  const pageCount = Math.max(1, Math.ceil(agents.length / pageSize));
  state.agentPage = Math.min(Math.max(1, state.agentPage), pageCount);
  const pageAgents = agents.slice((state.agentPage - 1) * pageSize, state.agentPage * pageSize);
  for (const agent of pageAgents) {
    const row = document.createElement("tr");
    const identity = document.createElement("td"); identity.className = "agent-identity-cell";
    const identityWrap = document.createElement("div"); identityWrap.className = "agent-row-identity";
    const avatar = document.createElement("span"); avatar.className = "agent-avatar"; avatar.textContent = initials(agent.name);
    const title = document.createElement("span"); title.className = "agent-row-title";
    const strong = document.createElement("strong"); strong.textContent = agent.name;
    const small = document.createElement("small"); small.textContent = agent.instructions || "使用 Runtime 默认工作方式";
    title.append(strong, small); identityWrap.append(avatar, title); identity.append(identityWrap);
    const runtime = document.createElement("td"); runtime.className = "agent-runtime-cell"; runtime.dataset.label = "Runtime";
    const runtimeBadge = document.createElement("span"); runtimeBadge.className = "agent-runtime-badge"; runtimeBadge.textContent = agent.provider; runtimeBadge.setAttribute("translate", "no"); runtime.append(runtimeBadge);
    const target = agent.runtimeID ? findRuntimeInstance(agent.runtimeID) : null;
    const scheduling = document.createElement("small"); scheduling.textContent = target ? target.node.id : agent.runtimeID ? `固定实例 · ${agent.runtimeID}` : "自动调度"; scheduling.title = scheduling.textContent; runtime.append(scheduling);
    const model = document.createElement("small"); model.textContent = agent.model || "默认模型"; model.title = model.textContent; model.setAttribute("translate", "no"); runtime.append(model);
    const workspace = document.createElement("td"); workspace.className = "agent-workspace-cell"; workspace.dataset.label = "工作区"; workspace.textContent = workspaceLabel(agent.workspace);
    const sessions = state.sessions.filter((session) => session.agentID === agent.id).length;
    const usage = document.createElement("td"); usage.className = "agent-conversations-cell"; usage.dataset.label = "对话"; usage.textContent = String(sessions);
    const available = agentIsAvailable(agent);
    const status = document.createElement("td"); status.className = "agent-status-cell"; status.dataset.label = "状态";
    const statusBadge = document.createElement("span"); statusBadge.className = `runtime-health ${available ? "" : "offline"}`; statusBadge.textContent = available ? "可执行" : "不可用"; status.append(statusBadge);
    const actions = document.createElement("td"); actions.className = "agent-actions-cell";
    const actionWrap = document.createElement("div"); actionWrap.className = "agent-row-actions";
    const chat = document.createElement("button"); chat.className = "secondary-button compact-button"; chat.textContent = "开始对话";
    chat.addEventListener("click", () => { state.selection.agentID = agent.id; state.selection.sessionID = ""; persist(); location.hash = "chat"; render(); });
    const edit = document.createElement("button"); edit.className = "icon-button"; edit.setAttribute("aria-label", `编辑 ${agent.name}`); edit.textContent = "•••"; edit.addEventListener("click", () => openAgentEditor(agent));
    actionWrap.append(chat, edit); actions.append(actionWrap);
    row.append(identity, runtime, workspace, usage, status, actions); elements.agentsGrid.append(row);
  }
  renderAgentPagination(agents.length, pageCount);
}

function renderAgentEmpty(title, description, actionLabel, onAction) {
  const row = document.createElement("tr"); row.className = "agent-empty-row";
  const cell = document.createElement("td"); cell.colSpan = 6;
  const content = document.createElement("div"); content.className = "agent-table-empty";
  content.append(emptyContent(title, description, actionLabel, onAction)); cell.append(content); row.append(cell); elements.agentsGrid.append(row);
}

function renderAgentPagination(total, pageCount) {
  const pagination = $("#agent-pagination");
  pagination.replaceChildren();
  pagination.classList.toggle("hidden", pageCount <= 1);
  if (pageCount <= 1) return;
  const info = document.createElement("span"); info.textContent = `第 ${state.agentPage} / ${pageCount} 页 · 共 ${total} 个`;
  const actions = document.createElement("div");
  const previous = document.createElement("button"); previous.className = "secondary-button compact-button"; previous.textContent = "上一页"; previous.disabled = state.agentPage === 1;
  const next = document.createElement("button"); next.className = "secondary-button compact-button"; next.textContent = "下一页"; next.disabled = state.agentPage === pageCount;
  previous.addEventListener("click", () => { state.agentPage -= 1; updateFilters(); renderAgentManagement(); });
  next.addEventListener("click", () => { state.agentPage += 1; updateFilters(); renderAgentManagement(); });
  actions.append(previous, next); pagination.append(info, actions);
}

const ICONS = {
  node: '<rect x="3" y="4" width="18" height="13" rx="2"/><path d="M8 21h8M12 17v4"/>',
  runtime: '<path d="m8 7-5 5 5 5m8-10 5 5-5 5M14 4l-4 16"/>',
  capacity: '<path d="M4 19V9m8 10V4m8 15v-7"/>',
  search: '<circle cx="10.5" cy="10.5" r="6.5"/><path d="m16 16 4 4"/>',
  trae: '<path d="m12 3 9 5-9 5-9-5 9-5Zm-9 9 9 5 9-5M3 16l9 5 9-5"/>',
  copy: '<rect x="8" y="8" width="11" height="11" rx="2"/><path d="M16 8V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h2"/>',
  check: '<path d="m5 12 4 4L19 6"/>',
  retry: '<path d="M20 11a8 8 0 1 0-2.34 5.66M20 4v7h-7"/>',
  download: '<path d="M12 3v12m0 0 5-5m-5 5-5-5M5 21h14"/>',
};
function icon(name) {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("aria-hidden", "true");
  svg.innerHTML = ICONS[name] || ICONS.runtime;
  return svg;
}
function emptyContent(title, description, actionLabel, onAction) {
  const content = document.createElement("div");
  const strong = document.createElement("strong"); strong.textContent = title;
  const text = document.createElement("p"); text.textContent = description;
  content.append(strong, text);
  if (actionLabel) {
    const button = document.createElement("button"); button.className = "secondary-button"; button.textContent = actionLabel;
    button.addEventListener("click", onAction); content.append(button);
  }
  return content;
}
function renderRuntimeManagement() {
  const runtimes = state.nodes.flatMap((node) => (node.runtimes || []).map((runtime) => ({ ...runtime, node })));
  const onlineNodes = state.nodes.filter((node) => node.state === "online");
  const healthy = runtimes.filter((item) => item.node.state === "online" && item.state !== "unhealthy");
  const active = onlineNodes.reduce((sum, node) => sum + (node.active || 0), 0);
  const capacity = onlineNodes.reduce((sum, node) => sum + (node.capacity || 0), 0);
  const metrics = [
    ["node", "在线节点", onlineNodes.length, state.nodes.length, "在线 / 已注册机器"],
    ["runtime", "可用 Runtime", healthy.length, null, "已就绪，随时可以执行"],
    ["capacity", "执行容量", active, capacity, capacity ? `还可并行执行 ${Math.max(0,capacity-active)} 个任务` : "连接节点以获得执行容量"],
  ];
  elements.runtimeMetrics.replaceChildren();
  for (const [symbol, label, number, total, caption] of metrics) {
    const card = document.createElement("div"); card.className = "metric-card";
    const title = document.createElement("small"); title.append(icon(symbol), document.createTextNode(label));
    const value = document.createElement("strong"); value.textContent = state.nodesLoaded ? String(number) : "—";
    if (total !== null && state.nodesLoaded) { const denominator = document.createElement("span"); denominator.textContent = `/ ${total}`; value.append(denominator); }
    const note = document.createElement("span"); note.className = "metric-caption";
    note.textContent = state.nodesError ? "连接中断 · 上次获取的数据" : !state.nodesLoaded ? "正在获取状态…" : caption;
    card.append(title, value, note); elements.runtimeMetrics.append(card);
  }
  $("#node-count").textContent = state.nodes.length;
  const notice = $("#connection-notice");
  notice.classList.toggle("hidden", !state.nodesError);
  notice.textContent = state.nodesError ? "无法获取最新节点状态。请检查服务连接后点击刷新；以下为上次获取的数据。" : "";
  elements.nodesGrid.replaceChildren();
  const query = $("#runtime-search").value.trim().toLowerCase();
  const filter = $("#node-filter").value;
  const nodes = state.nodes.filter((node) => (filter === "all" || (filter === "online" ? node.state === "online" : node.state !== "online")) &&
    `${node.id} ${(node.runtimes || []).map((r) => `${r.provider} ${runtimeInstanceID(node, r)}`).join(" ")}`.toLowerCase().includes(query));
  if (!nodes.length) {
    const empty = document.createElement("div"); empty.className = "empty-resource";
    const mark = document.createElement("span"); mark.className = "empty-icon"; mark.append(icon("node")); empty.append(mark);
    if (!state.nodesLoaded && !state.nodesError) empty.append(emptyContent("正在发现节点…", "连接后即可查看可用的 Runtime 与执行容量。"));
    else if (query || filter !== "all") empty.append(emptyContent("没有匹配的节点", "试试其他关键词，或清除当前筛选。", "清除筛选", () => { $("#runtime-search").value = ""; $("#node-filter").value = "all"; updateFilters(); renderRuntimeManagement(); }));
    else if (state.nodesError) empty.append(emptyContent("暂时无法连接", "服务恢复后可重新获取节点列表。", "重新连接", () => loadNodes()));
    else empty.append(emptyContent("连接你的第一台机器", "注册一个 Runtime，让 Agent 在你的机器上开始工作。", "注册 Runtime", openRegistration));
    elements.nodesGrid.append(empty); return;
  }
  for (const node of nodes) {
    const card = document.createElement("article"); card.className = "node-card";
    const head = document.createElement("header"); head.className = "node-card-head";
    const mark = document.createElement("span"); mark.className = "node-symbol"; mark.append(icon("node"));
    const copy = document.createElement("span"); copy.className = "node-card-head-copy";
    const name = document.createElement("strong"); name.textContent = node.id;
    const meta = document.createElement("small"); meta.textContent = `${node.runtimes?.length || 0} 个 Runtime · Node ${node.version || "dev"} · 协议 ${node.protocol_version || "未知"} · 最近心跳 ${formatRelative(node.last_seen)}`;
    copy.append(name, meta);
    const badge = document.createElement("span"); badge.className = `node-state-badge ${node.state === "online" ? "" : "offline"}`; badge.textContent = node.state === "online" ? "在线" : "离线";
    const loadGroup = document.createElement("span"); loadGroup.className = "node-load-group";
    const load = document.createElement("span"); load.className = "node-load"; load.textContent = `${node.active || 0} / ${node.capacity || 0} 执行中`;
    const track = document.createElement("span"); track.className = "load-track";
    const value = document.createElement("span"); value.className = "load-value"; value.style.width = `${Math.min(100, Math.max(0,(node.active || 0) / Math.max(1,node.capacity || 0) * 100))}%`;
    track.append(value); loadGroup.append(load, track); head.append(mark, copy, badge, loadGroup);
    const columns = document.createElement("div"); columns.className = "runtime-columns"; columns.setAttribute("aria-hidden", "true");
    for (const label of ["Runtime", "版本", "状态"]) { const text = document.createElement("span"); text.textContent = label; columns.append(text); }
    const list = document.createElement("div"); list.className = "node-runtimes";
    for (const runtime of node.runtimes || []) {
      const row = document.createElement("div"); row.className = "node-runtime";
      const identity = document.createElement("div"); identity.className = "runtime-identity";
      const runtimeMark = document.createElement("span"); runtimeMark.className = `runtime-mark ${runtime.provider === "trae" ? "trae" : ""}`; runtimeMark.append(icon(runtime.provider === "trae" ? "trae" : "runtime"));
      const runtimeCopy = document.createElement("span"); runtimeCopy.className = "node-runtime-copy";
      const runtimeName = document.createElement("strong"); runtimeName.textContent = runtime.provider; runtimeName.setAttribute("translate", "no");
      const runtimeID = document.createElement("small"); runtimeID.textContent = runtimeInstanceID(node, runtime); runtimeID.setAttribute("translate", "no");
      runtimeCopy.append(runtimeName, runtimeID);
      if (runtime.state === "unhealthy" && runtime.message) { const error = document.createElement("small"); error.textContent = runtime.message; runtimeCopy.append(error); }
      const version = document.createElement("span"); version.className = "runtime-version"; version.textContent = runtime.version ? `v${runtime.version.replace(/^v/,"")}` : "—";
      const isHealthy = node.state === "online" && runtime.state !== "unhealthy";
      const health = document.createElement("span"); health.className = `runtime-health ${isHealthy ? "" : "offline"}`; health.textContent = isHealthy ? "就绪" : "不可用";
      identity.append(runtimeMark, runtimeCopy); row.append(identity, version, health); list.append(row);
    }
    if (!node.runtimes?.length) { const empty = document.createElement("p"); empty.className = "form-help"; empty.style.padding = "0 20px"; empty.textContent = "节点已连接，尚未发现 Runtime。"; list.append(empty); }
    card.append(head, columns, list); elements.nodesGrid.append(card);
  }
}
function openRegistration() { updateRegistrationPreview(); showDialog(elements.registerDialog); }
function updateFilters() {
  const url = new URL(location.href);
  for (const [key, selector, fallback] of [["agent", "#agent-search", ""], ["runtime", "#runtime-search", ""], ["status", "#node-filter", "all"]]) {
    const value = $(selector).value;
    if (value && value !== fallback) url.searchParams.set(key, value);
    else url.searchParams.delete(key);
  }
  if (state.agentPage > 1) url.searchParams.set("agent_page", String(state.agentPage));
  else url.searchParams.delete("agent_page");
  history.replaceState(null, "", url);
}
function restoreFilters() {
  const params = new URLSearchParams(location.search);
  $("#agent-search").value = params.get("agent") || "";
  $("#agent-sort").value = ["name", "created"].includes(params.get("agent_sort")) ? params.get("agent_sort") : "recent";
  state.agentPage = Math.max(1, Number.parseInt(params.get("agent_page") || "1", 10) || 1);
  $("#runtime-search").value = params.get("runtime") || "";
  $("#node-filter").value = ["online", "offline"].includes(params.get("status")) ? params.get("status") : "all";
}

function renderDetails() {
  const run = state.currentRun;
  if (!run) return;
  elements.detailsContent.replaceChildren();
  const summary = document.createElement("section");
  summary.className = "detail-section";
  summary.innerHTML = `<h3>执行</h3><dl class="detail-grid"></dl>`;
  const grid = summary.querySelector("dl");
  const rows = [
    ["状态", statusLabel(run.status)],
    ["Run", run.id],
    ["Runtime", run.runtime?.provider || "—"],
    ["节点", run.attempt?.node_id || "等待分配"],
    ["创建时间", new Date(run.created_at).toLocaleString("zh-CN")],
  ];
  for (const [key, value] of rows) {
    const dt = document.createElement("dt"); dt.textContent = key;
    const dd = document.createElement("dd"); dd.textContent = value;
    grid.append(dt, dd);
  }
  const eventSection = document.createElement("section");
  eventSection.className = "detail-section";
  const heading = document.createElement("h3");
  heading.textContent = `事件 · ${state.events.length}`;
  const list = document.createElement("div");
  list.className = "event-list";
  for (const event of state.events.slice(-80).reverse()) {
    const row = document.createElement("div");
    row.className = "event-row";
    const node = document.createElement("span"); node.className = "event-node";
    const copy = document.createElement("span"); copy.className = "event-copy";
    const type = document.createElement("strong"); type.textContent = event.type;
    const time = document.createElement("small"); time.textContent = `#${event.sequence} · ${new Date(event.created_at).toLocaleTimeString("zh-CN")}`;
    copy.append(type, time); row.append(node, copy); list.append(row);
  }
  eventSection.append(heading, list);
  elements.detailsContent.append(summary, eventSection);
}

function selectSession(id) {
  const session = state.sessions.find((item) => item.id === id);
  if (!session) return;
  state.selection.sessionID = id;
  if (state.agents.some((agent) => agent.id === session.agentID)) state.selection.agentID = session.agentID;
  state.currentRun = null;
  state.events = [];
  persist();
  history.replaceState(null, "", `#chat/${encodeURIComponent(id)}`);
  closeSidebar();
  render();
  scrollToBottom(false);
}

function newChat() {
  if (!currentAgent()) {
    showDialog(elements.agentPickerDialog);
    return;
  }
  state.selection.sessionID = "";
  state.currentRun = null;
  state.events = [];
  persist();
  history.replaceState(null, "", "#chat");
  closeSidebar();
  render();
  elements.prompt.focus();
}

function manageSession(session) {
  state.editingSessionID = session.id;
  $("#session-title-input").value = session.title || "新对话";
  showDialog($("#session-dialog"));
}

function saveSession(event) {
  event.preventDefault();
  const session = state.sessions.find((item) => item.id === state.editingSessionID);
  const title = $("#session-title-input").value.trim();
  if (!session || !title) return;
  session.title = title.slice(0, 60);
  session.updatedAt = new Date().toISOString();
  persist(); closeDialog($("#session-dialog")); render(); toast("对话已重命名");
}

async function confirmRemoval(title, description) {
  const dialog = $("#confirm-dialog");
  $("#confirm-title").textContent = title;
  $("#confirm-description").textContent = description;
  dialog.returnValue = "cancel";
  return new Promise((resolve) => {
    dialog.addEventListener("close", () => resolve(dialog.returnValue === "delete"), { once: true });
    showDialog(dialog);
  });
}

async function deleteSession() {
  const session = state.sessions.find((item) => item.id === state.editingSessionID);
  if (!session || !await confirmRemoval("删除对话", `“${session.title || "新对话"}”及其消息记录将从此浏览器移除。此操作无法撤销。`)) return;
  state.sessions = state.sessions.filter((item) => item.id !== session.id);
  delete state.messages[session.id];
  if (state.selection.sessionID === session.id) state.selection.sessionID = "";
  persist(); closeDialog($("#session-dialog")); render(); toast("对话已删除");
}

function ensureSession(prompt) {
  let session = currentSession();
  if (session && session.agentID === currentAgent().id) return session;
  const now = new Date().toISOString();
  session = { id: uid("session"), agentID: currentAgent().id, title: prompt.trim().slice(0, 38), createdAt: now, updatedAt: now };
  state.sessions.push(session);
  state.messages[session.id] = [];
  state.selection.sessionID = session.id;
  return session;
}

function conversationPrompt(messages, prompt) {
  const history = messages.filter((item) => item.status !== "pending").slice(-12);
  if (!history.length) return prompt;
  const transcript = history.map((item) => `${item.role === "user" ? "User" : "Assistant"}: ${item.content}`).join("\n\n");
  return `Continue this conversation. Use the earlier messages only as context.\n\n${transcript}\n\nUser: ${prompt}`;
}

async function submitMessage(event) {
  event.preventDefault();
  const prompt = elements.prompt.value.trim();
  const agent = currentAgent();
  if (!prompt || !agent || state.busy) return;
  const session = ensureSession(prompt);
  const prior = [...(state.messages[session.id] || [])];
  const userMessage = { id: uid("message"), role: "user", content: prompt, createdAt: new Date().toISOString() };
  const assistantMessage = { id: uid("message"), role: "assistant", content: "", status: "pending", createdAt: new Date().toISOString() };
  state.messages[session.id].push(userMessage, assistantMessage);
  session.updatedAt = new Date().toISOString();
  elements.prompt.value = "";
  resizePrompt();
  state.busy = true;
  state.cancelRequested = false;
  state.currentRun = null;
  persist();
  render();
  scrollToBottom();

  const request = {
    agent_id: agent.id,
    idempotency_key: randomID(),
    session_id: session.id,
    runtime: { provider: agent.provider, ...(agent.runtimeID ? { id: agent.runtimeID } : {}), ...(agent.model ? { model: agent.model } : {}) },
    source: { kind: "playground.chat", external_id: session.id },
    input: { type: "chat.message", version: "1", prompt: conversationPrompt(prior, prompt) },
    principal: { type: "user", id: "playground" },
    timeout: "30m",
  };
  if (agent.instructions) {
    request.instructions = { agent: [{ id: `agent-${agent.id}`, version: "1", title: agent.name, content: agent.instructions }] };
  }
  if (agent.workspace?.kind) request.workspace = agent.workspace;

  try {
    const run = await api("/v1/runs", { method: "POST", body: JSON.stringify(request) });
    assistantMessage.runID = run.id;
    state.currentRun = run;
    state.events = [];
    persist();
    render();
    watchRun(run.id, assistantMessage, session);
  } catch (error) {
    finishAssistant(assistantMessage, session, "failed", error.message);
  }
}

function watchRun(runID, assistantMessage, session) {
  let finished = false;
  let checkingStatus = false;
  const source = new EventSource(`/v1/runs/${encodeURIComponent(runID)}/events/stream`);
  state.currentEventSource = source;
  source.onopen = () => {
    if (finished) return;
    state.lastActivity = "Agent 正在工作";
    scheduleMessageUpdate(assistantMessage);
  };
  source.addEventListener("relay.event", (message) => {
    const event = JSON.parse(message.data);
    const isNew = !state.events.some((item) => item.id === event.id);
    if (isNew) {
      state.events.push(event);
      applyAssistantEvent(event, assistantMessage);
    }
    renderDetails();
    if (["run.succeeded", "run.failed", "run.cancelled"].includes(event.type)) {
      finished = true;
      source.close();
      state.currentEventSource = null;
      completeRun(runID, assistantMessage, session);
    }
  });
  source.onerror = async () => {
    if (finished || checkingStatus) return;
    state.lastActivity = "连接中断，正在恢复消息…";
    scheduleMessageUpdate(assistantMessage);
    checkingStatus = true;
    try {
      const run = await api(`/v1/runs/${encodeURIComponent(runID)}`);
      if (finished) return;
      if (["succeeded", "failed", "cancelled"].includes(run.status)) {
        finished = true;
        source.close();
        state.currentEventSource = null;
        await completeRun(runID, assistantMessage, session);
      }
    } catch { /* EventSource will reconnect and resume from its last event ID. */ }
    finally { checkingStatus = false; }
  };
}

function applyAssistantEvent(event, assistantMessage) {
  if (event.type === "run.started") {
    state.currentRun.status = "running";
    renderHeader();
    return;
  }
  const activity = activityLabel(event);
  if (activity) {
    state.lastActivity = activity;
    if (["pending", "streaming"].includes(assistantMessage.status)) scheduleMessageUpdate(assistantMessage);
  }
  if (event.type === "assistant.message.delta" && event.data?.delta) {
    assistantMessage.status = "streaming";
    assistantMessage.content += event.data.delta;
    scheduleMessageUpdate(assistantMessage);
    return;
  }
  if (event.type === "assistant.message.completed" && event.data?.text) {
    assistantMessage.status = "streaming";
    assistantMessage.content = event.data.text;
    scheduleMessageUpdate(assistantMessage, true);
  }
}

function activityLabel(event) {
  if (event.type === "attempt.leased") return `已分配到 ${event.data?.node_id || "Node"}`;
  if (event.type.endsWith("thread.started")) return "Runtime 已启动";
  if (event.type.includes("commandExecution") || event.type.includes("command_execution")) return "正在执行命令";
  if (event.type.includes("reasoning")) return "正在分析";
  if (event.type === "artifact.created") return "正在保存产物";
  return "";
}

async function completeRun(runID, assistantMessage, session) {
  try {
    const run = await api(`/v1/runs/${encodeURIComponent(runID)}`);
    const artifactResponse = await api(`/v1/runs/${encodeURIComponent(runID)}/artifacts`).catch(() => []);
    const artifacts = Array.isArray(artifactResponse) ? artifactResponse : [];
    state.currentRun = run;
    if (run.status !== "succeeded") {
      const events = await api(`/v1/runs/${encodeURIComponent(runID)}/events`).catch(() => null);
      if (Array.isArray(events)) {
        let partial = "";
        for (const event of events) {
          if (run.attempt?.id && event.attempt_id !== run.attempt.id) continue;
          if (event.type === "assistant.message.delta") partial += event.data?.delta || "";
          if (event.type === "assistant.message.completed") partial = event.data?.text || partial;
        }
        assistantMessage.content = partial;
      }
    }
    const content = run.status === "succeeded" ? run.result?.summary : run.error || `执行${statusLabel(run.status)}`;
    finishAssistant(assistantMessage, session, run.status, content, artifacts);
  } catch (error) {
    finishAssistant(assistantMessage, session, "failed", error.message);
  }
}

function finishAssistant(message, session, status, content, artifacts = []) {
  message.status = status;
  message.error = status === "succeeded" ? "" : content;
  if (status === "succeeded") message.content = content;
  message.artifacts = (Array.isArray(artifacts) ? artifacts : []).filter((artifact) => !artifact.type?.includes("instruction"));
  session.updatedAt = new Date().toISOString();
  state.busy = false;
  state.cancelRequested = false;
  state.lastActivity = "";
  persist();
  render();
  scrollToBottom();
}

async function cancelCurrentRun() {
  if (!state.currentRun || !state.busy || state.cancelRequested) return;
  state.cancelRequested = true;
  updateComposerAction();
  try {
    state.currentRun = await api(`/v1/runs/${encodeURIComponent(state.currentRun.id)}/cancel`, { method: "POST", body: JSON.stringify({ reason: "用户从 Playground 停止", requested_by: "playground" }) });
    renderHeader();
  } catch (error) {
    state.cancelRequested = false;
    toast(`停止失败：${error.message}`);
  }
  updateComposerAction();
}

function openAgentEditor(agent = null) {
  closeDialog(elements.agentPickerDialog);
  state.editingAgentID = agent?.id || null;
  $("#agent-dialog-title").textContent = agent ? "编辑 Agent" : "创建 Agent";
  $("#agent-name-input").value = agent?.name || "";
  $("#agent-instructions-input").value = agent?.instructions || "";
  fillRuntimeOptions(agent);
  fillModelOptions(agent?.model || "");
  elements.workspaceKindInput.value = agent?.workspace?.kind || "";
  elements.workspaceSourceInput.value = agent?.workspace?.source || "";
  $("#delete-agent").classList.toggle("hidden", !agent);
  updateWorkspaceFields();
  showDialog(elements.agentDialog);
  window.setTimeout(() => $("#agent-name-input").focus(), 0);
}

function fillRuntimeOptions(agent = null) {
  elements.providerInput.replaceChildren();
  for (const runtime of runtimeProviders()) {
    const option = document.createElement("option");
    option.value = `auto:${runtime.provider}`;
    option.dataset.provider = runtime.provider;
    option.textContent = `${runtime.provider} · 自动调度`;
    option.dataset.description = "在所有兼容且可用的 Runtime 实例中调度";
    elements.providerInput.append(option);
  }
  for (const { node, runtime, id } of runtimeInstances()) {
    const option = document.createElement("option");
    option.value = `instance:${id}`;
    option.dataset.provider = runtime.provider;
    option.textContent = `${node.id} · ${runtime.provider}`;
    option.dataset.description = `固定 Runtime · ${runtime.version ? `v${runtime.version.replace(/^v/, "")} · ` : ""}${node.state === "online" && runtime.state !== "unhealthy" ? "可用" : "不可用"}`;
    elements.providerInput.append(option);
  }
  if (agent?.runtimeID && !findRuntimeInstance(agent.runtimeID)) {
    const option = document.createElement("option");
    option.value = `instance:${agent.runtimeID}`;
    option.dataset.provider = agent.provider;
    option.textContent = `${agent.provider} · 已断开实例`;
    option.dataset.description = agent.runtimeID;
    elements.providerInput.append(option);
  }
  if (agent?.runtimeID) elements.providerInput.value = `instance:${agent.runtimeID}`;
  else if (agent?.provider) elements.providerInput.value = `auto:${agent.provider}`;
  refreshEnhancedSelect(elements.providerInput);
}

function selectedRuntimeTarget() {
  const value = elements.providerInput.value;
  const provider = elements.providerInput.selectedOptions[0]?.dataset.provider || "";
  if (value.startsWith("instance:")) {
    const runtimeID = value.slice("instance:".length);
    return { provider, runtimeID };
  }
  return { provider: provider || value.slice("auto:".length), runtimeID: "" };
}

function fillModelOptions(selected = "") {
  const { provider, runtimeID } = selectedRuntimeTarget();
  const models = modelsForRuntime(provider, runtimeID);
  elements.modelInput.replaceChildren();
  const defaultOption = document.createElement("option");
  defaultOption.value = "";
  const runtimeDefault = models.find((model) => model.default);
  defaultOption.textContent = runtimeDefault?.display_name ? `Runtime 默认模型 · ${runtimeDefault.display_name}` : "Runtime 默认模型";
  defaultOption.dataset.description = runtimeDefault?.id || "使用 Runtime 当前配置的默认值";
  elements.modelInput.append(defaultOption);
  for (const model of models) {
    const option = document.createElement("option");
    option.value = model.id;
    option.textContent = model.display_name || model.id;
    option.dataset.description = model.description || (model.display_name && model.display_name !== model.id ? model.id : "");
    option.setAttribute("translate", "no");
    elements.modelInput.append(option);
  }
  elements.modelInput.value = models.some((model) => model.id === selected) ? selected : "";
  refreshEnhancedSelect(elements.modelInput);
}

function saveAgent(event) {
  event.preventDefault();
  try {
    const name = $("#agent-name-input").value.trim();
    const { provider, runtimeID } = selectedRuntimeTarget();
    const model = elements.modelInput.value;
    if (!name || !provider) return;
    const workspaceKind = elements.workspaceKindInput.value;
    const workspace = workspaceKind ? { kind: workspaceKind, ephemeral: workspaceKind === "temp" } : {};
    if (["local", "git"].includes(workspaceKind)) workspace.source = elements.workspaceSourceInput.value.trim();
    const value = { name, provider, runtimeID, model, instructions: $("#agent-instructions-input").value.trim(), workspace };
    if (state.editingAgentID) {
      const index = state.agents.findIndex((agent) => agent.id === state.editingAgentID);
      state.agents[index] = { ...state.agents[index], ...value };
    } else {
      value.id = uid("agent");
      value.createdAt = new Date().toISOString();
      state.agents.push(value);
      state.selection.agentID = value.id;
      state.selection.sessionID = "";
    }
    persist();
    closeDialog(elements.agentDialog);
    render();
    toast(state.editingAgentID ? "Agent 已更新" : "Agent 已创建");
    elements.prompt.focus();
  } catch (error) {
    console.error("save agent failed", error);
    toast(`保存 Agent 失败：${error?.message || "未知错误"}`);
  }
}

async function deleteAgent() {
  const id = state.editingAgentID;
  if (!id || !await confirmRemoval("删除 Agent", "已有对话会保留，但不能再使用此 Agent 继续执行。此操作无法撤销。")) return;
  state.agents = state.agents.filter((agent) => agent.id !== id);
  if (state.selection.agentID === id) {
    state.selection.agentID = state.agents[0]?.id || "";
    state.selection.sessionID = "";
  }
  persist();
  closeDialog(elements.agentDialog);
  render();
  toast("Agent 已删除");
}

function updateWorkspaceFields() {
  const needsSource = ["local", "git"].includes(elements.workspaceKindInput.value);
  elements.workspaceSourceLabel.classList.toggle("hidden", !needsSource);
  elements.workspaceSourceInput.required = needsSource;
  elements.workspaceSourceInput.placeholder = elements.workspaceKindInput.value === "git" ? "https://github.com/org/repo.git" : "/absolute/path/to/project";
}

function resizePrompt() {
  elements.prompt.style.height = "auto";
  elements.prompt.style.height = `${Math.min(elements.prompt.scrollHeight, 180)}px`;
  updateComposerAction();
}

function updateComposerAction() {
  const running = state.busy;
  elements.send.type = running ? "button" : "submit";
  elements.send.classList.toggle("is-running", running);
  elements.send.disabled = running ? !state.currentRun || state.cancelRequested : !elements.prompt.value.trim() || !currentAgent();
  elements.send.setAttribute("aria-label", running ? state.cancelRequested ? "正在停止" : "停止生成" : "发送消息");
  elements.send.title = running ? state.cancelRequested ? "正在停止" : "停止生成" : "发送消息";
  elements.composerHint.textContent = running ? state.cancelRequested ? "正在停止 Agent…" : "Agent 正在工作 · 点击停止按钮可取消" : "Enter 发送 · Shift + Enter 换行";
}

function scrollToBottom(smooth = true) {
  const prefersReducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  requestAnimationFrame(() => requestAnimationFrame(() => {
    elements.chatEndAnchor.scrollIntoView({ block: "end", behavior: smooth && !prefersReducedMotion ? "smooth" : "auto" });
  }));
}

function openSidebar() {
  elements.sidebar.classList.add("open");
  elements.sidebarBackdrop.classList.add("open");
}

function closeSidebar() {
  elements.sidebar.classList.remove("open");
  elements.sidebarBackdrop.classList.remove("open");
}

async function loadNodes() {
  if (state.refreshingNodes) return false;
  state.refreshingNodes = true;
  const button = $("#refresh-runtimes");
  button.disabled = true;
  button.setAttribute("aria-busy", "true");
  button.querySelector("svg")?.classList.add("spinner");
  try {
    const nodes = await api("/v1/nodes");
    if (!Array.isArray(nodes)) throw new Error("节点列表格式不正确");
    state.nodes = nodes;
    state.nodesLoaded = true;
    state.nodesError = "";
    closeDialog(elements.authDialog);
    render();
    await restoreActiveRun();
    return true;
  } catch (error) {
    state.nodesError = error.message;
    renderHeader(); renderRuntimeManagement();
    return false;
  } finally {
    state.refreshingNodes = false;
    button.disabled = false;
    button.removeAttribute("aria-busy");
    button.querySelector("svg")?.classList.remove("spinner");
  }
}

async function loadBuildInfo() {
  try {
    state.buildInfo = await api("/version");
    renderHeader();
  } catch {
    state.buildInfo = null;
  }
}

async function restoreActiveRun() {
  if (state.restoredRun) return;
  state.restoredRun = true;
  for (const session of [...state.sessions].sort((a, b) => new Date(b.updatedAt) - new Date(a.updatedAt))) {
    const assistant = (state.messages[session.id] || []).findLast?.((message) => message.role === "assistant" && ["pending", "streaming"].includes(message.status) && message.runID);
    if (!assistant) continue;
    try {
      const run = await api(`/v1/runs/${encodeURIComponent(assistant.runID)}`);
      state.selection.sessionID = session.id; state.selection.agentID = session.agentID; state.currentRun = run;
      if (["succeeded", "failed", "cancelled"].includes(run.status)) {
        await completeRun(run.id, assistant, session);
      } else {
        const events = await api(`/v1/runs/${encodeURIComponent(run.id)}/events`);
        state.events = Array.isArray(events) ? events : [];
        assistant.content = state.events.filter((event) => event.type === "assistant.message.delta").map((event) => event.data?.delta || "").join("");
        assistant.status = assistant.content ? "streaming" : "pending";
        state.busy = true; watchRun(run.id, assistant, session); render();
      }
    } catch { /* stale local run; leave its previous representation intact */ }
    break;
  }
}

function updateRegistrationPreview() {
  const provider = $("#register-provider").value;
  const command = $("#register-command").value.trim() || (provider === "trae" ? "traex" : "codex");
  const defaultModel = $("#register-default-model").value.trim();
  const models = $("#register-models").value.split(",").map((model) => model.trim()).filter((model, index, items) => model && items.indexOf(model) === index);
  const config = {
    server: location.origin,
    node: { id: $("#register-node-id").value.trim() || "developer-node", capacity: Number($("#register-capacity").value) || 1, runtimes: [] },
    runtimes: [{ id: `${$("#register-node-id").value.trim() || "developer-node"}/${provider}`, kind: provider, provider, protocol: "app-server", command, ...(defaultModel ? { model: defaultModel } : {}), ...(models.length ? { models } : {}), work_root: "/tmp/relay-runs", ephemeral: true }],
  };
  $("#runtime-config-preview").textContent = JSON.stringify(config, null, 2);
}

$("#agent-switcher").addEventListener("click", () => showDialog(elements.agentPickerDialog));
$("#new-chat").addEventListener("click", newChat);
$("#empty-create-agent").addEventListener("click", () => openAgentEditor());
$("#create-agent-from-picker").addEventListener("click", () => openAgentEditor());
$("#agent-form").addEventListener("submit", saveAgent);
elements.providerInput.addEventListener("change", () => fillModelOptions());
$("#delete-agent").addEventListener("click", deleteAgent);
$("#session-form").addEventListener("submit", saveSession);
$("#delete-session").addEventListener("click", deleteSession);
$("#page-create-agent").addEventListener("click", () => openAgentEditor());
elements.send.addEventListener("click", (event) => {
  if (!state.busy) return;
  event.preventDefault();
  cancelCurrentRun();
});
elements.workspaceKindInput.addEventListener("change", updateWorkspaceFields);
elements.composer.addEventListener("submit", submitMessage);
elements.prompt.addEventListener("input", resizePrompt);
elements.prompt.addEventListener("keydown", (event) => {
  if (event.key === "Enter" && !event.shiftKey && !event.isComposing) {
    event.preventDefault();
    elements.composer.requestSubmit();
  }
});
$("#open-details").addEventListener("click", () => {
  elements.detailsPanel.classList.add("open");
  elements.detailsPanel.setAttribute("aria-hidden", "false");
  elements.detailsPanel.inert = false;
  $("#close-details").focus();
});
$("#close-details").addEventListener("click", () => {
  elements.detailsPanel.classList.remove("open");
  elements.detailsPanel.setAttribute("aria-hidden", "true");
  elements.detailsPanel.inert = true;
  $("#open-details").focus();
});
$("#open-runtimes").addEventListener("click", () => { location.hash = "runtimes"; closeSidebar(); });
$("#refresh-runtimes").addEventListener("click", async () => { if (await loadNodes()) toast("Runtime 状态已更新"); });
$("#register-runtime").addEventListener("click", openRegistration);
[$("#register-provider"), $("#register-node-id"), $("#register-command"), $("#register-capacity"), $("#register-default-model"), $("#register-models")].forEach((input) => input.addEventListener("input", () => {
  if (input.id === "register-provider") $("#register-command").value = input.value === "trae" ? "traex" : "codex";
  updateRegistrationPreview();
}));
$("#copy-runtime-config").addEventListener("click", async () => {
  try { await navigator.clipboard.writeText($("#runtime-config-preview").textContent); toast("Node 配置已复制"); }
  catch { toast("复制失败，请选中配置内容手动复制。"); }
});
$("#open-sidebar").addEventListener("click", openSidebar);
$("#open-management-sidebar").addEventListener("click", openSidebar);
$("#close-sidebar").addEventListener("click", closeSidebar);
elements.sidebarBackdrop.addEventListener("click", closeSidebar);
document.querySelectorAll("[data-close-dialog]").forEach((button) => button.addEventListener("click", () => closeDialog(document.getElementById(button.dataset.closeDialog))));
window.addEventListener("hashchange", () => { closeSidebar(); render(); });
$("#auth-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const error = $("#auth-error");
  error.classList.add("hidden");
  try {
    await api("/v1/console/session", { method: "POST", body: JSON.stringify({ token: $("#host-token").value }) });
    $("#host-token").value = "";
    closeDialog(elements.authDialog);
    await loadNodes();
  } catch (cause) {
    error.textContent = cause.message;
    error.classList.remove("hidden");
  }
});

$("#agent-search").addEventListener("input", () => { state.agentPage = 1; updateFilters(); renderAgentManagement(); });
$("#agent-sort").addEventListener("change", () => {
  state.agentPage = 1;
  const url = new URL(location.href);
  if ($("#agent-sort").value === "recent") url.searchParams.delete("agent_sort");
  else url.searchParams.set("agent_sort", $("#agent-sort").value);
  url.searchParams.delete("agent_page");
  history.replaceState(null, "", url);
  renderAgentManagement();
});
$("#runtime-search").addEventListener("input", () => { updateFilters(); renderRuntimeManagement(); });
$("#node-filter").addEventListener("change", () => { updateFilters(); renderRuntimeManagement(); });
document.querySelectorAll("[data-prompt]").forEach((button) => button.addEventListener("click", () => {
  elements.prompt.value = button.dataset.prompt; resizePrompt(); elements.prompt.focus();
}));
document.querySelectorAll("dialog").forEach((dialog) => {
  const heading = dialog.querySelector("h2");
  if (heading) { if (!heading.id) heading.id = `${dialog.id}-title`; dialog.setAttribute("aria-labelledby", heading.id); }
});
window.addEventListener("keydown", (event) => { if (event.key === "Escape") { closeSidebar(); if (elements.detailsPanel.classList.contains("open")) $("#close-details").click(); } });
window.addEventListener("popstate", () => { restoreFilters(); render(); });
restoreFilters();
enhanceSelects();

const initialSessionID = decodeURIComponent(location.hash.split("/")[1] || "");
if (initialSessionID && state.sessions.some((session) => session.id === initialSessionID)) state.selection.sessionID = initialSessionID;
if (!currentAgent() && state.agents.length) state.selection.agentID = currentSession()?.agentID || state.agents[0].id;
if (!location.hash) history.replaceState(null, "", "#chat");
render();
if (currentMessages().length) scrollToBottom(false);
loadBuildInfo();
loadNodes();
window.setInterval(loadNodes, 15_000);
