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

const WATCH_LABEL = { listener: "事件驱动", viewer: "轮询", none: "未启用" };
const IMG_EXT = /\.(png|jpe?g|gif|bmp|webp)$/i;
const THEMES = ["auto", "light", "dark"];

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
  theme: localStorage.getItem("cw-theme") || "dark",
  pendingPair: null,
  confirmFn: null,
  dragName: "",
  lastDate: "",
};

// ---------------------------------------------------------------- 启动

async function boot() {
  applyTheme(app.theme);
  bindStaticUI();
  bindWailsEvents();
  await Promise.allSettled([loadState(), loadPeers(), loadRecords(), loadLogs(), loadPendingPair()]);
  try {
    await call("UiReady", `${navigator.userAgent} / ${window.innerWidth}x${window.innerHeight}`);
  } catch (e) {
    console.error("UiReady 调用失败", e);
  }
  autoSizeComposer();
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
  for (const r of list) addRecord(r, true);
  renderSub();
}

async function loadLogs() {
  const lines = await call("Logs");
  app.logs = (Array.isArray(lines) ? lines : []).slice(-1000);
  if (app.logOpen) renderLogs();
  updateLogBadge();
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
}

function renderSub() {
  const st = app.state || {};
  const today = localDateStr(new Date());
  const todayCount = app.feed.filter((r) => r.date === today).length;
  $("stageSub").textContent = `在线 ${app.peers.length} · 已配对 ${st.pairedCount || 0} · 今日 ${todayCount} 条`;
  const hint = $("composerHint");
  if (hint) {
    hint.textContent = app.peers.length
      ? `在线 ${app.peers.length} 个节点 · 拖入文件即投送`
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
  const key = [r.date, r.time, r.in ? 1 : 0, r.peer, r.name, r.size, r.detail].join("|");
  if (app.recordsSeen.has(key)) return;
  app.recordsSeen.add(key);
  app.feed.push({ ...r, key });

  const timeline = $("timeline");
  $("feedEmpty").hidden = true;
  const near = nearBottom();
  if (r.date !== app.lastDate) {
    app.lastDate = r.date;
    timeline.appendChild(dateSeparator(r.date));
  }
  timeline.appendChild(recordRow(r));
  renderSub();
  if (near || silent) scrollToBottom();
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
    const copy = iconButton("复制内容", '<path d="M9 9h11v11H9zM4 15V4h11"/>', async () => {
      try {
        await call("CopyText", r.detail || r.name || "");
        toast("已复制到本机剪贴板");
      } catch (e) {
        toast("复制失败: " + errText(e), true);
      }
    });
    meta.appendChild(copy);
  } else if (r.in && r.detail) {
    const open = iconButton("打开文件", '<path d="M14 4h6v6M20 4l-9 9M18 13v7H4V6h6"/>', () =>
      openPath(r.detail, false)
    );
    const reveal = iconButton("打开所在位置", '<path d="M3 6h6l2 2h10v11H3z"/>', () =>
      openPath(r.detail, true)
    );
    meta.appendChild(open);
    meta.appendChild(reveal);
    row.classList.add("act");
    row.title = "双击打开文件";
    row.addEventListener("dblclick", () => openPath(r.detail, false));
  }
  if (meta.childNodes.length) bubble.appendChild(meta);
  return row;
}

function renderRecordBody(r) {
  if (r.text) {
    const d = document.createElement("div");
    d.className = "b-text";
    d.textContent = r.detail && r.detail.trim() ? r.detail : r.name;
    d.title = "点击展开 / 收起";
    d.onclick = () => d.classList.toggle("open");
    return { el: d };
  }
  if (r.in && r.detail && IMG_EXT.test(r.name || "")) {
    const wrap = document.createElement("div");
    const img = document.createElement("img");
    img.className = "b-img";
    img.alt = r.name || "图片";
    img.title = "点击放大";
    const fallback = fileCard(r);
    const fail = () => {
      if (img.parentNode === wrap) {
        img.remove();
        wrap.appendChild(fallback);
      }
    };
    img.onerror = fail;
    img.onclick = () => showLightbox(img.src, r.name, r.detail);
    wrap.appendChild(img);
    call("ImagePreview", r.detail)
      .then((src) => {
        if (src) img.src = src;
        else fail();
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

  if (p.sent < 0) {
    if (x) {
      x.el.remove();
      xfers.delete(key);
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
    x = { el: box, bar: box.querySelector("i"), pct: box.querySelector(".pct"), amt: box.querySelector(".amt"), timer: null };
    xfers.set(key, x);
  }
  const pct = p.total > 0 ? Math.min(100, (p.sent / p.total) * 100) : 100;
  x.bar.style.width = pct + "%";
  x.pct.textContent = pct.toFixed(0) + "%";
  x.amt.textContent = `${fmtBytes(p.sent)} / ${fmtBytes(p.total)}`;
  if (p.total <= 0 || p.sent >= p.total) {
    clearTimeout(x.timer);
    x.timer = setTimeout(() => {
      const cur = xfers.get(key);
      if (cur === x) {
        x.el.remove();
        xfers.delete(key);
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

function showPairRequest(p) {
  if (!p || !p.id) return;
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
  localStorage.setItem("cw-theme", app.theme);
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

  $("btnOpenDir").onclick = async () => {
    try {
      await call("OpenReceiveDir");
    } catch (e) {
      toast("打开接收目录失败: " + errText(e), true);
    }
  };

  $("btnClearFeed").onclick = () =>
    confirmAction("清空显示", "只清空本窗口的时间线显示，不影响对端与已传输的文件。", "清空", () => {
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
      $("composerInput").focus();
    } else if (mod && e.key === ",") {
      e.preventDefault();
      openSettings();
    } else if (mod && e.key.toLowerCase() === "l") {
      e.preventDefault();
      app.logOpen ? closeLogs() : openLogs();
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
