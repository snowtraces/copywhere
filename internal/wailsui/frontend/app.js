// copywhere · Wails v3 桌面窗口前端
//
// 通过 Wails 绑定直接调用进程内的 Go 服务（不经过 HTTP/SSE），
// 事件由 Go 端以 "cw:event"（与共享事件总线同构的 type/data）推送。
import * as wails from "/wails/runtime.js";

const SVC = "copywhere/internal/wailsui.Service";
const call = (method, ...args) => wails.Call.ByName(`${SVC}.${method}`, ...args);

const $ = (id) => document.getElementById(id);
const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
const fmtBytes = (n) => {
  if (n == null || Number.isNaN(n)) return "—";
  if (n >= 1 << 30) return (n / (1 << 30)).toFixed(2) + " GB";
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB";
  if (n >= 1 << 10) return (n / (1 << 10)).toFixed(1) + " KB";
  return n + " B";
};
const errText = (e) => {
  const msg = e && e.message ? e.message : String(e ?? "未知错误");
  const m = /returned an error: (.+)$/i.exec(msg);
  return m ? m[1] : msg;
};
// Wails v3 把文件选择器的「用户取消」也当作错误返回
// （Invalid dialog call: Dialog.OpenFile failed: ... cancelled by user）。
// 取消属于正常操作：识别出来后静默返回，不弹错误提示。
// 只匹配取消文案本身，避免把「context canceled」一类的真实故障一起吞掉。
const isDialogCancelled = (e) => /cancel+ed by user/i.test(errText(e));

const WATCH_LABEL = { listener: "事件驱动", viewer: "轮询", none: "未启用" };
const IMG_EXT = /\.(png|jpe?g|gif|bmp|webp)$/i;
const FILE_EXT_RE = /\.[a-z][a-z0-9]{0,7}$/i;
const ABS_PATH_RE = /^(?:[a-z]:[\\/]|\\\\|\/)/i;
const THEMES = ["auto", "light", "dark"];

// 图片缩略图缓存：记录本身只带路径，简约面板重绘时不应反复走 binding
// 读盘 + base64。值为 data URL；失败缓存空串，避免同一张坏图反复重试。
const imgPreviewCache = new Map();

// storeGet / storeSet：localStorage 在 WebView 里可能直接抛异常（存储被禁用、
// 配额已满）。模块求值阶段抛出去会让整个 app.js 停在原地——boot() 都不会执行，
// 用户看到的是白窗口。主题与界面模式这两处求值期/启动期的读写必须走这里；
// 传输记录历史的读写另有自己的一层 try/catch。
function storeGet(key, fallback) {
  try {
    const v = localStorage.getItem(key);
    return v === null ? fallback : v;
  } catch (e) {
    return fallback;
  }
}

function storeSet(key, value) {
  try {
    localStorage.setItem(key, value);
    return true;
  } catch (e) {
    return false;
  }
}

// pairingPeers：正在等对端确认的节点 id。在线节点快照每 2 秒到达一次并重建
// 按钮，这个状态必须活在 DOM 之外，否则 disabled/"…" 会被下一次渲染抹掉，
// 用户在对端还没确认时能重复点，对端会收到重复的配对请求。
const pairingPeers = new Set();

function isTextRecord(r) {
  if (!r) return false;
  if (typeof r.text === "boolean") return r.text;
  // 旧版持久化记录没有 text 字段：接收文件的 detail 是落盘绝对路径，
  // 文件记录的 name 一定带扩展名；两者都不像才是文本。
  if (ABS_PATH_RE.test(String(r.detail || ""))) return false;
  if (FILE_EXT_RE.test(String(r.name || ""))) return false;
  return true;
}

// recordPath 返回记录对应的本机路径：接收记录落盘在接收目录，
// 发送记录指向用户原始文件（旧记录没有 path 字段时退回 detail）。
function recordPath(r) {
  if (!r) return "";
  if (r.path) return r.path;
  const detail = String(r.detail || "");
  if (!isTextRecord(r) && ABS_PATH_RE.test(detail)) return detail;
  return "";
}

// previewImage 读取图片缩略图：接收记录限定接收目录（ImagePreview），
// 发送记录读用户本机文件（LocalImagePreview），两边都可能出现在时间线里。
function previewImage(r, path) {
  if (!path) return Promise.reject(new Error("没有可预览的路径"));
  if (imgPreviewCache.has(path)) {
    const cached = imgPreviewCache.get(path);
    return cached ? Promise.resolve(cached) : Promise.reject(new Error("图片预览不可用"));
  }
  // 两个方法各写一条字面量调用：静态检查（frontend_wiring_test.go）只匹配
  // 直接写字面量方法名的调用，拼成变量后改名/误删就检查不出来了。
  function cachePreview(src) {
    if (imgPreviewCache.size > 80) imgPreviewCache.clear();
    imgPreviewCache.set(path, src || "");
    if (!src) throw new Error("图片预览不可用");
    return src;
  }

  function cacheFailed(e) {
    imgPreviewCache.set(path, "");
    throw e;
  }

  if (r.in) {
    return call("ImagePreview", path).then(cachePreview, cacheFailed);
  }
  return call("LocalImagePreview", path).then(cachePreview, cacheFailed);
}

function copyRecordText(r) {
  const text = (r && (r.detail || r.name)) || "";
  return call("CopyText", text)
    .then(() => toast("已复制到本机剪贴板"))
    .catch((e) => toast("复制失败: " + errText(e), true));
}

// 传输记录持久化：与旧浏览器面板同名（cw-feed），但 WebView2 与浏览器是两个
// 独立的 origin/存储，旧面板的记录不会自动出现在这里（详见 README 已知限制）。
const FEED_KEY = "cw-feed";
const FEED_MAX = 400; // 最多保留的条数
const FEED_TEXT_MAX = 2000; // 单条文本内容预览的最大字符数
const FEED_WRITE_DELAY = 400; // 写盘去抖（ms）

const app = {
  state: null,
  peers: [],
  feed: [],
  recordsSeen: new Set(),
  settings: null,
  paired: [],
  info: null,
  logs: [],
  logOpen: false,
  unread: 0,
  theme: storeGet("cw-theme", "dark"),
  pendingPair: null,
  confirmFn: null,
  dragName: "",
  lastDate: "",
  feedTimer: 0,
  // 界面模式由 Go 端窗口控制器持有（wails-window.json），前端只缓存快照。
  ui: { mode: "standard", collapsed: false, edge: "right", alwaysOnTop: false },
  miniUnread: 0,
  miniFeedSig: null,
  miniStick: true, // 简约时间线是否跟随到底部（用户向上翻看时置 false）
  dockDragged: false, // 本次按住吸边图标是否已拖动过（拖动松手后不应再展开面板）
};

// ---------------------------------------------------------------- 启动

async function boot() {
  applyTheme(app.theme);
  bindStaticUI();
  bindWailsEvents();
  await Promise.allSettled([loadState(), loadPeers(), loadRecords(), loadLogs(), loadPendingPair(), loadUiState()]);
  try {
    await call("UiReady", `${navigator.userAgent} / ${window.innerWidth}x${window.innerHeight}`);
  } catch (e) {
    console.error("UiReady 调用失败", e);
  }
  autoSizeComposer();
  autoSizeMiniComposer();
}

// loadUiState 读取窗口界面模式（标准 / 简约）。窗口尺寸已由 Go 端按同一份状态
// 创建好，这里只需要把前端外壳切到对应布局。
async function loadUiState() {
  const ui = await call("UiState");
  applyUiState(ui);
}

async function loadPendingPair() {
  try {
    const p = await call("PendingPair");
    if (p && p.id) showPairRequest(p);
  } catch (e) {
    console.error("读取待裁决配对请求失败", e);
  }
}

function bindWailsEvents() {
  wails.Events.On("cw:event", (e) => {
    const ev = e && e.data ? e.data : e;
    if (!ev || !ev.type) return;
    handleEvent(ev.type, ev.data);
  });
  wails.Events.On("cw:toast", (e) => {
    const d = e && e.data ? e.data : e;
    if (d && d.message) toast(d.message, !!d.error);
  });
  // 窗口模式/置顶/吸边收起由 Go 端改变（托盘、拖动吸边、右键菜单）时推送。
  wails.Events.On("cw:ui", (e) => {
    const ui = e && e.data ? e.data : e;
    applyUiState(ui);
  });
}

function handleEvent(type, data) {
  switch (type) {
    case "log":
      pushLog(data && data.line ? data.line : "");
      break;
    case "file":
      addRecord(data);
      break;
    case "progress":
      applyProgress(data);
      break;
    case "pair_request":
      showPairRequest(data);
      break;
    case "peers":
      app.peers = Array.isArray(data) ? data : [];
      renderPeers();
      break;
    case "state":
      applyState(data);
      break;
    default:
      break;
  }
}

// ---------------------------------------------------------------- 数据加载

async function loadState() {
  const st = await call("State");
  applyState(st);
}

async function loadPeers() {
  const peers = await call("Peers");
  app.peers = Array.isArray(peers) ? peers : [];
  renderPeers();
}

async function loadRecords() {
  const records = await call("Records");
  const list = Array.isArray(records) ? records : [];
  app.feed = [];
  app.recordsSeen.clear();
  app.lastDate = "";
  // 先铺从 .copywhere/feed.json 读到的历史；若存在旧版 localStorage 记录则合并迁移
  for (const r of list) addRecord(r, true);
  const legacy = loadPersistedFeed();
  if (legacy.length > 0) {
    for (const r of legacy) addRecord(r, true);
    schedulePersistFeed();
    try {
      localStorage.removeItem(FEED_KEY);
    } catch (e) {}
  }
  renderSub();
}

async function loadLogs() {
  const lines = await call("Logs");
  app.logs = (Array.isArray(lines) ? lines : []).slice(-1000);
  if (app.logOpen) renderLogs();
  updateLogBadge();
}

// ---------- 传输记录持久化（~/.copywhere/feed.json） ----------

// slimFeed 生成可写进 .copywhere/feed.json 的精简记录：去掉渲染用字段，
// 文本内容截断，条数取最近 FEED_MAX 条。
// text/path 必须一起持久化：丢了 text 会让文本记录在下次启动后显示成文件卡片。
function slimFeed() {
  return app.feed.slice(-FEED_MAX).map((r) => ({
    date: r.date || "",
    time: r.time || "",
    in: !!r.in,
    peer: r.peer || "",
    name: r.name || "",
    size: r.size || 0,
    status: r.status || "",
    detail: String(r.detail || "").slice(0, FEED_TEXT_MAX),
    path: r.path || "",
    text: !!r.text,
  }));
}

// loadPersistedFeed 读取旧版 localStorage 遗留的历史记录（用于平滑迁移到 .copywhere/feed.json）。
function loadPersistedFeed() {
  try {
    const raw = localStorage.getItem(FEED_KEY);
    if (!raw) return [];
    const list = JSON.parse(raw);
    return Array.isArray(list) ? list : [];
  } catch (e) {
    console.warn("读取旧版历史传输记录失败", e);
    return [];
  }
}

// schedulePersistFeed 去抖写入：新纪录密集到达时合并成一次写，落盘到 ~/.copywhere/feed.json。
function schedulePersistFeed() {
  if (app.feedTimer) clearTimeout(app.feedTimer);
  app.feedTimer = setTimeout(() => {
    app.feedTimer = 0;
    try {
      const slim = slimFeed();
      call("SaveFeed", slim).catch((e) => {
        console.warn("保存传输记录到 .copywhere 失败", e);
      });
    } catch (e) {
      console.warn("保存传输记录失败", e);
    }
  }, FEED_WRITE_DELAY);
}

// clearPersistedFeed 清空 ~/.copywhere/feed.json 里的历史（「清空显示」时调用）。
function clearPersistedFeed() {
  if (app.feedTimer) {
    clearTimeout(app.feedTimer);
    app.feedTimer = 0;
  }
  call("ClearFeed").catch((e) => {
    console.warn("清空 .copywhere 传输记录失败", e);
  });
  try {
    localStorage.removeItem(FEED_KEY);
  } catch (e) {
    console.warn("清空旧版 localStorage 传输记录失败", e);
  }
}

// ---------------------------------------------------------------- 界面模式（标准 / 简约）

const MINI_FEED_MAX = 40;

// applyUiState 把 Go 端窗口状态同步到外壳：html[data-ui-mode] 决定标准/简约，
// data-mini-collapsed 决定显示面板还是屏幕边缘的小图标。
function applyUiState(ui) {
  if (!ui || (ui.mode !== "mini" && ui.mode !== "standard")) return;
  const prev = app.ui || {};
  app.ui = ui;
  const mini = ui.mode === "mini";
  document.documentElement.setAttribute("data-ui-mode", mini ? "mini" : "standard");
  document.documentElement.setAttribute("data-mini-collapsed", ui.collapsed ? "1" : "0");
  document.documentElement.setAttribute("data-mini-edge", ui.edge || "right");
  try {
    localStorage.setItem("cw-ui-mode", ui.mode);
    localStorage.setItem("cw-ui-collapsed", ui.collapsed ? "1" : "0");
  } catch (e) {
    /* 存储不可用：只影响下次启动的首帧，不影响功能 */
  }

  const pin = $("btnMiniPin");
  if (pin) {
    pin.classList.toggle("on", !!ui.alwaysOnTop);
    pin.setAttribute("aria-pressed", String(!!ui.alwaysOnTop));
    pin.title = ui.alwaysOnTop ? "取消置顶" : "固定在屏幕最上层";
  }

  if (!mini) return; // 标准界面的内容一直是活的，不需要重绘
  syncMini();
  if (ui.collapsed) {
    // 收起到 64x64 后弹层几乎无法操作：日志抽屉先关掉；配对确认会先展开面板。
    if (app.logOpen) closeLogs();
  } else {
    clearMiniBadge();
    if (prev.collapsed) {
      const input = $("miniInput");
      if (input) {
        autoSizeMiniComposer();
        input.focus();
      }
    }
  }
}

// syncMini 只在简约面板可见时重绘（收起时窗口只有 64x64，画了也看不见）。
function syncMini() {
  if (!app.ui || app.ui.mode !== "mini" || app.ui.collapsed) return;
  renderMini();
}

function renderMini() {
  const st = app.state || {};
  const name = st.nodeName || "—";
  $("miniAvatar").textContent = initialOf(name);
  $("miniName").textContent = name;
  $("miniName").title = name;
  $("miniSub").textContent = st.selfIP || "—";
  $("miniSub").title = st.selfIP || "";
  const paused = !!st.paused;
  $("miniDot").classList.toggle("on", !paused);
  $("miniSync").textContent = paused ? "已暂停" : "同步中";
  $("miniCount").textContent = `在线 ${app.peers.length}`;
  $("btnMiniPause").textContent = paused ? "恢复" : "暂停";
  $("btnMiniPause").title = paused ? "恢复自动同步" : "暂停自动同步";
  renderMiniPeers();
  renderMiniXfers();
  renderMiniFeed();
}

// renderMiniPeers 只列「未配对」节点：简约面板顶部不显示具体配对设备信息
// （在线数量由状态条给出），但保留配对入口，否则窄面板里无法完成首次配对。
function renderMiniPeers() {
  const box = $("miniPeers");
  if (!box) return;
  box.innerHTML = "";
  const pending = (app.peers || []).filter((p) => !p.paired);
  if (!pending.length) return; // 空节点由 CSS :empty 整条隐藏
  for (const p of pending) {
    const chip = document.createElement("span");
    chip.className = "mini-peer";
    const ip = ((p.ips || [])[0] || "—");
    chip.title = `${p.name || p.id} · ${ip} · 未配对`;

    const dot = document.createElement("span");
    dot.className = "mini-peer-dot" + (p.blocked ? " err" : "");
    const nm = document.createElement("span");
    nm.className = "mini-peer-nm";
    nm.textContent = p.name || p.id;
    chip.appendChild(dot);
    chip.appendChild(nm);

    const b = document.createElement("button");
    b.className = "mini-peer-btn";
    const busy = pairingPeers.has(p.id); // 重建后仍要显示"等待确认"
    b.disabled = busy;
    b.textContent = busy ? "…" : "配对";
    b.onclick = async (e) => {
      e.stopPropagation();
      if (pairingPeers.has(p.id)) return;
      pairingPeers.add(p.id);
      b.disabled = true;
      b.textContent = "…";
      try {
        await call("Pair", p.id);
        toast(`已与 ${p.name} 完成配对`);
        // 配对成功后立刻刷新节点表：否则这条"未配对"记录要等下一次 2s 快照
        // 才消失，期间按钮会恢复成可点的「配对」。
        await loadPeers().catch(() => {});
      } catch (err) {
        toast("配对失败: " + errText(err), true);
      } finally {
        // 先删在途标记再重建，否则重绘出来的按钮仍然是"…/disabled"。
        pairingPeers.delete(p.id);
        renderMiniPeers();
      }
    };
    chip.appendChild(b);
    box.appendChild(chip);
  }
}

// renderMiniXfers 把进行中的传输渲染成窄面板顶部的进度条：
// 标准工作台的 xferStrip 在简约模式下不可见，大文件传输没有百分比会像卡住。
function renderMiniXfers() {
  const box = $("miniXfer");
  if (!box) return;
  // 进度事件每 256KB 推一次（1GB ≈ 4000 次）。标准界面里 #miniXfer 是
  // display:none，收起到图标时也看不见——这两种情况直接清空返回，不做重绘。
  if (!app.ui || app.ui.mode !== "mini" || app.ui.collapsed) {
    if (!box.hidden || box.childElementCount) {
      box.hidden = true;
      box.innerHTML = "";
    }
    return;
  }
  const feed = $("miniFeed");
  const list = [...xfers.values()].filter((x) => x.total > 0);
  box.hidden = !list.length;
  box.innerHTML = "";
  for (const x of list) {
    const pct = Math.min(100, (x.sent / x.total) * 100);
    const row = document.createElement("div");
    row.className = "mini-xfer" + (x.in ? " in" : " out");
    row.title = `${x.name} · ${fmtBytes(x.sent)} / ${fmtBytes(x.total)}`;
    row.innerHTML = `
      <div class="mini-xfer-head">
        <span class="mini-xfer-dir">${x.in ? "←" : "→"}</span>
        <b class="mini-xfer-name">${esc(x.name)}</b>
        <span class="mini-xfer-pct">${pct.toFixed(0)}%</span>
      </div>
      <div class="mini-xfer-bar"><i style="width:${pct}%"></i></div>`;
    box.appendChild(row);
  }
  // 进度条出现/消失会改变时间线高度；原本停在底部时继续贴底，别把最新一条挤掉。
  if (feed && app.miniStick) requestAnimationFrame(() => (feed.scrollTop = feed.scrollHeight));
}

function miniNearBottom() {
  const box = $("miniFeed");
  return !box || box.scrollHeight - box.scrollTop - box.clientHeight < 40;
}

function renderMiniFeed() {
  const box = $("miniFeed");
  if (!box) return;
  const list = app.feed.slice(-MINI_FEED_MAX);
  // 状态快照每 2 秒到达一次：内容没变就不重建 DOM，避免图片反复走 binding 读盘。
  const sig = list.map((r) => r.key || [r.date, r.time, r.in, r.name, r.detail].join("|")).join("~");
  if (sig === app.miniFeedSig && box.childElementCount) return;
  app.miniFeedSig = sig;

  const near = miniNearBottom();
  app.miniStick = near;
  // 用户向上翻看历史时（near=false），重建 DOM 会把 scrollTop 归零，
  // 看起来像"来一条新消息就跳到最顶上"。这里先把滚动位置存下来再还原。
  const keepTop = near ? 0 : box.scrollTop;
  box.innerHTML = "";
  if (!list.length) {
    const d = document.createElement("div");
    d.className = "mini-empty-feed";
    d.innerHTML = "还没有传输记录<br>复制文本 / 文件即自动同步到在线节点";
    box.appendChild(d);
    return;
  }
  for (const r of list) box.appendChild(miniRow(r));
  box.scrollTop = near ? box.scrollHeight : keepTop;
}

// miniRow 是简约面板的 IM 式消息行：接收靠左、发送靠右，
// 上方是名字 + 时间，下方是文本 / 图片 / 文件气泡。
function miniRow(r) {
  const text = !!r.text;
  const failed = String(r.status || "").includes("失败");
  const path = recordPath(r);
  const isImage = !text && !!path && IMG_EXT.test(r.name || "");
  const row = document.createElement("div");
  row.className = `mini-row ${r.in ? "in" : "out"}${failed ? " fail" : ""}`;
  const who = r.in ? r.peer || "未知节点" : (app.state && app.state.nodeName) || "本机";

  const msg = document.createElement("div");
  msg.className = "mini-msg";

  const head = document.createElement("div");
  head.className = "mini-who";
  const nm = document.createElement("span");
  nm.className = "mini-who-name";
  nm.textContent = r.in ? who : "我";
  nm.title = who;
  const tm = document.createElement("span");
  tm.className = "mini-who-time";
  tm.textContent = r.time || "";
  if (r.in) head.append(nm, tm);
  else head.append(tm, nm);
  msg.appendChild(head);

  if (text) msg.appendChild(miniTextBubble(r));
  else if (isImage) msg.appendChild(miniImageBubble(r, path));
  else msg.appendChild(miniFileBubble(r, path));

  if (failed) {
    const f = document.createElement("span");
    f.className = "mini-fail";
    f.textContent = r.status || "失败";
    msg.appendChild(f);
  }

  if (text) {
    msg.title = "双击复制文本";
    msg.addEventListener("dblclick", (e) => {
      e.preventDefault();
      void copyRecordText(r);
    });
  } else if (path && !isImage) {
    msg.title = "双击打开文件位置";
    msg.addEventListener("dblclick", (e) => {
      e.preventDefault();
      openPath(path, true);
    });
  }
  row.appendChild(msg);
  return row;
}

function miniTextBubble(r) {
  const b = document.createElement("div");
  b.className = "mini-bubble text";
  b.textContent = r.detail || r.name || "（空文本）";
  b.title = "点击展开 / 收起；双击复制";
  b.addEventListener("click", (e) => {
    if (e.detail > 1) return; // 双击只负责复制，不来回折叠
    b.classList.toggle("open");
  });
  return b;
}

function miniFileBubble(r, path) {
  const { cls, label } = fileBadge(r.name);
  const b = document.createElement("div");
  b.className = "mini-bubble file" + (path ? " clickable" : "");
  b.innerHTML = `
    <span class="mini-fico ${cls}">${esc(label)}</span>
    <span class="mini-finfo">
      <b class="mini-fname" title="${esc(r.name)}">${esc(r.name || "（无名称）")}</b>
      <small class="mini-fmeta">${esc(fmtBytes(r.size))}${r.status ? " · " + esc(r.status) : ""}</small>
    </span>`;
  return b;
}

function miniImageBubble(r, path) {
  const wrap = document.createElement("div");
  wrap.className = "mini-bubble img";
  const img = document.createElement("img");
  img.className = "mini-img";
  img.alt = r.name || "图片";
  img.title = "点击放大；双击打开文件位置";
  const feedBox = $("miniFeed");
  let zoomTimer = 0;
  img.addEventListener("click", (e) => {
    e.stopPropagation();
    if (e.detail > 1) return;
    zoomTimer = setTimeout(() => showLightbox(img.src, r.name, path), 180);
  });
  img.addEventListener("dblclick", (e) => {
    e.preventDefault();
    e.stopPropagation();
    clearTimeout(zoomTimer);
    openPath(path, true);
  });
  img.addEventListener("load", () => {
    if (app.miniStick) feedBox.scrollTop = feedBox.scrollHeight;
  });
  const fail = () => {
    if (img.parentNode !== wrap) return;
    wrap.replaceWith(miniFileBubble(r, path));
  };
  img.onerror = fail;
  previewImage(r, path)
    .then((src) => {
      img.src = src;
    })
    .catch(fail);
  wrap.appendChild(img);
  return wrap;
}

function bumpMiniBadge(isErr) {
  app.miniUnread = Math.min(99, app.miniUnread + 1);
  const b = $("miniDockBadge");
  if (!b) return;
  b.hidden = false;
  // 错误标记只加不清：后面来的普通记录不应该把"有错误未查看"的标记抹掉，
  // 只有展开面板（clearMiniBadge）才算看过。
  if (isErr) b.classList.add("err");
  b.textContent = app.miniUnread > 9 ? "9+" : String(app.miniUnread);
}

function clearMiniBadge() {
  app.miniUnread = 0;
  const b = $("miniDockBadge");
  if (!b) return;
  b.hidden = true;
  b.classList.remove("err");
}

async function setUiMode(mode) {
  try {
    const ui = await call("SetUiMode", mode);
    applyUiState(ui);
    toast(mode === "mini" ? "简约界面：拖到屏幕左右边缘会收起成图标" : "已回到标准界面");
  } catch (e) {
    toast("切换界面失败: " + errText(e), true);
  }
}

function toggleUiMode() {
  return setUiMode(app.ui && app.ui.mode === "mini" ? "standard" : "mini");
}

async function toggleMiniPin() {
  const on = !(app.ui && app.ui.alwaysOnTop);
  try {
    applyUiState(await call("SetAlwaysOnTop", on));
    toast(on ? "已固定在屏幕最上层" : "已取消置顶");
  } catch (e) {
    toast("切换置顶失败: " + errText(e), true);
  }
}

async function collapseMini() {
  try {
    applyUiState(await call("SetCollapsed", true));
  } catch (e) {
    toast("收起失败: " + errText(e), true);
  }
}

async function expandMini() {
  try {
    applyUiState(await call("SetCollapsed", false));
  } catch (e) {
    toast("展开失败: " + errText(e), true);
  }
}

// expandFromDock 是吸边图标的点击行为：刚拖动过的那次手势不当作点击，
// 否则"拖完图标"会顺手把面板展开。
function expandFromDock() {
  if (app.dockDragged) {
    app.dockDragged = false;
    return;
  }
  expandMini();
}

// bindMiniDockDrag 让吸边图标可拖动：按住移动超过阈值后交给 Go 端进入系统
// 原生拖动循环（窗口跟手、不会一卡一顿），松手后的位置由窗口事件吸到最近一侧
// 并写入 wails-window.json。点击（没有移动）仍然走 click 展开面板。
// 不用 CSS 的 --wails-draggable: drag 是因为 Wails 在指针一移动时就进入拖动并
// 吞掉 click，点击与拖动会互相干扰。
function bindMiniDockDrag(dock) {
  const THRESHOLD = 4; // CSS 像素：小于它算点击，大于它算拖动
  let startX = 0;
  let startY = 0;
  let armed = false;
  let dragStarted = false;

  dock.addEventListener("pointerdown", (e) => {
    if (e.button !== 0) return;
    app.dockDragged = false;
    startX = e.clientX;
    startY = e.clientY;
    armed = true;
    dragStarted = false;
    try {
      dock.setPointerCapture(e.pointerId);
    } catch (err) {
      /* 指针捕获失败也不影响后面的阈值判断 */
    }
  });

  window.addEventListener("pointermove", (e) => {
    if (!armed || dragStarted) return;
    if (Math.abs(e.clientX - startX) < THRESHOLD && Math.abs(e.clientY - startY) < THRESHOLD) return;
    dragStarted = true;
    app.dockDragged = true;
    call("BeginWindowDrag").catch((err) => {
      // 原生拖动不可用（非 Windows 等）：退回普通点击语义，别把图标"按死"。
      app.dockDragged = false;
      dragStarted = false;
      console.warn("吸边图标拖动不可用", err);
    });
  });

  const endGesture = () => {
    armed = false;
  };
  window.addEventListener("pointerup", endGesture);
  window.addEventListener("pointercancel", endGesture);
}

// ensureMiniExpanded 在需要用户操作的场景（如配对确认）先把面板从图标展开，
// 否则 64x64 的窗口里弹窗根本点不到。
async function ensureMiniExpanded() {
  if (!app.ui || app.ui.mode !== "mini" || !app.ui.collapsed) return;
  try {
    applyUiState(await call("SetCollapsed", false));
  } catch (e) {
    console.warn("展开简约面板失败", e);
  }
}

async function sendMiniText() {
  const input = $("miniInput");
  const text = input.value;
  if (!text.trim()) return;
  const btn = $("btnMiniSend");
  btn.disabled = true;
  try {
    await call("SendText", text);
    input.value = "";
    autoSizeMiniComposer();
    toast("已发送");
  } catch (e) {
    toast("发送失败: " + errText(e), true);
  } finally {
    btn.disabled = false;
  }
}

function autoSizeMiniComposer() {
  const input = $("miniInput");
  if (!input) return;
  input.style.height = "auto";
  const max = 88;
  // textarea 是 border-box：scrollHeight 含 padding 不含 border，补 2px 才不会
  // 因为内容区比内容矮 1px 而默认出现竖向滚动条。
  const h = Math.min(max, input.scrollHeight + 2);
  input.style.height = h + "px";
  input.classList.toggle("scroll", input.scrollHeight + 2 > max);
}

async function hideWindow() {
  try {
    await call("HideWindow");
  } catch (e) {
    toast("收进托盘失败: " + errText(e), true);
  }
}

// ---------------------------------------------------------------- 状态

function applyState(st) {
  if (!st) return;
  app.state = st;
  $("verLabel").textContent = "v" + (st.version || "—");
  $("selfName").textContent = st.nodeName || "—";
  $("selfIP").textContent = st.selfIP || "—";
  $("dockSelfName").textContent = st.nodeName || "—";
  $("dockSelfIP").textContent = st.selfIP || "—";
  $("selfAvatar").textContent = initialOf(st.nodeName);
  $("layoutSelfInitial").textContent = initialOf(st.nodeName);
  $("layoutSelfName").textContent = st.nodeName || "—";
  $("layoutSelfIP").textContent = st.selfIP || "—";
  $("dockSelfMeta").innerHTML = [
    ["阈值", `${st.thresholdMB} MB`],
    ["已配对", String(st.pairedCount ?? 0)],
    ["监听", WATCH_LABEL[st.watchMode] || st.watchMode || "—"],
  ].map(([k, v]) => `<div class="kv"><span>${esc(k)}</span><b>${esc(v)}</b></div>`).join("");

  const paused = !!st.paused;
  $("syncDot").classList.toggle("on", !paused);
  $("syncText").textContent = paused ? "已暂停" : "同步中";
  $("syncChip").title = paused ? "自动同步已暂停（手动发送不受影响）" : "自动同步运行中";
  $("btnPause").classList.toggle("active", paused);
  $("btnPause").title = paused ? "恢复自动同步" : "暂停自动同步";
  $("btnPause").setAttribute("aria-label", paused ? "恢复自动同步" : "暂停自动同步");
  $("statusLeft").textContent = `Wails v3 · WebView2 · ${paused ? "已暂停" : "同步中"}`;
  $("statusRight").textContent = st.configPath ? `配置 ${st.configPath}` : "";
  $("statusRight").title = st.configPath || "";
  $("kvmToggle").checked = !!st.kvmEnabled;

  renderSub();
  renderLayout();
  syncMini();
}

function renderSub() {
  const st = app.state || {};
  const today = localDateStr(new Date());
  const todayCount = app.feed.filter((r) => r.date === today).length;
  $("stageSub").textContent = `在线 ${app.peers.length} · 已配对 ${st.pairedCount || 0} · 今日 ${todayCount} 条`;
  const hint = $("composerHint");
  if (hint) {
    hint.textContent = app.peers.length
      ? `在线 ${app.peers.length} 个节点 · 拖入或复制后 Ctrl+V 即投送`
      : "暂无在线节点，拖入文件会提示原因";
  }
}

function initialOf(name) {
  const s = String(name || "?").trim();
  return s ? s[0].toUpperCase() : "?";
}

// ---------------------------------------------------------------- 设备坞

function renderPeers() {
  const box = $("peerList");
  const empty = $("peerEmpty");
  box.innerHTML = "";
  empty.hidden = app.peers.length > 0;

  for (const p of app.peers) {
    box.appendChild(peerCard(p));
  }
  renderSub();
  renderLayout();
  syncMini();
}

function peerCard(p) {
  const card = document.createElement("div");
  card.className = "dev-card";
  const tags = [];
  if (p.paired) tags.push('<span class="tag ok">已配对</span>');
  else tags.push('<span class="tag mute">未配对</span>');
  if (p.blocked) tags.push('<span class="tag warn">鉴权被拒</span>');
  if (p.side === "left") tags.push('<span class="tag">左邻</span>');
  if (p.side === "right") tags.push('<span class="tag">右邻</span>');

  card.innerHTML = `
    <div class="head">
      <div class="avatar">${esc(initialOf(p.name))}</div>
      <div class="who">
        <div class="nm"><span class="nm-txt" title="${esc(p.name)}">${esc(p.name)}</span></div>
        <div class="sub" title="${esc((p.ips || []).join(", "))}">${esc((p.ips || [])[0] || "—")} · :${p.tcpPort || "—"}</div>
      </div>
      <span class="peer-dot${p.blocked ? " err" : " on"}" title="${p.blocked ? "鉴权被拒" : "在线"}"></span>
    </div>
    <div class="meta">
      ${tags.join("")}
      <span class="sep"></span>
      <span>活跃 ${esc(p.idle || "—")}</span>
      <span class="sep"></span>
      <span>${(p.ips || []).length} 个地址</span>
    </div>
    <div class="acts"></div>`;

  const acts = card.querySelector(".acts");
  if (!p.paired) {
    const b = document.createElement("button");
    b.className = "chipbtn accent";
    b.textContent = "配对";
    b.onclick = async () => {
      b.disabled = true;
      b.textContent = "等待对端确认…";
      try {
        await call("Pair", p.id);
        toast(`已与 ${p.name} 完成配对`);
      } catch (e) {
        toast("配对失败: " + errText(e), true);
      } finally {
        b.disabled = false;
        b.textContent = "配对";
      }
    };
    acts.appendChild(b);
  } else {
    const un = document.createElement("button");
    un.className = "chipbtn";
    un.textContent = "解除";
    un.onclick = () =>
      confirmAction("解除配对", `确定解除与「${p.name}」的配对吗？解除后双方将无法再互相传输。`, "解除", async () => {
        try {
          await call("Unpair", p.id);
          toast(`已解除与 ${p.name} 的配对`);
        } catch (e) {
          toast("解除失败: " + errText(e), true);
        }
      });
    acts.appendChild(un);
  }

  const left = document.createElement("button");
  left.className = "chipbtn";
  left.textContent = "← 左邻";
  left.title = "把该节点配置为本机左邻（光标推向左边缘时控制它）";
  left.onclick = () => setNeighbor("left", p.name);
  const right = document.createElement("button");
  right.className = "chipbtn";
  right.textContent = "右邻 →";
  right.title = "把该节点配置为本机右邻（光标推向右边缘时控制它）";
  right.onclick = () => setNeighbor("right", p.name);
  acts.appendChild(left);
  acts.appendChild(right);

  return card;
}

async function setNeighbor(side, name) {
  try {
    await call("SetKVMNeighbor", side, name);
    toast(`${name || "（空）"} 已设为${side === "left" ? "左" : "右"}邻${name ? "" : "，已清除"}`);
  } catch (e) {
    toast("更新布局失败: " + errText(e), true);
  }
}

// ---------------------------------------------------------------- 时间线

function addRecord(r, silent) {
  if (!r) return;
  // 兼容旧版 localStorage：当时 slimFeed 没有存 text 字段，重启后文本记录
  // 会退化成文件卡片。这里统一归一化，之后的时间线/简约面板都只看 r.text。
  const rec = { ...r, text: isTextRecord(r) };
  const key = [rec.date, rec.time, rec.in ? 1 : 0, rec.peer, rec.name, rec.size, rec.detail].join("|");
  if (app.recordsSeen.has(key)) return;
  app.recordsSeen.add(key);
  app.feed.push({ ...rec, key });

  const timeline = $("timeline");
  $("feedEmpty").hidden = true;
  const near = nearBottom();
  if (rec.date !== app.lastDate) {
    app.lastDate = rec.date;
    timeline.appendChild(dateSeparator(rec.date));
  }
  timeline.appendChild(recordRow(rec));
  renderSub();
  if (!silent) schedulePersistFeed();
  if (near || silent) scrollToBottom();
  // 简约面板可见时增量重绘；收起成图标时只把新记录记到角标上。
  if (app.ui && app.ui.mode === "mini" && app.ui.collapsed) bumpMiniBadge();
  else syncMini();
}

function dateSeparator(date) {
  const d = document.createElement("div");
  d.className = "fdate";
  d.innerHTML = `<span>${esc(dateLabel(date))}</span>`;
  return d;
}

function dateLabel(dateStr) {
  const today = localDateStr(new Date());
  const y = new Date();
  y.setDate(y.getDate() - 1);
  if (dateStr === today) return "今天";
  if (dateStr === localDateStr(y)) return "昨天";
  return dateStr || "";
}

function localDateStr(d) {
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

function nearBottom() {
  const t = $("timeline");
  return t.scrollHeight - t.scrollTop - t.clientHeight < 80;
}

function scrollToBottom() {
  const t = $("timeline");
  t.scrollTop = t.scrollHeight;
}

function recordRow(r) {
  const failed = String(r.status || "").includes("失败");
  const row = document.createElement("div");
  row.className = `chat-row ${r.in ? "in" : "out"}${failed ? " fail" : ""}`;
  const who = r.in ? r.peer || "未知节点" : (app.state && app.state.nodeName) || "本机";

  row.innerHTML = `
    <div class="ava" title="${esc(who)}">${esc(initialOf(who))}</div>
    <div class="msg-col">
      <div class="m-head">
        <span class="m-name" title="${esc(who)}">${esc(who)}</span>
        <span class="m-time">${esc(r.time || "")}</span>
        ${r.in ? "" : `<span class="m-dir" title="发送到 ${esc(r.peer || "")}">→ ${esc(r.peer || "")}</span>`}
      </div>
      <div class="bubble"></div>
    </div>`;

  const bubble = row.querySelector(".bubble");
  const body = renderRecordBody(r);
  bubble.appendChild(body.el);

  const meta = document.createElement("div");
  meta.className = "b-meta";
  const parts = [];
  // 文件卡片本身已显示大小/状态，元信息行不再重复；文本记录显示字节数。
  if (r.text) parts.push(`<span>${fmtBytes(r.size)}</span>`, '<span class="sep"></span><span>文本</span>');
  if (failed && r.text && r.status) parts.push(`<span class="pill fail">${esc(r.status)}</span>`);
  meta.innerHTML = parts.join("");

  if (r.text) {
    const copy = iconButton("复制内容", '<path d="M9 9h11v11H9zM4 15V4h11"/>', () => {
      void copyRecordText(r);
    });
    meta.appendChild(copy);
    // 文本：双击复制（单击仍是展开/收起）。
    row.title = "双击复制文本";
    row.addEventListener("dblclick", (e) => {
      if (e.target.closest("button")) return; // 图标按钮自己的双击不重复触发
      void copyRecordText(r);
    });
  } else if (recordPath(r)) {
    const path = recordPath(r);
    const open = iconButton("打开文件", '<path d="M14 4h6v6M20 4l-9 9M18 13v7H4V6h6"/>', () =>
      openPath(path, false)
    );
    const reveal = iconButton("打开所在位置", '<path d="M3 6h6l2 2h10v11H3z"/>', () =>
      openPath(path, true)
    );
    meta.appendChild(open);
    meta.appendChild(reveal);
    row.classList.add("act");
    row.title = "双击打开文件位置";
    row.addEventListener("dblclick", (e) => {
      if (e.target.closest("button")) return;
      openPath(path, true);
    });
  }
  if (meta.childNodes.length) bubble.appendChild(meta);
  return row;
}

function renderRecordBody(r) {
  if (r.text) {
    const d = document.createElement("div");
    d.className = "b-text";
    d.textContent = r.detail && r.detail.trim() ? r.detail : r.name;
    d.title = "点击展开 / 收起；双击复制";
    d.onclick = (e) => {
      if (e.detail > 1) return; // 双击只负责复制
      d.classList.toggle("open");
    };
    return { el: d };
  }
  const path = recordPath(r);
  if (path && IMG_EXT.test(r.name || "")) {
    const wrap = document.createElement("div");
    const img = document.createElement("img");
    img.className = "b-img";
    img.alt = r.name || "图片";
    img.title = "点击放大；双击打开文件位置";
    // 图片解码前气泡没有高度，加载完成后若原本贴底则重新贴底。
    const stick = nearBottom();
    img.addEventListener("load", () => {
      if (stick) scrollToBottom();
    });
    const fallback = fileCard(r);
    const fail = () => {
      if (img.parentNode === wrap) {
        img.remove();
        wrap.appendChild(fallback);
      }
    };
    img.onerror = fail;
    img.onclick = () => showLightbox(img.src, r.name, path);
    img.ondblclick = (e) => {
      e.preventDefault();
      e.stopPropagation();
      openPath(path, true);
    };
    wrap.appendChild(img);
    previewImage(r, path)
      .then((src) => {
        img.src = src;
      })
      .catch(fail);
    return { el: wrap };
  }
  return { el: fileCard(r) };
}

function fileCard(r) {
  const { cls, label } = fileBadge(r.name);
  const failed = String(r.status || "").includes("失败");
  const card = document.createElement("div");
  card.className = "fcard";
  card.innerHTML = `
    <div class="fico ${cls}">${esc(label)}</div>
    <div class="finfo">
      <div class="fcard-name" title="${esc(r.name)}">${esc(r.name || "（无名称）")}</div>
      <div class="fcard-line">
        <span>${fmtBytes(r.size)}</span>
        ${failed && r.status ? `<span class="fcard-status">${esc(r.status)}</span>` : ""}
      </div>
    </div>`;
  return card;
}

function fileBadge(name) {
  const m = String(name || "").toLowerCase().match(/\.([a-z0-9]+)$/);
  const ext = m ? m[1] : "";
  const kinds = {
    doc: "doc", docx: "doc", rtf: "doc", wps: "doc",
    xls: "xls", xlsx: "xls", csv: "xls",
    ppt: "ppt", pptx: "ppt",
    pdf: "pdf",
    zip: "zip", rar: "zip", "7z": "zip", tar: "zip", gz: "zip",
    png: "img", jpg: "img", jpeg: "img", gif: "img", bmp: "img", webp: "img", svg: "img",
    mp3: "aud", wav: "aud", flac: "aud", ogg: "aud", m4a: "aud",
    mp4: "vid", mkv: "vid", avi: "vid", mov: "vid", wmv: "vid",
    exe: "exe", msi: "exe", bat: "exe", cmd: "exe",
    txt: "txt", md: "txt", log: "txt",
    js: "cod", ts: "cod", go: "cod", py: "cod", java: "cod", c: "cod", cpp: "cod",
    cs: "cod", rs: "cod", html: "cod", htm: "cod", css: "cod", json: "cod", xml: "cod",
    yml: "cod", yaml: "cod", sql: "cod", sh: "cod", ps1: "cod",
  };
  return { cls: "fico-" + (kinds[ext] || "def"), label: (ext || "file").slice(0, 4).toUpperCase() };
}

function iconButton(title, svgPath, fn) {
  const b = document.createElement("button");
  b.className = "iact";
  b.title = title;
  b.setAttribute("aria-label", title);
  b.innerHTML = `<svg class="ic" viewBox="0 0 24 24" style="width:13px;height:13px" aria-hidden="true">${svgPath}</svg>`;
  b.onclick = (e) => {
    e.stopPropagation();
    fn();
  };
  return b;
}

async function openPath(path, select) {
  try {
    await call("OpenPath", path, !!select);
  } catch (e) {
    toast("打开失败: " + errText(e), true);
  }
}

// ---------------------------------------------------------------- 进度

const xfers = new Map();

function applyProgress(p) {
  if (!p) return;
  const key = (p.in ? "R|" : "S|") + p.peer + "|" + p.name;
  const arrow = p.in ? "←" : "→";
  let x = xfers.get(key);
  // 同一个 key（对端 + 文件名）会被重试/重发复用。上一轮结束时的移除定时器
  // 必须在这里清掉，否则它在 2.5s 后把重启的条目删掉：进度条中途消失、
  // 下一个事件又把它按 0% 建回来。
  if (x && x.timer) {
    clearTimeout(x.timer);
    x.timer = null;
  }

  if (p.sent < 0) {
    if (x) {
      x.el.remove();
      xfers.delete(key);
      renderMiniXfers();
    }
    toast(p.in ? `${p.peer} 发来的 ${p.name} 接收失败` : `${p.name} → ${p.peer} 发送失败`, true);
    return;
  }

  if (!x) {
    const box = document.createElement("div");
    box.className = "xfer";
    box.innerHTML = `
      <div class="cap"><b title="${esc(p.name)}">${esc(p.name)}</b>
        <span>${arrow} ${esc(p.peer)} · <span class="pct"></span> · <span class="amt"></span></span></div>
      <div class="bar"><i></i></div>`;
    const stick = nearBottom();
    $("xferStrip").appendChild(box);
    if (stick) requestAnimationFrame(scrollToBottom);
    x = {
      el: box, bar: box.querySelector("i"), pct: box.querySelector(".pct"), amt: box.querySelector(".amt"),
      timer: null, name: p.name, in: !!p.in, peer: p.peer, sent: 0, total: 0,
    };
    xfers.set(key, x);
  }
  const pct = p.total > 0 ? Math.min(100, (p.sent / p.total) * 100) : 100;
  x.sent = Math.max(0, p.sent);
  x.total = Math.max(0, p.total);
  x.bar.style.width = pct + "%";
  x.pct.textContent = pct.toFixed(0) + "%";
  x.amt.textContent = `${fmtBytes(p.sent)} / ${fmtBytes(p.total)}`;
  renderMiniXfers();
  if (p.total <= 0 || p.sent >= p.total) {
    clearTimeout(x.timer);
    x.timer = setTimeout(() => {
      const cur = xfers.get(key);
      if (cur === x) {
        x.el.remove();
        xfers.delete(key);
        renderMiniXfers();
      }
    }, 2500);
  }
}

// ---------------------------------------------------------------- 屏幕布局

function renderLayout() {
  if (!app.state) return;
  renderSlot("left", app.state.kvmLeft);
  renderSlot("right", app.state.kvmRight);
  $("kvmToggle").checked = !!app.state.kvmEnabled;
  renderLayoutPeers();
}

function renderSlot(side, name) {
  const el = $(side === "left" ? "slotLeft" : "slotRight");
  const target = (app.peers || []).find((p) => p.name === name);
  const offline = !!name && !target;
  el.classList.toggle("empty", !name);
  el.classList.toggle("offline", offline);
  const label = side === "left" ? "左邻 · 推向左边缘控制" : "右邻 · 推向右边缘控制";

  let inner = `<span class="side-label">${label}</span>`;
  if (!name) {
    inner += `<div class="bezel">
        <svg class="ic" viewBox="0 0 24 24"><path d="M12 5v14M5 12h14"/></svg>
        <span class="scr-sub">拖动节点到${side === "left" ? "左" : "右"}侧</span>
      </div><div class="stand"></div><div class="base"></div>`;
  } else {
    inner += `<div class="bezel">
        <button class="scr-x" data-clear="${side}" title="清除该侧邻居" aria-label="清除该侧邻居">
          <svg class="ic" viewBox="0 0 24 24" style="width:11px;height:11px"><path d="M18 6L6 18M6 6l12 12"/></svg>
        </button>
        <span class="initial">${esc(initialOf(name))}</span>
        <span class="scr-name" title="${esc(name)}">${esc(name)}</span>
        <span class="scr-sub">${offline ? "离线 / 未发现" : esc(((target && target.ips) || [])[0] || "")}</span>
      </div><div class="stand"></div><div class="base"></div>`;
  }
  el.innerHTML = inner;
  const clear = el.querySelector("[data-clear]");
  if (clear) clear.onclick = () => setNeighbor(side, "");
}

function renderLayoutPeers() {
  const box = $("layoutPeers");
  box.innerHTML = "";
  if (!app.peers.length) {
    const s = document.createElement("span");
    s.className = "hint";
    s.textContent = "暂无在线节点，对端运行 copywhere 后会自动出现。";
    box.appendChild(s);
    return;
  }
  for (const p of app.peers) {
    const chip = document.createElement("div");
    chip.className = "dev-chip";
    chip.draggable = true;
    chip.dataset.name = p.name;
    const sideTag = p.side === "left" ? '<span class="chip-side">左邻</span>' : p.side === "right" ? '<span class="chip-side">右邻</span>' : "";
    chip.innerHTML = `<span class="nm" title="${esc(p.name)}">${esc(p.name)}</span>${sideTag}
      <button class="mini" data-side="left" title="设为左邻">左</button>
      <button class="mini" data-side="right" title="设为右邻">右</button>`;
    chip.addEventListener("dragstart", (e) => {
      app.dragName = p.name;
      chip.classList.add("dragging");
      e.dataTransfer.effectAllowed = "move";
      e.dataTransfer.setData("text/plain", p.name);
    });
    chip.addEventListener("dragend", () => {
      chip.classList.remove("dragging");
      app.dragName = "";
    });
    chip.querySelectorAll(".mini").forEach((b) => {
      b.onclick = () => setNeighbor(b.dataset.side, p.name);
    });
    box.appendChild(chip);
  }
}

function bindLayoutDrop() {
  for (const [side, id] of [["left", "slotLeft"], ["right", "slotRight"]]) {
    const el = $(id);
    el.addEventListener("dragover", (e) => {
      e.preventDefault();
      e.dataTransfer.dropEffect = "move";
      el.classList.add("drop-active");
    });
    el.addEventListener("dragleave", () => el.classList.remove("drop-active"));
    el.addEventListener("drop", (e) => {
      e.preventDefault();
      el.classList.remove("drop-active");
      const name = e.dataTransfer.getData("text/plain") || app.dragName;
      if (name) setNeighbor(side, name);
    });
  }
}

// ---------------------------------------------------------------- 输入与投送

function autoSizeComposer() {
  const input = $("composerInput");
  input.style.height = "auto";
  input.style.height = Math.min(140, input.scrollHeight) + "px";
}

async function sendText() {
  const input = $("composerInput");
  const text = input.value;
  if (!text.trim()) return;
  const btn = $("btnSend");
  btn.disabled = true;
  try {
    await call("SendText", text);
    input.value = "";
    autoSizeComposer();
    toast("已发送");
  } catch (e) {
    toast("发送失败: " + errText(e), true);
  } finally {
    btn.disabled = false;
  }
}

async function pickFiles() {
  let picked;
  try {
    picked = await wails.Dialogs.OpenFile({
      Title: "选择要发送的文件或文件夹",
      CanChooseFiles: true,
      CanChooseDirectories: true,
      AllowsMultipleSelection: true,
    });
  } catch (e) {
    if (isDialogCancelled(e)) return; // 正常取消：静默处理
    toast("打开文件选择器失败: " + errText(e), true);
    return;
  }
  const paths = Array.isArray(picked) ? picked : picked ? [picked] : [];
  if (!paths.length) return;
  try {
    await call("SendFiles", paths);
    toast(`已开始投送 ${paths.length} 项内容`);
  } catch (e) {
    toast("投送失败: " + errText(e), true);
  }
}

// pasteFilesFromClipboard 读取剪贴板里的文件并立即投送（输入框 focus 时 Ctrl+V）。
// 网页拿不到文件的真实路径，路径由 Go 端读 CF_HDROP 取得，与拖放同一条链路。
// in-flight 保护：连按 Ctrl+V 只投送一次，避免同一批文件被重复传输。
let pasteInFlight = false;
async function pasteFilesFromClipboard() {
  if (pasteInFlight) return;
  pasteInFlight = true;
  try {
    const n = await call("PasteFiles");
    if (n > 0) toast(`已开始投送 ${n} 项内容`);
  } catch (e) {
    toast("粘贴投送失败: " + errText(e), true);
  } finally {
    pasteInFlight = false;
  }
}

// handleComposerPaste 是两个输入框（标准 / 简约）共用的粘贴处理：
// 剪贴板里带文本就交给浏览器默认粘贴（保留光标与撤销栈），只有"文件"
// 才被接管成一次投送，真实路径由 Go 端读 CF_HDROP 取得。
function handleComposerPaste(e) {
  const dt = e.clipboardData;
  if (!dt) return;
  const types = Array.from(dt.types || []);
  const hasText = types.includes("text/plain") || types.includes("text/html") || !!dt.getData("text/plain");
  if (hasText) return;
  const hasFiles = types.includes("Files") || (dt.files && dt.files.length > 0);
  if (!hasFiles && types.length > 0) return;
  e.preventDefault();
  void pasteFilesFromClipboard();
}

async function togglePause() {
  const next = !(app.state && app.state.paused);
  try {
    await call("SetPaused", next);
    toast(next ? "已暂停自动同步（手动发送不受影响）" : "已恢复自动同步");
  } catch (e) {
    toast("切换失败: " + errText(e), true);
  }
}

// ---------------------------------------------------------------- 配对

async function showPairRequest(p) {
  if (!p || !p.id) return;
  // 配对确认必须点得到：简约界面若已收起到边缘，先把面板展开。
  await ensureMiniExpanded();
  app.pendingPair = p;
  $("pairText").textContent = `「${p.name || p.id}」请求与本机配对。接受后双方即可互相收发内容。`;
  $("pairBackdrop").hidden = false;
  $("btnPairAccept").focus();
}

async function respondPair(accept) {
  const p = app.pendingPair;
  if (!p) {
    $("pairBackdrop").hidden = true;
    return;
  }
  $("btnPairAccept").disabled = true;
  $("btnPairDecline").disabled = true;
  try {
    await call("RespondPair", accept);
    toast(accept ? `已接受 ${p.name} 的配对` : `已拒绝 ${p.name} 的配对`);
  } catch (e) {
    toast("处理配对请求失败: " + errText(e), true);
  } finally {
    $("btnPairAccept").disabled = false;
    $("btnPairDecline").disabled = false;
    $("pairBackdrop").hidden = true;
    app.pendingPair = null;
  }
}

// ---------------------------------------------------------------- 设置

async function openSettings() {
  try {
    const [s, paired, info] = await Promise.all([call("Settings"), call("Paired"), call("Info")]);
    app.settings = s;
    app.paired = Array.isArray(paired) ? paired : [];
    app.info = info;
    fillSettings(s);
    renderPaired();
    fillAbout();
    $("settingsBackdrop").hidden = false;
    $("btnCloseSettings").focus();
  } catch (e) {
    toast("打开设置失败: " + errText(e), true);
  }
}

function fillSettings(s) {
  if (!s) return;
  $("cfgNodeName").value = s.nodeName || "";
  $("cfgReceiveDir").value = s.receiveDir || "";
  $("cfgThreshold").value = s.maxAutoCopyMB ?? 0;
  $("cfgAutoPaste").checked = !!s.autoPaste;
  $("cfgTextSync").checked = !!s.textSync;
  $("cfgShellMenu").checked = !!s.shellMenu;
  $("cfgKvmEnabled").checked = !!s.kvmEnabled;
  $("cfgKvmTouchpad").checked = !!s.kvmTouchpad;
  $("cfgTouchSpeed").value = s.kvmTouchSpeed ?? 100;
  $("cfgSpeedPct").value = s.kvmSpeedPct ?? 100;
  $("cfgMoveMs").value = s.kvmMoveMs ?? 0;
  $("cfgReflowMs").value = s.kvmReflowMs ?? 0;
  $("cfgEdgeDZ").value = (s.kvmEdgeDZ ?? -1) < 0 ? 48 : s.kvmEdgeDZ;
  $("settingsHint").textContent = "修改后立即保存并生效，无需重启";
}

function fillAbout() {
  const st = app.state || {};
  const info = app.info || {};
  $("aboutVersion").textContent = info.version || st.version || "—";
  $("aboutConfig").textContent = info.configPath || st.configPath || "—";
  $("aboutReceiveDir").textContent = info.receiveDir || st.receiveDir || "—";
  $("aboutPorts").textContent = app.settings
    ? `发现 ${app.settings.discoveryPort}/udp · 传输 ${app.settings.transferPort}/tcp · KVM ${app.settings.kvmPort}/tcp`
    : "—";
  $("aboutWatch").textContent = WATCH_LABEL[st.watchMode] || st.watchMode || "—";
  $("aboutMode").textContent = info.panelMode || "Wails v3";
}

function renderPaired() {
  const box = $("pairedList");
  box.innerHTML = "";
  if (!app.paired.length) {
    box.innerHTML = '<div class="paired-empty">还没有已配对的设备。在左侧设备坞点击「配对」，对端确认后即完成。</div>';
    return;
  }
  for (const d of app.paired) {
    const row = document.createElement("div");
    row.className = "paired-row";
    row.innerHTML = `<span class="nm">${esc(d.name || d.id)}</span>
      <span class="sub">配对于 ${esc(d.pairedAt || "")}</span>
      <span class="grow"></span>`;
    const un = document.createElement("button");
    un.className = "chipbtn";
    un.textContent = "解除";
    un.onclick = () =>
      confirmAction("解除配对", `确定解除与「${d.name || d.id}」的配对吗？`, "解除", async () => {
        try {
          await call("Unpair", d.id);
          toast(`已解除与 ${d.name || d.id} 的配对`);
          app.paired = (await call("Paired")) || [];
          renderPaired();
        } catch (e) {
          toast("解除失败: " + errText(e), true);
        }
      });
    row.appendChild(un);
    box.appendChild(row);
  }
}

async function saveSettings() {
  const s = app.settings || {};
  const payload = {};
  const num = (id) => Number($(id).value);
  const setIf = (key, val, old) => {
    if (val !== old) payload[key] = val;
  };
  const nodeName = $("cfgNodeName").value.trim();
  const receiveDir = $("cfgReceiveDir").value.trim();
  if (!nodeName) {
    toast("节点名不能为空", true);
    return;
  }
  if (!receiveDir) {
    toast("接收目录不能为空", true);
    return;
  }
  setIf("node_name", nodeName, s.nodeName);
  setIf("receive_dir", receiveDir, s.receiveDir);
  setIf("max_auto_copy_mb", num("cfgThreshold"), s.maxAutoCopyMB);
  setIf("auto_paste", $("cfgAutoPaste").checked, !!s.autoPaste);
  setIf("text_sync", $("cfgTextSync").checked, !!s.textSync);
  setIf("shell_menu", $("cfgShellMenu").checked, !!s.shellMenu);
  setIf("kvm_enabled", $("cfgKvmEnabled").checked, !!s.kvmEnabled);
  setIf("kvm_touchpad_gestures", $("cfgKvmTouchpad").checked, !!s.kvmTouchpad);
  setIf("kvm_touchpad_speed", num("cfgTouchSpeed"), s.kvmTouchSpeed);
  setIf("kvm_speed_percent", num("cfgSpeedPct"), s.kvmSpeedPct);
  setIf("kvm_move_interval_ms", num("cfgMoveMs"), s.kvmMoveMs);
  setIf("kvm_reflow_step_ms", num("cfgReflowMs"), s.kvmReflowMs);
  setIf("kvm_edge_dead_zone", num("cfgEdgeDZ"), (s.kvmEdgeDZ ?? -1) < 0 ? 48 : s.kvmEdgeDZ);

  if (!Object.keys(payload).length) {
    toast("没有需要保存的修改");
    return;
  }
  const btn = $("btnSaveSettings");
  btn.disabled = true;
  try {
    await call("SaveSettings", payload);
    toast("设置已保存并即时生效");
    const [st, sett] = await Promise.all([call("State"), call("Settings")]);
    app.settings = sett;
    applyState(st);
    fillSettings(sett);
    fillAbout();
  } catch (e) {
    toast("保存失败: " + errText(e), true);
  } finally {
    btn.disabled = false;
  }
}

// ---------------------------------------------------------------- 日志

function pushLog(line) {
  if (line == null || line === "") return;
  app.logs.push(line);
  if (app.logs.length > 1000) app.logs.splice(0, app.logs.length - 1000);
  if (app.logOpen) {
    const box = $("logBox");
    const near = box.scrollHeight - box.scrollTop - box.clientHeight < 60;
    appendLogLine(line);
    if (near) box.scrollTop = box.scrollHeight;
  } else {
    app.unread++;
    updateLogBadge();
  }
  updateLogCount();
}

function appendLogLine(line) {
  const box = $("logBox");
  const span = document.createElement("span");
  span.className = "log-line" + (/失败|错误|error/i.test(line) ? " log-err" : "");
  span.textContent = line;
  box.appendChild(span);
}

function renderLogs() {
  const box = $("logBox");
  box.innerHTML = "";
  for (const line of app.logs) appendLogLine(line);
  box.scrollTop = box.scrollHeight;
  updateLogCount();
}

function updateLogCount() {
  $("logCount").textContent = `${app.logs.length} 行`;
}

function updateLogBadge() {
  const b = $("logBadge");
  if (app.unread > 0) {
    b.hidden = false;
    b.textContent = app.unread > 99 ? "99+" : String(app.unread);
  } else {
    b.hidden = true;
  }
}

function openLogs() {
  app.logOpen = true;
  app.unread = 0;
  updateLogBadge();
  $("logDrawer").classList.add("open");
  $("logDrawer").setAttribute("aria-hidden", "false");
  renderLogs();
}

function closeLogs() {
  app.logOpen = false;
  $("logDrawer").classList.remove("open");
  $("logDrawer").setAttribute("aria-hidden", "true");
}

// ---------------------------------------------------------------- 主题

function applyTheme(t) {
  app.theme = THEMES.includes(t) ? t : "dark";
  storeSet("cw-theme", app.theme);
  if (app.theme === "auto") document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", app.theme);
  document.querySelectorAll("#themeSeg button").forEach((b) => {
    b.setAttribute("aria-checked", String(b.dataset.themeValue === app.theme));
  });
}

function cycleTheme() {
  const i = THEMES.indexOf(app.theme);
  applyTheme(THEMES[(i + 1) % THEMES.length]);
  toast(`主题：${{ auto: "跟随系统", light: "浅色", dark: "深色" }[app.theme]}`);
}

// ---------------------------------------------------------------- Toast / 确认 / 灯箱

function toast(message, isErr) {
  if (!message) return;
  // 收起到 64x64 的图标状态时没有地方显示气泡：错误另写一行运行日志
  // （回标准界面后 Ctrl+L 能看到原文，不再只剩一个没有上下文的数字），
  // 图标角标同时打上错误标记。
  if (app.ui && app.ui.mode === "mini" && app.ui.collapsed) {
    if (isErr) pushLog(message);
    bumpMiniBadge(isErr);
    return;
  }
  const box = document.createElement("div");
  box.className = "toast" + (isErr ? " err" : "");
  box.textContent = message;
  $("toastWrap").appendChild(box);
  setTimeout(() => box.remove(), isErr ? 6000 : 3500);
}

function confirmAction(title, text, okLabel, fn) {
  $("confirmTitle").textContent = title;
  $("confirmText").textContent = text;
  $("btnConfirmOk").textContent = okLabel || "确定";
  app.confirmFn = fn;
  $("confirmBackdrop").hidden = false;
  $("btnConfirmOk").focus();
}

async function runConfirm() {
  const fn = app.confirmFn;
  closeConfirm();
  if (fn) await fn();
}

function closeConfirm() {
  app.confirmFn = null;
  $("confirmBackdrop").hidden = true;
}

function showLightbox(src, name, path) {
  if (!src) return;
  $("lightboxImg").src = src;
  $("lightboxName").textContent = name || "";
  $("btnLightboxOpen").onclick = () => openPath(path, false);
  $("lightbox").hidden = false;
  $("btnLightboxClose").focus();
}

function closeLightbox() {
  $("lightbox").hidden = true;
  $("lightboxImg").removeAttribute("src");
}

// ---------------------------------------------------------------- 视图切换

function switchView(name) {
  app.view = name === "layout" ? "layout" : "workspace";
  document.querySelectorAll(".tabs button").forEach((b) => {
    const active = b.dataset.view === app.view;
    b.classList.toggle("active", active);
    b.setAttribute("aria-selected", String(active));
  });
  $("view-workspace").classList.toggle("active", app.view === "workspace");
  $("view-layout").classList.toggle("active", app.view === "layout");
  if (app.view === "layout") renderLayout();
  else if (nearBottom()) scrollToBottom();
}

// ---------------------------------------------------------------- 静态 UI 绑定

function bindStaticUI() {
  document.querySelectorAll(".tabs button").forEach((b) => {
    b.addEventListener("click", () => switchView(b.dataset.view));
  });

  $("btnPause").onclick = togglePause;
  $("btnTheme").onclick = cycleTheme;
  $("btnSettings").onclick = openSettings;
  $("btnMiniMode").onclick = toggleUiMode;
  $("btnCloseSettings").onclick = () => ($("settingsBackdrop").hidden = true);
  $("btnReloadSettings").onclick = openSettings;
  $("btnSaveSettings").onclick = saveSettings;
  $("btnBrowseReceiveDir").onclick = () => {
    const dir = $("cfgReceiveDir").value.trim();
    if (dir) openPath(dir, false).catch(() => {});
  };

  $("btnLogs").onclick = () => (app.logOpen ? closeLogs() : openLogs());
  $("btnCloseLogs").onclick = closeLogs;
  $("btnClearLogs").onclick = () => {
    app.logs = [];
    renderLogs();
    toast("本窗口日志已清空（不影响日志文件）");
  };
  $("btnCopyLogs").onclick = async () => {
    try {
      await call("CopyText", app.logs.join("\n"));
      toast("日志已复制到剪贴板");
    } catch (e) {
      toast("复制失败: " + errText(e), true);
    }
  };

  $("btnSend").onclick = sendText;
  $("btnAttach").onclick = pickFiles;
  const input = $("composerInput");
  input.addEventListener("input", autoSizeComposer);
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      sendText();
    }
  });
  // 复制文件后在输入框直接 Ctrl+V 即投送：粘贴事件只当触发器，真实路径由 Go 端
  // 读剪贴板取得。只要剪贴板里带文本（含 Excel/Word 复制时附带的位图预览组合），
  // 就一律放行给浏览器默认粘贴，绝不因为看到 Files 就吞掉文本。
  input.addEventListener("paste", handleComposerPaste);

  // ---------- 简约界面 ----------
  $("btnMiniPin").onclick = toggleMiniPin;
  $("btnMiniCollapse").onclick = collapseMini;
  $("btnMiniStandard").onclick = () => setUiMode("standard");
  $("btnMiniHide").onclick = hideWindow;
  $("miniDock").onclick = expandFromDock;
  bindMiniDockDrag($("miniDock"));
  $("btnMiniPause").onclick = togglePause;
  $("btnMiniAttach").onclick = pickFiles;
  $("btnMiniSend").onclick = sendMiniText;
  const miniInput = $("miniInput");
  miniInput.addEventListener("input", autoSizeMiniComposer);
  $("miniFeed").addEventListener("scroll", () => {
    app.miniStick = miniNearBottom();
  });
  miniInput.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      sendMiniText();
    }
  });
  miniInput.addEventListener("paste", handleComposerPaste);

  $("btnOpenDir").onclick = async () => {
    try {
      await call("OpenReceiveDir");
    } catch (e) {
      toast("打开接收目录失败: " + errText(e), true);
    }
  };

  $("btnClearFeed").onclick = () =>
    confirmAction("清空显示", "清空本窗口的时间线显示与历史记录（不影响对端与已传输的文件）。", "清空", () => {
      const timeline = $("timeline");
      const empty = $("feedEmpty");
      timeline.innerHTML = "";
      if (empty) {
        timeline.appendChild(empty);
        empty.hidden = false;
      }
      app.feed = [];
      app.recordsSeen.clear();
      app.lastDate = "";
      clearPersistedFeed();
      renderSub();
    });

  $("btnCopyPairHint").onclick = () =>
    toast("在另一台机器上打开 copywhere（同一网段），它会出现在设备坞；点「配对」并在对端确认即可。");

  $("btnPairAccept").onclick = () => respondPair(true);
  $("btnPairDecline").onclick = () => respondPair(false);

  $("btnConfirmOk").onclick = runConfirm;
  $("btnConfirmCancel").onclick = closeConfirm;

  $("btnLightboxClose").onclick = closeLightbox;
  $("lightbox").onclick = (e) => {
    if (e.target === $("lightbox")) closeLightbox();
  };

  $("kvmToggle").onchange = async (e) => {
    const on = e.target.checked;
    try {
      await call("SaveSettings", { kvm_enabled: on });
      toast(on ? "已启用跨屏" : "已停用跨屏");
    } catch (err) {
      e.target.checked = !on;
      toast("切换跨屏失败: " + errText(err), true);
    }
  };

  document.querySelectorAll("#themeSeg button").forEach((b) => {
    b.onclick = () => applyTheme(b.dataset.themeValue);
  });

  bindLayoutDrop();

  // 任意弹层：点背景关闭（配对弹窗除外——必须明确裁决）
  for (const [id, close] of [
    ["settingsBackdrop", () => ($("settingsBackdrop").hidden = true)],
    ["confirmBackdrop", closeConfirm],
  ]) {
    $(id).addEventListener("mousedown", (e) => {
      if (e.target === $(id)) close();
    });
  }

  // 拖拽视觉反馈（真实路径由 Go 端窗口事件处理，这里只做提示）
  let dragDepth = 0;
  window.addEventListener("dragenter", () => {
    dragDepth++;
    document.body.classList.add("dragging-file");
  });
  window.addEventListener("dragleave", () => {
    dragDepth = Math.max(0, dragDepth - 1);
    if (!dragDepth) document.body.classList.remove("dragging-file");
  });
  window.addEventListener("drop", () => {
    dragDepth = 0;
    document.body.classList.remove("dragging-file");
  });
  window.addEventListener("dragover", (e) => e.preventDefault());

  // 键盘快捷键
  document.addEventListener("keydown", (e) => {
    const mod = e.ctrlKey || e.metaKey;
    if (mod && e.key.toLowerCase() === "k") {
      e.preventDefault();
      switchView("workspace");
      if (app.ui && app.ui.mode === "mini") $("miniInput").focus();
      else $("composerInput").focus();
    } else if (mod && e.key === ",") {
      e.preventDefault();
      openSettings();
    } else if (mod && e.key.toLowerCase() === "l") {
      e.preventDefault();
      app.logOpen ? closeLogs() : openLogs();
    } else if (mod && e.key.toLowerCase() === "m") {
      e.preventDefault();
      // 简约界面收起到边缘时，Ctrl+M 先展开面板（与点图标一致）。
      if (app.ui && app.ui.mode === "mini" && app.ui.collapsed) expandMini();
      else toggleUiMode();
    } else if (e.key === "Escape") {
      if (!$("lightbox").hidden) closeLightbox();
      else if (!$("confirmBackdrop").hidden) closeConfirm();
      else if (!$("settingsBackdrop").hidden) $("settingsBackdrop").hidden = true;
      else if (app.logOpen) closeLogs();
    }
  });
}

boot().catch((e) => {
  console.error("前端启动失败", e);
  toast("前端启动失败: " + errText(e), true);
});
