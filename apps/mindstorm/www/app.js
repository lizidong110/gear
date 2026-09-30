/* mindstorm 前端 —— Finder 风格三栏思维导图（原生 JS，零依赖）
 *
 * 后端契约：所有操作经 POST /api/tools/{id}/run（body=入参 map，响应 {id,output}）。
 *   调用统一走 api.js（gen-client.mjs 从 Tool YAML 生成）暴露的 API.* 方法，
 *   不在此文件里手写 tool id 字符串；AI 流式路径（SSE 直调 ext.ai）除外。
 * 视图状态（当前树 / 选中节点 / 聚焦根 / 阅读编辑模式）存 localStorage，便于刷新后恢复。
 *
 * 渲染策略：中栏是「聚焦 3 级窗口」——以 focusId 为锚，只渲染其下最多 3 级，
 * 天然把 DOM 规模限制在可见范围；要看更深就选中节点后「下一级」(→) 设为新焦点。
 * 右栏详情默认「阅读模式」（markdown 渲染），可切「编辑模式」改标题/摘要/正文。
 */
"use strict";

const $ = (s) => document.querySelector(s);
const el = (tag, cls, txt) => {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (txt != null) e.textContent = txt;
  return e;
};

/* ---------- Markdown（阅读模式渲染，零依赖、转义防 XSS） ---------- */
function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));
}
function inlineMd(s) {
  let t = escapeHtml(s);
  t = t.replace(/`([^`]+)`/g, "<code>$1</code>");
  t = t.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
  t = t.replace(/(^|[^*])\*([^*\s][^*]*?)\*/g, "$1<em>$2</em>");
  t = t.replace(/__([^_]+)__/g, "<strong>$1</strong>");
  t = t.replace(/(^|[^_\w])_([^_\s][^_]*?)_/g, "$1<em>$2</em>");
  t = t.replace(/\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g,
    '<a href="$2" target="_blank" rel="noopener">$1</a>');
  return t;
}
function mdToHtml(src) {
  if (!src || !src.trim()) return '<p class="md-empty">（空）</p>';
  const lines = src.replace(/\r\n/g, "\n").split("\n");
  let html = "", i = 0, inList = null;
  const closeList = () => { if (inList) { html += `</${inList}>`; inList = null; } };
  while (i < lines.length) {
    const line = lines[i];
    if (/^```/.test(line)) {                       // 代码块
      closeList();
      const buf = []; i++;
      while (i < lines.length && !/^```/.test(lines[i])) { buf.push(lines[i]); i++; }
      i++;
      html += `<pre><code>${escapeHtml(buf.join("\n"))}</code></pre>`;
      continue;
    }
    let m = line.match(/^(#{1,6})\s+(.*)$/);        // 标题
    if (m) { closeList(); const lv = m[1].length; html += `<h${lv}>${inlineMd(m[2])}</h${lv}>`; i++; continue; }
    if (/^>\s?/.test(line)) {                        // 引用
      closeList();
      const buf = [];
      while (i < lines.length && /^>\s?/.test(lines[i])) { buf.push(lines[i].replace(/^>\s?/, "")); i++; }
      html += `<blockquote>${inlineMd(buf.join(" "))}</blockquote>`;
      continue;
    }
    m = line.match(/^\s*[-*]\s+(.*)$/);              // 无序列表
    if (m) { if (inList !== "ul") { closeList(); html += "<ul>"; inList = "ul"; } html += `<li>${inlineMd(m[1])}</li>`; i++; continue; }
    m = line.match(/^\s*\d+\.\s+(.*)$/);             // 有序列表
    if (m) { if (inList !== "ol") { closeList(); html += "<ol>"; inList = "ol"; } html += `<li>${inlineMd(m[1])}</li>`; i++; continue; }
    if (/^\s*$/.test(line)) { closeList(); i++; continue; }
    closeList();                                     // 段落
    const buf = [line]; i++;
    while (i < lines.length && !/^\s*$/.test(lines[i]) &&
      !/^(#{1,6}\s|>\s?|\s*[-*]\s|\s*\d+\.\s|```)/.test(lines[i])) { buf.push(lines[i]); i++; }
    html += `<p>${inlineMd(buf.join("<br>"))}</p>`;
  }
  closeList();
  return html;
}

/* 搜索结果高亮：先转义再标 <mark>（防 XSS） */
function hl(text, q) {
  const safe = escapeHtml(text || "");
  if (!q) return safe;
  const re = new RegExp(q.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "gi");
  return safe.replace(re, (m) => `<mark>${m}</mark>`);
}

/* ---------- 状态 ---------- */
const state = {
  trees: [],
  currentTree: null,   // tree id
  selected: null,      // node id
  focusId: null,       // 中栏聚焦根节点（null = 整棵树根）
  readMode: true,      // 右栏默认阅读模式
  catFilter: "",       // 左栏分类筛选（"" = 全部）
  searchQ: "",         // 左栏搜索关键词（非空时左栏显示搜索结果）
  searchHits: [],      // 搜索结果缓存
  aiCfg: {},           // AI 配置（内核配置库 config.db 持久化，与 gear config 同源），SSE 直调 ext.ai 时作调用参数带上
  nodes: {},           // id -> node (含 _parent / _expanded / _childrenLoaded / _children / _tags)
  roots: [],           // root node 列表（_parent=null）
  visible: [],         // 当前可见节点 id（渲染顺序，键盘导航用）
};
const MAX_REL = 2;     // 聚焦窗口最多 3 级（0/1/2）
const LS = {
  get tree() { return localStorage.getItem("mindstorm.tree"); },
  set tree(v) { localStorage.setItem("mindstorm.tree", v); },
  get node() { return localStorage.getItem("mindstorm.node"); },
  set node(v) { v ? localStorage.setItem("mindstorm.node", v) : localStorage.removeItem("mindstorm.node"); },
  // AI 续写要求：是「意图」而非「数据」，刷新后保留能省去重复输入。
  get instr() { return localStorage.getItem("mindstorm.instr") || ""; },
  set instr(v) { v ? localStorage.setItem("mindstorm.instr", v) : localStorage.removeItem("mindstorm.instr"); },
};

function genId() {
  if (crypto && crypto.randomUUID) return crypto.randomUUID();
  return "n_" + Date.now().toString(36) + "_" + Math.random().toString(36).slice(2, 8);
}
function toast(msg, isErr) {
  const t = $("#toast");
  t.textContent = msg;
  t.style.background = isErr ? "#5a1d1d" : "#333";
  t.classList.add("show");
  clearTimeout(toast._t);
  // 错误消息停留 8 秒且可点击关闭（此前 2.6s 一闪而过，「没看清报了什么错」——2026-09-24）
  toast._t = setTimeout(() => t.classList.remove("show"), isErr ? 8000 : 2600);
}
function showBanner(msg) {
  const b = $("#banner");
  if (!msg) { b.style.display = "none"; return; }
  b.textContent = msg; b.style.display = "block";
}

/* ---------- 应用内对话框（替代原生 confirm / prompt） ----------
 * 为什么必须自绘：原生 `confirm()` / `prompt()` 在**内嵌 webview / IDE 预览面板**里
 * 常被静默拦掉 —— 直接返回 null/false，既不弹窗也不报错。症状是「点了删除没反应」
 * 「点新建没反应」，而且没有任何线索指向原因（控制台也干净）。
 * 自绘对话框在所有宿主环境行为一致，还能对齐暗色主题、给危险操作上红字。
 *
 * 返回：input=false → true(确定) / null(取消)；input=true → 字符串(可能为空串) / null(取消)。
 * 注意**不对空串做拦截**：是否允许空值由调用方决定（如「分类」允许清空、「节点标题」不允许）。
 */
function ask(opts) {
  const o = opts || {};
  return new Promise((resolve) => {
    const back = el("div", "modal-back");
    const box = el("div", "modal");
    box.appendChild(el("div", "modal-msg", o.message || ""));

    let input = null;
    if (o.input) {
      input = el("input", "modal-input");
      input.value = o.defaultValue || "";
      input.placeholder = o.placeholder || "";
      box.appendChild(input);
    }

    const actions = el("div", "modal-actions");
    const cancel = el("button", "tiny", o.cancelText || "取消");
    const ok = el("button", "tiny " + (o.danger ? "danger" : "primary"), o.okText || "确定");
    actions.appendChild(cancel);
    actions.appendChild(ok);
    box.appendChild(actions);
    back.appendChild(box);
    document.body.appendChild(back);

    const done = (val) => {
      document.removeEventListener("keydown", onModalKey, true);
      back.remove();
      resolve(val);
    };
    const submit = () => done(o.input ? input.value.trim() : true);
    // capture 阶段只拦 Esc/Enter（防止落到底层的中栏键盘导航）；其余按键放行，
    // 否则输入框自己收不到字符。
    const onModalKey = (e) => {
      if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); done(null); }
      else if (e.key === "Enter") { e.preventDefault(); e.stopPropagation(); submit(); }
    };
    document.addEventListener("keydown", onModalKey, true);

    cancel.onclick = () => done(null);
    ok.onclick = submit;
    // 点击遮罩空白处 = 取消（与原生对话框「点外面不生效」不同，这里显式给出取消路径）
    back.onclick = (e) => { if (e.target === back) done(null); };

    setTimeout(() => { if (input) { input.focus(); input.select(); } else ok.focus(); }, 0);
  });
}

/* ---------- AI 配置面板（⚙） ----------
 * 把 AI 相关配置（provider / 端点 / 模型 / api_key）放进界面并持久化到内核配置库（config.db，
 * 经 ctx.config，与 `gear config set ai.*` 同源），替代「每次在命令行 export GEAR_AI_API_KEY 再起服务」。后端两侧都认这份配置：
 *   - Tool 同步路径：mindstorm_ai.yaml 的 aiCfg() 从库里读出来，作为 ext.ai.complete 调用参数；
 *   - 前端流式路径：state.aiCfg 在 boot 时拉一次，streamComplete 直接带在 SSE 调用参数里。
 * 留空的字段 = 不覆盖，回退引擎默认链（调用参数 → GEAR_AI_* 环境变量 → 内置默认）。
 */
function openSettings() {
  const back = el("div", "modal-back");
  const box = el("div", "modal settings-modal");
  box.appendChild(el("div", "modal-msg", "AI 配置"));

  const mkRow = (labelText, input, hint) => {
    const wrap = el("div", "set-row");
    wrap.appendChild(el("label", "set-label", labelText));
    wrap.appendChild(input);
    if (hint) wrap.appendChild(el("span", "hint-inline", hint));
    return wrap;
  };

  const provider = el("select", "modal-input");
  for (const p of ["", "siliconflow", "openai", "ollama"]) {
    const opt = el("option", null, p === "" ? "（默认 siliconflow）" : p);
    opt.value = p;
    if ((state.aiCfg.ai_provider || "") === p) opt.selected = true;
    provider.appendChild(opt);
  }
  const base = el("input", "modal-input");
  base.value = state.aiCfg.ai_base_url || "";
  base.placeholder = "留空用 provider 默认端点（如 https://api.siliconflow.cn/v1）";
  const model = el("input", "modal-input");
  model.value = state.aiCfg.ai_model || "";
  model.placeholder = "留空用 provider 默认模型";
  const key = el("input", "modal-input");
  key.type = "password";
  key.value = state.aiCfg.ai_api_key || "";
  key.placeholder = state.aiCfg.ai_api_key ? "已保存（输入新值可覆盖，清空并保存 = 删除）" : "sk-…";
  box.appendChild(mkRow("Provider", provider));
  box.appendChild(mkRow("API 端点", base));
  box.appendChild(mkRow("模型", model));
  box.appendChild(mkRow("API Key", key, "只存在本地 mindstorm.db，不上传"));

  const actions = el("div", "modal-actions");
  const cancel = el("button", "tiny", "取消");
  const ok = el("button", "tiny primary", "保存");
  actions.appendChild(cancel); actions.appendChild(ok);
  box.appendChild(actions);
  back.appendChild(box);
  document.body.appendChild(back);

  const done = (val) => { document.removeEventListener("keydown", onKey, true); back.remove(); if (val) val(); };
  const onKey = (e) => {
    if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); done(); }
    else if (e.key === "Enter" && e.target.tagName !== "SELECT") { e.preventDefault(); e.stopPropagation(); ok.onclick(); }
  };
  document.addEventListener("keydown", onKey, true);
  cancel.onclick = () => done();
  back.onclick = (e) => { if (e.target === back) done(); };
  ok.onclick = async () => {
    const cfg = {
      ai_provider: provider.value,
      ai_base_url: base.value.trim(),
      ai_model: model.value.trim(),
      ai_api_key: key.value.trim(),
    };
    try {
      await API.settingsSet(cfg);
      state.aiCfg = cfg;   // 本地缓存同步更新，流式路径立即生效，无需刷新
      updateCfgBtnTitle();
      done(() => toast("AI 配置已保存"));
    } catch (e) { toast("保存失败：" + e.message, true); }
  };
  setTimeout(() => (state.aiCfg.ai_api_key ? ok : key).focus(), 0);
}

function updateCfgBtnTitle() {
  const btn = $("#aiCfgBtn");
  if (btn) btn.title = state.aiCfg.ai_api_key
    ? "AI 配置（已设置 Key）" : "AI 配置（未设置 Key：真实生成会失败，可先用 demo 勾选体验）";
}

async function loadSettings() {
  try {
    const out = await API.settingsGet({});
    state.aiCfg = out.settings || {};
    updateCfgBtnTitle();
  } catch (_) { /* 拉不到就维持空配置（回退环境变量） */ }
}

/* ---------- 左栏：搜索 / 分类 / 思维树 ---------- */
function renderCats() {
  const box = $("#catChips");
  if (!box) return;
  box.innerHTML = "";
  const cats = [...new Set(state.trees.map((t) => t.category || "").filter(Boolean))];
  const make = (label, val) => {
    const c = el("span", "cat-chip" + (state.catFilter === val ? " active" : ""), label);
    c.onclick = () => { state.catFilter = val; renderCats(); renderTrees(); };
    return c;
  };
  box.appendChild(make("全部", ""));
  for (const cat of cats) box.appendChild(make(cat, cat));
}
function renderTrees() {
  // 搜索态：左栏展示搜索结果
  if (state.searchQ) return renderSearchResults();
  const box = $("#treeList");
  box.innerHTML = "";
  const list = state.trees.filter((t) => !state.catFilter || (t.category || "") === state.catFilter);
  if (!state.trees.length) {
    box.appendChild(el("div", "empty", "还没有思维树，点右上角 ＋ 新建"));
    return;
  }
  if (!list.length) {
    box.appendChild(el("div", "empty", "该分类下暂无思维树"));
    return;
  }
  for (const t of list) {
    const item = el("div", "tree-item" + (t.id === state.currentTree ? " active" : ""));
    const title = el("div", "t-title", t.title);
    title.title = "双击重命名";
    title.ondblclick = (e) => { e.stopPropagation(); editTreeField(t, "title", t.title); };
    item.appendChild(title);
    const cat = el("div", "t-cat", t.category || "未分类");
    cat.title = "双击修改分类";
    cat.ondblclick = (e) => { e.stopPropagation(); editTreeField(t, "category", t.category || ""); };
    item.appendChild(cat);
    item.onclick = () => selectTree(t.id);
    const actions = el("div", "tree-actions");
    const ren = el("button", "tiny", "重命名");
    ren.onclick = (e) => { e.stopPropagation(); editTreeField(t, "title", t.title); };
    const catBtn = el("button", "tiny", "分类");
    catBtn.onclick = (e) => { e.stopPropagation(); editTreeField(t, "category", t.category || ""); };
    const del = el("button", "tiny", "删除");
    del.onclick = (e) => { e.stopPropagation(); deleteTree(t); };
    actions.appendChild(ren); actions.appendChild(catBtn); actions.appendChild(del);
    item.appendChild(actions);
    box.appendChild(item);
  }
}
async function editTreeField(t, field, cur) {
  const val = await ask({
    message: field === "title" ? "重命名思维树" : "修改分类",
    input: true, defaultValue: cur,
    placeholder: field === "title" ? "输入新标题" : "输入分类名（可留空）",
  });
  if (val === null) return;
  const payload = { id: t.id };
  if (field === "title") payload.title = val; else payload.category = val;
  try {
    await API.treeUpdate(payload);
    toast("已更新");
    await loadTrees();
    if (state.currentTree === t.id) {
      const fresh = state.trees.find((x) => x.id === t.id);
      $("#midTitle").textContent = fresh ? fresh.title : $("#midTitle").textContent;
    }
  } catch (e) { toast("更新失败：" + e.message, true); }
}
async function loadTrees() {
  const out = await API.treeList({});
  state.trees = out.trees || [];
  renderCats();
  renderTrees();
}

/* ---------- 左栏：搜索结果 ---------- */
let searchTimer = null;
function onSearchInput() {
  clearTimeout(searchTimer);
  const q = $("#searchBox").value.trim();
  searchTimer = setTimeout(() => runSearch(q), 300);
}
async function runSearch(q) {
  state.searchQ = q;
  if (!q) { state.searchHits = []; renderTrees(); return; }
  try {
    const out = await API.search({ q });
    state.searchHits = out.nodes || [];
  } catch (e) {
    state.searchHits = [];
    toast("搜索失败：" + e.message, true);
  }
  renderTrees();
}
function renderSearchResults() {
  const box = $("#treeList");
  box.innerHTML = "";
  const q = state.searchQ;
  box.appendChild(el("div", "sr-count", `${state.searchHits.length} 条命中「${q}」`));
  if (!state.searchHits.length) {
    box.appendChild(el("div", "empty", "没有匹配的节点"));
    return;
  }
  for (const h of state.searchHits) {
    const item = el("div", "sr-item");
    const path = el("div", "sr-path");
    path.textContent = (h.tree_title || "?") + (h.breadcrumb ? " › " + h.breadcrumb : "");
    item.appendChild(path);
    const title = el("div", "sr-title");
    title.innerHTML = hl(h.title, q);
    item.appendChild(title);
    if (h.snippet) {
      const sn = el("div", "sr-snippet");
      sn.innerHTML = hl(h.snippet, q);
      item.appendChild(sn);
    }
    item.onclick = () => openSearchResult(h.tree_id, h.id);
    box.appendChild(item);
  }
}
async function openSearchResult(treeId, nodeId) {
  try {
    if (state.currentTree !== treeId) await selectTree(treeId);
    await revealNode(nodeId);
  } catch (e) { toast("定位失败：" + e.message, true); }
}
/* 把某节点在树上「滚出来」：焦点设到能看见它的祖先，展开链路后选中 */
async function revealNode(nodeId) {
  let full;
  try { full = await API.nodeGet({ id: nodeId }); } catch (e) { toast("定位失败：" + e.message, true); return; }
  const chain = String(full.path || "").split("/").filter(Boolean);  // root..node
  const D = chain.length - 1;
  const d0 = Math.max(0, D - MAX_REL);
  // 加载链上缺失节点
  for (const nid of chain) {
    if (!state.nodes[nid]) {
      try { const f = await API.nodeGet({ id: nid }); cacheNode(f, f.parent_id || null); } catch (_) {}
    }
  }
  state.focusId = d0 === 0 ? null : chain[d0];
  if (state.focusId && !state.nodes[state.focusId]) state.focusId = null;
  if (state.focusId) await ensureSubtree(state.focusId, MAX_REL);
  else await ensureSubtree(null, MAX_REL);
  // 展开链上节点，保证目标可见
  for (let i = d0; i < chain.length; i++) { const n = state.nodes[chain[i]]; if (n) n._expanded = true; }
  selectNode(nodeId, true);
}
async function newTree() {
  const title = await ask({ message: "新建思维树", input: true, defaultValue: "未命名想法", placeholder: "输入标题" });
  if (!title) return;
  const id = genId();
  await API.treeCreate({ id, title, category: "" });
  // 新建后退出搜索态，回到树列表
  state.searchQ = ""; state.searchHits = []; if ($("#searchBox")) $("#searchBox").value = "";
  await loadTrees();
  selectTree(id);
}
async function deleteTree(t) {
  if (!(await ask({
    message: `删除思维树「${t.title}」及其所有节点？\n此操作不可撤销。`,
    okText: "删除", danger: true,
  }))) return;
  await API.treeDelete({ id: t.id });
  if (state.currentTree === t.id) { state.currentTree = null; state.roots = []; state.selected = null; renderTrees(); renderMiddle(); renderDetail(); }
  await loadTrees();
}

/* ---------- 中栏：节点树 ---------- */
async function selectTree(treeId) {
  state.currentTree = treeId;
  LS.tree = treeId;
  state.selected = null; LS.node = "";
  state.focusId = null;
  state.nodes = {};
  state.roots = [];
  $("#addBtn").style.display = "inline-block";
  await loadRoots();
  await ensureSubtree(null, MAX_REL);          // 整树根 + 下方 3 级
  if (LS.node && state.nodes[LS.node]) selectNode(LS.node, false);
  renderTrees(); renderMiddle(); renderDetail(); updateMidHeader();
}

/* 以 focusId 为锚，把其下方最多 levels 级的子节点全部载入缓存 */
async function ensureSubtree(id, levels) {
  let frontier = id ? [id] : state.roots.map((r) => r.id);
  for (let d = 0; d < levels; d++) {
    const next = [];
    for (const pid of frontier) {
      const node = state.nodes[pid];
      if (!node) continue;
      await ensureChildren(pid);
      for (const c of (node._children || [])) next.push(c.id);
    }
    frontier = next;
  }
}

async function loadRoots() {
  const out = await API.nodeChildren({ tree_id: state.currentTree });
  state.roots = (out.nodes || []).map((n) => cacheNode(n, null));
}
function cacheNode(n, parentId) {
  const cur = state.nodes[n.id] || {};
  const merged = Object.assign(cur, {
    id: n.id, title: n.title, depth: n.depth, ai_generated: n.ai_generated,
    summary: n.summary, child_count: n.child_count, _parent: parentId,
  });
  state.nodes[n.id] = merged;
  return merged;
}
async function ensureChildren(id) {
  const node = state.nodes[id];
  if (!node || node._childrenLoaded) return;
  node._loading = true; renderMiddle();
  try {
    const out = await API.nodeChildren({ tree_id: state.currentTree, parent_id: id });
    node._children = (out.nodes || []).map((c) => cacheNode(c, id));
    node.child_count = node._children.length;
    node._childrenLoaded = true;
  } finally {
    node._loading = false;
  }
}
function toggleExpand(id) {
  const node = state.nodes[id];
  if (!node) return;
  node._expanded = !node._expanded;
  if (node._expanded) ensureChildren(id).then(renderMiddle);
  else renderMiddle();
}

/* ---------- 中栏聚焦导航（上/下/Home） ---------- */
async function applyFocus(newFocus, doSelect) {
  state.focusId = newFocus;
  if (newFocus) {
    if (!state.nodes[newFocus]) return;
    if (doSelect) { state.selected = newFocus; LS.node = newFocus; }
    await ensureSubtree(newFocus, MAX_REL);
  } else {
    await ensureSubtree(null, MAX_REL);
  }
  renderMiddle(); renderDetail(); updateMidHeader();
}
async function focusUp() {
  if (!state.focusId) return;
  const f = state.nodes[state.focusId];
  const parent = f && f._parent;
  await applyFocus(parent || null, !!parent);
}
async function drillInto(id) {
  if (!id) return;
  await ensureChildren(id);
  const node = state.nodes[id];
  if (!(node && (node._children || []).length)) { toast("该节点没有子节点"); return; }
  await applyFocus(id, true);
}
async function focusHome() {
  if (!state.focusId) return;
  await applyFocus(null, false);
}
function updateMidHeader() {
  const focus = state.focusId ? state.nodes[state.focusId] : null;
  $("#midTitle").textContent = focus
    ? focus.title
    : ((state.trees.find((t) => t.id === state.currentTree) || {}).title || "节点");
  const addBtn = $("#addBtn");
  if (addBtn) addBtn.textContent = state.selected ? "＋ 子节点" : "＋ 根节点";
  const up = $("#navUp"), down = $("#navDown"), home = $("#navHome");
  if (up) up.disabled = !state.focusId;
  if (home) home.disabled = !state.focusId;
  const sel = state.selected && state.nodes[state.selected];
  if (down) down.disabled = !(sel && sel.child_count > 0);
}

function renderMiddle() {
  const box = $("#nodeTree");
  const focusedBefore = box.contains(document.activeElement);
  box.innerHTML = "";
  state.visible = [];
  if (!state.currentTree) { box.appendChild(el("div", "empty", "从左侧选择一个思维树")); return; }
  const start = state.focusId ? [state.nodes[state.focusId]] : state.roots;
  if (!start.length) { box.appendChild(el("div", "empty", "点「＋ 子节点」新建根节点，或选中某节点后按 Tab 给它加子节点")); }
  for (const n of start) if (n) renderNode(box, n, 0);
  // 重渲染后把焦点留在树容器，键盘导航才不会断（仅当焦点本就在树内时，避免抢走右栏输入框）
  if (focusedBefore) box.focus({ preventScroll: true });
}

function renderNode(box, node, rel) {
  if (!node) return;
  const isFocus = node.id === state.focusId;
  const hasKids = node.child_count > 0;
  const row = el("div", "node-row" +
    (node.id === state.selected ? " selected" : "") +
    (isFocus ? " focus-root" : ""));
  row.style.paddingLeft = 8 + rel * 18 + "px";
  row.dataset.id = node.id;
  row.tabIndex = -1;

  const disc = el("span", "disclosure" + (hasKids ? "" : " leaf"));
  if (rel < MAX_REL) {
    disc.textContent = node._expanded ? "▾" : "▸";
    disc.onclick = (e) => { e.stopPropagation(); toggleExpand(node.id); };
  } else {
    // 已达窗口最深级：有子节点则显示「…」，点击钻入（设为新焦点）
    disc.textContent = hasKids ? "…" : "";
    if (hasKids) disc.onclick = (e) => { e.stopPropagation(); drillInto(node.id); };
  }
  row.appendChild(disc);

  if (node._loading) row.appendChild(el("span", "spinner"));

  const title = el("span", "node-title" + (node.ai_generated ? " ai" : ""), node.title || "(未命名)");
  row.appendChild(title);

  if (isFocus) row.appendChild(el("span", "badge focus", "根"));
  if (node.ai_generated) row.appendChild(el("span", "badge ai", "✨"));
  if (hasKids && !(node._expanded && node._childrenLoaded))
    row.appendChild(el("span", "badge", String(node.child_count)));

  row.onclick = () => selectNode(node.id, true);
  box.appendChild(row);
  state.visible.push(node.id);

  // 仅在窗口内（rel < MAX_REL）且已展开时递归渲染子级
  if (rel < MAX_REL && node._expanded && node._childrenLoaded) {
    for (const c of node._children) renderNode(box, c, rel + 1);
  }
}

/* ---------- 右栏：详情 + AI 续写 ---------- */
let saveTimer = null;
function renderDetail() {
  const box = $("#detail");
  box.innerHTML = "";
  const id = state.selected;
  const node = id && state.nodes[id];
  const toggle = $("#modeToggle");
  if (toggle) toggle.textContent = state.readMode ? "✏️ 编辑" : "👁 阅读";
  if (!node) { box.appendChild(el("div", "empty", "从中间栏选择一个节点")); return; }

  let editRefs = null;
  if (state.readMode) {
    // —— 阅读模式：只渲染已写好的 markdown，不暴露任何操作按钮 ——
    box.appendChild(el("h2", "rd-title", node.title || "(未命名)"));
    box.appendChild(el("div", "meta",
      `深度 ${node.depth} · ${node.ai_generated ? "AI 生成 ✨" : "手工"} · 子节点 ${node.child_count}`));
    if (node.summary && node.summary.trim()) {
      box.appendChild(el("div", "rd-label", "摘要"));
      const s = el("div", "md"); s.innerHTML = mdToHtml(node.summary); box.appendChild(s);
    }
    if (node.body && node.body.trim()) {
      box.appendChild(el("div", "rd-label", "正文"));
      const b = el("div", "md"); b.innerHTML = mdToHtml(node.body); box.appendChild(b);
    }
    if (!(node.summary && node.summary.trim()) && !(node.body && node.body.trim())) {
      box.appendChild(el("div", "rd-empty", "（暂无内容，点「✏️ 编辑」补充摘要 / 正文）"));
    }
    if (node._tags && node._tags.length) appendTags(box, node, false);
    // 阅读模式引导：一键进编辑模式（那里才有 AI 续写 / 生成轮廓 / 删除）
    const hint = el("button", "tiny hint-btn",
      "👉 切到「✏️ 编辑」可 AI 续写子节点 / 生成完整轮廓 / 删除节点");
    hint.onclick = () => { state.readMode = false; renderDetail(); };
    box.appendChild(hint);
    return;
  }

  // —— 编辑模式：表单 + AI 操作 ——
  box.appendChild(el("label", null, "标题"));
  const title = el("input", null); title.value = node.title || ""; box.appendChild(title);
  box.appendChild(el("label", null, "摘要（AI 续写的裁剪单位）"));
  const summary = el("textarea", null); summary.value = node.summary || ""; summary.rows = 2; box.appendChild(summary);
  box.appendChild(el("label", null, "正文"));
  const body = el("textarea", "body", null); body.value = node.body || ""; box.appendChild(body);
  box.appendChild(el("div", "meta",
    `深度 ${node.depth} · ${node.ai_generated ? "AI 生成 ✨" : "手工"} · 子节点 ${node.child_count}`));
  appendTags(box, node, true);
  const onInput = () => {
    node.title = title.value; node.summary = summary.value; node.body = body.value;
    const t = state.nodes[node.id];
    if (t) { t.title = node.title; t.summary = node.summary; t.body = node.body; }
    const r = document.querySelector(`.node-row[data-id="${cssEsc(node.id)}"] .node-title`);
    if (r) r.textContent = node.title || "(未命名)";
    clearTimeout(saveTimer);
    saveTimer = setTimeout(() => saveNode(node.id), 400);
  };
  title.oninput = onInput; summary.oninput = onInput; body.oninput = onInput;
  editRefs = { title, summary, body };

  // AI 续写 / 生成整棵轮廓（仅编辑模式）
  // 「续写要求」是本次新增的核心输入：回答「AI 到底该续写个啥」——不填则只按标题自由发散。
  const ai = el("div", "ai-tools");
  ai.appendChild(el("label", null, "AI 要求（可选）：限定角度 / 受众 / 详略 —— 对下面两个按钮都生效"));
  const instr = el("textarea", "instr"); instr.rows = 2;
  instr.placeholder = "例1：只从工程实现切入，不要市场类主题。例2：我是初学者，请把这一节讲详细些";
  instr.value = LS.instr;
  instr.oninput = () => { LS.instr = instr.value; };
  ai.appendChild(instr);

  const row = el("div", "ai-row");
  row.appendChild(el("label", null, "每层分支"));
  // 留空 / 0 = 让 AI 按内容自然决定分几支（默认，也是推荐）——用户本来就不该猜 N。
  const cnt = el("input"); cnt.type = "number"; cnt.min = "0"; cnt.max = "12"; cnt.value = ""; cnt.placeholder = "自动"; cnt.style.width = "56px";
  cnt.title = "留空或填 0 = 让 AI 按内容自然决定分几支（推荐）；也可指定 1~12 作为目标条数";
  row.appendChild(cnt);
  row.appendChild(el("span", "hint-inline", "留空=自动，或 1~12"));
  row.appendChild(el("label", null, "层"));
  const depth = el("input"); depth.type = "number"; depth.min = "1"; depth.max = "4"; depth.value = "1"; depth.style.width = "40px";
  row.appendChild(depth);
  row.appendChild(el("span", "hint-inline", "≤4（1=续写一层，>1=生成整棵轮廓）"));
  // 重新规划整体：解决「重复点生成越点越差」——append 每轮只能挑没被占的，几轮下来是边角料拼凑；
  // regroup 让 AI 看到已有全部子项后重新给一份完整划分（复用已有的 + 补齐缺失的，**不删任何节点**）。
  const regroup = el("input"); regroup.type = "checkbox"; regroup.id = "regroupChk";
  const regroupLbl = el("label", null, "重新规划整体"); regroupLbl.prepend(regroup);
  row.appendChild(regroupLbl);
  row.appendChild(el("span", "hint-inline", "（重复生成前建议勾上）"));
  const demo = el("input"); demo.type = "checkbox"; demo.id = "demoChk";
  const demoLbl = el("label", null, "demo（无 Key 也能试）"); demoLbl.prepend(demo);
  row.appendChild(demoLbl);
  // 联网检索（2026-09-24）：勾上后生成前先用必应搜一次节点标题+要求，把结果锚进 prompt。
  // 解决「纯记忆生成」对领域知识类主题（如软考考纲）自由发挥、结构跑偏的问题。默认不勾——
  // 大多数节点是纯组织性内容，搜了反而掺噪声、多等几秒。
  const web = el("input"); web.type = "checkbox"; web.id = "webChk";
  const webLbl = el("label", null, "🔍 联网检索"); webLbl.prepend(web);
  row.appendChild(webLbl);
  row.appendChild(el("span", "hint-inline", "（考纲/标准/技术选型类主题建议勾上）"));
  // regroup 只对「续写一层」有意义（分层大纲本就是一次整体生成），层>1 时禁用并解释
  const syncRegroup = () => {
    const multi = (parseInt(depth.value, 10) || 1) > 1;
    regroup.disabled = multi;
    regroupLbl.style.opacity = multi ? ".45" : "1";
    regroupLbl.title = multi
      ? "「重新规划整体」只对「续写一层」（层=1）有意义：分层大纲本就是一次整体生成的"
      : "把已有子项作为整体重新梳理：复用已有的、补齐缺失的（不会删除任何节点，未纳入新划分的会单独列出）";
  };
  depth.oninput = syncRegroup; syncRegroup();
  ai.appendChild(row);

  const go = el("button", "primary", "生成子节点 ✨");
  go.title = "让 AI 为该节点生成不重复的子节点（条数留空=由 AI 决定；层>1 时生成整棵轮廓）";
  go.onclick = () => aiExpand(node.id, parseInt(cnt.value, 10) || 0, parseInt(depth.value, 10) || 1, instr.value, go, regroup.checked);
  // 「展开本节点」= 让 AI 写**当前节点自己**的摘要与正文（不改子节点）。
  // 与「生成子节点」是两件不同的事，此前只有后者，导致「我想让 AI 把这一节讲清楚」无处可施。
  const en = el("button", "primary", "展开本节点 ✨");
  en.title = "让 AI 为当前节点自己写摘要 + 正文（Markdown），不改动子节点";
  en.onclick = () => aiEnrich(node.id, instr.value, en);
  const btnRow = el("div", "ai-row");
  btnRow.appendChild(go);
  btnRow.appendChild(en);
  ai.appendChild(btnRow);
  ai.appendChild(el("span", "hint-inline", "真实 AI：生成子节点约 20–90 秒，展开本节点约 30–160 秒；期间请勿关页面"));
  box.appendChild(ai);

  // 删除节点（仅编辑模式）
  const del = el("button", "tiny", "删除此节点（级联）");
  del.onclick = () => deleteNode(node.id);
  box.appendChild(del);

  if (editRefs) setTimeout(() => editRefs.title.focus(), 0);
}
function cssEsc(s) { return String(s).replace(/["\\]/g, "\\$&"); }

/* 标签展示：editable=true 时可增删（编辑模式），否则只读 */
function appendTags(box, node, editable) {
  box.appendChild(el("div", "rd-label-inline", "标签"));
  const wrap = el("div", "tags");
  const renderChips = () => {
    wrap.innerHTML = "";
    const tags = node._tags || [];
    if (!tags.length) wrap.appendChild(el("span", "sr-path", "（无）"));
    for (const tag of tags) {
      const chip = el("span", "tag");
      chip.appendChild(el("span", null, tag));
      if (editable) {
        const x = el("span", "x", "×");
        x.title = "移除标签";
        x.onclick = async () => {
          try {
            await API.tagRemove({ node_id: node.id, tag });
            node._tags = (node._tags || []).filter((t) => t !== tag);
            renderChips(); toast("已移除标签");
          } catch (e) { toast("移除失败：" + e.message, true); }
        };
        chip.appendChild(x);
      }
      wrap.appendChild(chip);
    }
  };
  renderChips();
  box.appendChild(wrap);
  if (!editable) return;
  const add = el("div", "tag-add");
  const inp = el("input"); inp.placeholder = "输入标签后回车";
  const btn = el("button", "tiny", "＋ 标签");
  const doAdd = async () => {
    const tag = inp.value.trim();
    if (!tag) return;
    try {
      await API.tagAdd({ node_id: node.id, tag });
      node._tags = [...new Set([...(node._tags || []), tag])];
      inp.value = ""; renderChips(); toast("已加标签");
    } catch (e) { toast("加标签失败：" + e.message, true); }
  };
  btn.onclick = doAdd;
  inp.onkeydown = (e) => { if (e.key === "Enter") { e.preventDefault(); doAdd(); } };
  add.appendChild(inp); add.appendChild(btn);
  box.appendChild(add);
}

async function saveNode(id) {
  const n = state.nodes[id]; if (!n) return;
  try {
    await API.nodeUpdate({ id, title: n.title, summary: n.summary, body: n.body });
  } catch (e) { toast("保存失败：" + e.message, true); }
}

/* 立刻落地还在防抖窗口里的右栏编辑。
 * onInput 已把输入值同步进 state.nodes，故这里只要触发一次保存即可。
 * 用于「即将切换/取消选中」——否则刚敲完的 400ms 内的内容会被直接丢弃
 * （「点空白取消选中」会新增这条丢数据的路径，故必须在此堵上）。 */
function flushPendingSave() {
  if (!saveTimer || state.selected == null) return;
  clearTimeout(saveTimer);
  saveTimer = null;
  saveNode(state.selected);
}
/* ---------- AI 流式生成（边生成边显示） ----------
 * 同步路径下 AI 要 20–160 秒才一次性返回，界面干等很难受。流式路径分三段：
 *   ① 调 expand(dry_run) 取组装好的 system/prompt（秒回，prompt 口径仍只在后端，前端不重写）
 *   ② 打 REST SSE 端点 /api/capabilities/invoke/stream，逐块收 AI 输出 → 实时显示已写出的条目
 *   ③ 解析完整输出后回传 expand(items_json) 落库（去重/截断/记录都在后端，与同步路径同口径）
 * 任一步失败都回退到同步路径，保证「功能不丢、只是慢一点」。
 */

// 从流式累计文本里实时抽出**已写完**的 title。
// 只匹配闭合的 `"title": "..."` —— 正在写的那一条匹配不上，所以不会显示半截标题。
function titlesSoFar(text) {
  const out = [];
  const re = /"title"\s*:\s*"((?:[^"\\]|\\.)*)"/g;
  let m;
  while ((m = re.exec(text)) !== null) {
    let v = m[1];
    try { v = JSON.parse('"' + v + '"'); } catch { /* 转义失败就保持原文 */ }
    if (v && out.indexOf(v) < 0) out.push(v);
  }
  return out;
}

// 把 AI 的完整输出解析成数组（与后端 askAI 同口径：剥代码围栏 → 兜底截取数组 → 包装容错 → 归一化）
function parseAIArray(text) {
  let t = String(text || "").trim();
  const fenced = t.match(/^```[a-zA-Z]*\s*\n([\s\S]*)\n```$/) || t.match(/^```[a-zA-Z]*\s*\n([\s\S]*)```$/);
  if (fenced) t = fenced[1].trim();
  let v = null;
  try { v = JSON.parse(t); } catch { v = null; }
  if (v == null) {
    // 兜底：模型可能在 JSON 前后夹了说明文字，取最外层 [ ... ]
    const i = t.indexOf("["), j = t.lastIndexOf("]");
    if (i >= 0 && j > i) { try { v = JSON.parse(t.slice(i, j + 1)); } catch { v = null; } }
  }
  if (!Array.isArray(v) && v && typeof v === "object") {
    for (const k of ["items", "data", "nodes", "children", "result"]) {
      if (Array.isArray(v[k])) { v = v[k]; break; }
    }
  }
  if (!Array.isArray(v)) return null;
  return v.map((it) => (typeof it === "string" ? { title: it } : it)).filter((it) => it && it.title);
}

// 卡死判据 —— **看是否还在出数据，而不是看总耗时**（与服务端 AI 层同口径，2026-09-23）：
//   - 空闲：超过 AI_STALL_IDLE_MS 没有任何新字符 ⇒ 疑似假死
//     （服务端 Idle = GEAR_AI_IDLE_TIMEOUT，默认 60s 无新增即判死并终止流，故前端阈值取更小值先给提示）
//   - 空转：连续 AI_STALL_REPEAT 个增量完全相同 ⇒ 疑似周期性吐同一段（正常流式不会连续完全一致）
// 注意：AI 只是**慢**但一直在出内容时，绝不判死 —— 总耗时由服务端 Total(900s) 兜底。
const AI_STALL_IDLE_MS = 45000;
const AI_STALL_REPEAT = 5;

// 打 REST SSE 端点逐块读 AI 输出；每收到增量就回调累计文本（供 UI 实时刷新）。
// 用 fetch + ReadableStream 而非 EventSource —— 后者不支持 POST。
// opts: { signal?: AbortSignal, onStall?: (kind: "idle"|"repeat", gapMs: number) => void }
async function streamComplete(system, prompt, onDelta, opts) {
  const o = opts || {};
  // 带上设置面板里保存的 AI 配置（key/model/端点）——SSE 直调 ext.ai 不经过后端 Tool，
  // 不带的话会回退环境变量，界面里配了等于没配。
  // 注意：settings_get 返回的字段名是 ai_*（对应 config.db 里的 ai.* 键），ext.ai 调用参数名不带前缀（api_key/base_url/model/provider），
  // 必须改名；原样塞 ai_* 会被 ext.ai 静默忽略（多出来的字段不报错），回退后表现为 401 Token invalid。
  const s = state.aiCfg || {};
  const cfg = {};
  if (s.ai_provider) cfg.provider = s.ai_provider;
  if (s.ai_base_url) cfg.base_url = s.ai_base_url;
  if (s.ai_model) cfg.model = s.ai_model;
  if (s.ai_api_key) cfg.api_key = s.ai_api_key;
  const res = await fetch("/api/capabilities/invoke/stream", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name: "ext.ai", method: "complete_stream",
      args: Object.assign({ system, prompt, format: "json" }, cfg) }),
    signal: o.signal,
  });
  if (!res.ok) throw new Error("流式接口返回 " + res.status);
  if (!res.body) throw new Error("浏览器不支持流式读取（res.body 为空）");
  const reader = res.body.getReader();
  const dec = new TextDecoder();
  let buf = "", acc = "";
  let lastAt = Date.now();
  const recent = [];
  const timer = setInterval(() => {
    const gap = Date.now() - lastAt;
    if (gap > AI_STALL_IDLE_MS && o.onStall) o.onStall("idle", gap);
  }, 2000);
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      const lines = buf.split("\n");
      buf = lines.pop() || "";
      for (const line of lines) {
        const s = line.trim();
        if (!s.startsWith("data:")) continue;
        const payload = s.slice(5).trim();
        if (!payload) continue;
        let obj = null;
        try { obj = JSON.parse(payload); } catch { continue; }
        // 服务端出错发 event:error + {"error":...}。必须立刻抛出，把真实原因
        // （key 无效 / 端点不通 / 超时）透给用户——此前被静默吞掉，acc 为空，
        // 最后报成误导性的「未能解析出子主题」（2026-09-24 实测踩坑）。
        if (obj && typeof obj.error === "string" && obj.error) {
          throw new Error("AI 服务报错: " + obj.error);
        }
        if (obj && typeof obj.delta === "string" && obj.delta) {
          acc += obj.delta;
          lastAt = Date.now();
          recent.push(obj.delta);
          if (recent.length > AI_STALL_REPEAT) recent.shift();
          if (recent.length === AI_STALL_REPEAT && recent[0] !== "" && recent.every((x) => x === recent[0])) {
            if (o.onStall) o.onStall("repeat", 0);
          }
          onDelta(acc);
        }
      }
    }
  } finally { clearInterval(timer); }
  return acc;
}

// 中栏顶部的「生成中」进度条：实时列出已经生成出来的条目
function showStreamBar() {
  let bar = $("#streamBar");
  if (!bar) {
    bar = el("div", "stream-bar");
    bar.id = "streamBar";
    $("#mid").insertBefore(bar, $("#nodeTree"));
  }
  bar.style.display = "";
  return bar;
}
function updateStreamBar(titles, stage, opts) {
  const o = opts || {};
  const bar = $("#streamBar");
  if (!bar) return;
  bar.innerHTML = "";
  const headRow = el("div", "stream-head-row");
  const label = o.warn
    ? o.warn
    : ((stage === "写入中" ? "⏳ 正在写入…" : "⏳ AI 正在生成…") + `（已 ${titles.length} 项）`);
  headRow.appendChild(el("span", o.warn ? "stream-warn" : "stream-head", label));
  if (o.onAbort) {
    const ab = el("button", "tiny", "中止");
    ab.title = "停止本次生成（已生成的内容不会写入）";
    ab.onclick = o.onAbort;
    headRow.appendChild(ab);
  }
  bar.appendChild(headRow);
  const list = el("div", "stream-list");
  for (const t of titles.slice(-40)) list.appendChild(el("div", "stream-item", t));
  bar.appendChild(list);
}
function hideStreamBar() { const b = $("#streamBar"); if (b) b.style.display = "none"; }

/* 生成前联网检索：勾选「🔍 联网检索」时调用，把必应搜索结果转成 prompt 锚定资料。
 * 检索失败不阻塞主流程——toast 提示后返回空串，等价于「无检索直接生成」（降级路径）。 */
async function webContextFor(title, instruction) {
  if (!$("#webChk") || !$("#webChk").checked) return "";
  const q = [title, instruction].filter((x) => x && x.trim()).join(" ").trim();
  if (!q) return "";
  try {
    const out = await API.webSearch({ query: q });
    const n = (out.results || []).length;
    if (n) toast(`🔍 已检索到 ${n} 条参考资料，正在生成…`);
    return out.text || "";
  } catch (e) {
    // 剥掉后端带出来的 stack traceback，toast 只留人能读的原因
    const why = String(e.message || "").replace(/。?\s*stack traceback[\s\S]*$/, "");
    toast("联网检索不可用（" + why + "），改为直接生成", true);
    return "";
  }
}

async function aiExpand(id, count, depth, instruction, btn, regroup) {
  depth = depth || 1;
  const node = state.nodes[id];
  const demo = $("#demoChk") ? $("#demoChk").checked : false;
  // 「重新规划」只对续写一层有意义（层>1 本就是一次整体生成大纲）
  const mode = (regroup && depth === 1) ? "regroup" : "append";
  const oldTxt = btn ? btn.textContent : "";
  if (btn) { btn.disabled = true; btn.textContent = "⏳ 生成中…"; }

  let out = null;
  try {
    if (demo) {
      // demo 秒回，不需要流式
      toast("demo 模式，直接生成…");
      out = await API.expand({ node_id: id, count, depth, instruction: instruction || "", demo: true, mode });
    } else {
      // ⓪ 联网检索（勾选时）：先搜后生成，资料经 web_context 进 prompt（后端 dry_run 组装时带上）
      const webctx = await webContextFor(node ? node.title : "", instruction);
      // ① 取 prompt（后端组装，秒回）
      const p = await API.expand({ node_id: id, count, depth, instruction: instruction || "", mode, dry_run: true, web_context: webctx });
      showStreamBar();
      const ctl = new AbortController();
      const stopIt = () => ctl.abort();
      updateStreamBar([], "生成中", { onAbort: stopIt });
      let acc = "";
      try {
        // ② 流式生成：边收边把已写完整的条目显示出来
        acc = await streamComplete(p.system, p.prompt,
          (cur) => updateStreamBar(titlesSoFar(cur), "生成中", { onAbort: stopIt }),
          {
            signal: ctl.signal,
            onStall: (kind, gap) => updateStreamBar(titlesSoFar(acc), "生成中", {
              onAbort: stopIt,
              warn: kind === "idle"
                ? `⚠ 已 ${Math.round(gap / 1000)} 秒没有新输出，AI 可能卡住（服务端空闲超时会自动终止；总耗时不判死）`
                : "⚠ AI 似乎在重复输出同一段，可能已空转",
            }),
          });
      } catch (se) {
        if (ctl.signal.aborted) { hideStreamBar(); toast("已中止生成（未写入任何节点）"); return; }
        // AI 服务本身报错（key/端点/超时等）：转同步也没用，直接把真实原因报给用户
        if (String(se.message).startsWith("AI 服务报错")) {
          hideStreamBar();
          toast(se.message + "（点左栏 ⚙ 检查 AI 配置）", true);
          return;
        }
        // 流式不可用（端点不通 / 浏览器不支持）→ 回退同步路径，功能不丢，只是要等
        hideStreamBar();
        toast("流式不可用（" + se.message + "），改用同步生成…（约 20–160 秒）");
        out = await API.expand({ node_id: id, count, depth, instruction: instruction || "", demo: false, mode, web_context: webctx });
      }
      if (out === null) {
        // ③ 解析完整输出后回传后端落库
        const items = parseAIArray(acc);
        // 带上原始输出：否则「解析不出东西」无从判断是模型没按格式、还是被截断
        if (!items || !items.length) {
          if (!acc.trim()) {
            throw new Error("AI 没有返回任何内容（流为空）。请检查 ⚙ AI 配置里的 Key 与端点是否有效");
          }
          throw new Error("未能解析出子主题（AI 输出可能不符合 JSON 或已被截断）。原始输出前 200 字符: " + acc.slice(0, 200));
        }
        updateStreamBar(items.map((x) => x.title), "写入中");
        out = await API.expand({ node_id: id, depth, mode, items_json: JSON.stringify(items) });
      }
    }
    hideStreamBar();
    const created = (out.created || []).length;
    const skipped = out.skipped || 0;
    // 重新载入该节点下方窗口（depth>1 会生成多层），并展开整条新链路，保证可见。
    // ⚠️ 必须**先让子列表缓存失效**（_childrenLoaded=false）：ensureChildren 见到
    //    _childrenLoaded===true 会直接早退（它的语义是「已加载过就不用再问服务端」），
    //    而 AI 刚在服务端插入了新节点、本地缓存并不知道 —— 不失效则新节点永远进不了
    //    state.nodes，renderMiddle 渲染的仍是旧 _children，表现为「生成成功但中栏不刷新，
    //    要手动刷整页才看到」（整页刷新后 state.nodes 从空对象重建，缓存标记自然没设，故能显示）。
    //    addChild / deleteNode 都做了同样的失效处理，此处曾漏（2026-09-23 修）。
    node._childrenLoaded = false;
    node._expanded = true;
    await ensureSubtree(id, MAX_REL);
    const expandChain = (nid) => {
      const n = state.nodes[nid];
      if (!n) return;
      n._expanded = true;
      for (const c of (n._children || [])) expandChain(c.id);
    };
    expandChain(id);
    renderMiddle();
    // regroup 下 skipped 的语义是「复用已有」（不是「跳过重复」），toast 措辞要分开，
    // 并把「未纳入新划分」的项数报出来（它们**没被删除**，需要用户自己决定去留）。
    const bits = [];
    if (mode === "regroup") {
      bits.push(`新增 ${created} 个`, `复用 ${out.kept || 0} 个`);
      const ex = (out.extras || []).length;
      if (ex) bits.push(`${ex} 个未纳入新划分（未删除）`);
    } else {
      bits.push(`已生成 ${created} 个`);
      if (skipped) bits.push(`跳过 ${skipped} 个重复`);
    }
    if (depth > 1) bits.push(`深度 ${depth}`);
    if (demo) bits.push("demo");
    toast(bits.join(" · "));
  } catch (e) { toast("生成失败：" + e.message, true); }
  finally { if (btn) { btn.disabled = false; btn.textContent = oldTxt || "生成子节点 ✨"; } }
}

// 解析 AI 输出的「对象」结果（与后端 enrich 同步路径同口径：剥围栏 → 兜底截取 → 包装容错 → 字段别名）
function parseAIObject(text) {
  let t = String(text || "").trim();
  const fenced = t.match(/^```[a-zA-Z]*\s*\n([\s\S]*)\n```$/) || t.match(/^```[a-zA-Z]*\s*\n([\s\S]*)```$/);
  if (fenced) t = fenced[1].trim();
  let v = null;
  try { v = JSON.parse(t); } catch { v = null; }
  if (v == null) {
    const i = t.indexOf("{"), j = t.lastIndexOf("}");
    if (i >= 0 && j > i) { try { v = JSON.parse(t.slice(i, j + 1)); } catch { v = null; } }
  }
  if (!v || typeof v !== "object") return null;
  if (Array.isArray(v)) v = v[0] || null;
  if (!v) return null;
  for (const k of ["data", "result", "output"]) {
    if (v[k] && typeof v[k] === "object") { v = v[k]; break; }
  }
  const summary = v.summary || v.abstract || v.brief || "";
  const body = v.body || v.content || v.markdown || v.text || v.detail || "";
  if (!summary && !body) return null;
  return { summary, body };
}

// 从**可能还没写完**的 JSON 里抠出 body 的已生成片段，用于流式预览。
// 顺带把 JSON 转义还原（写作中的正文里 \n 就是换行）。
function partialBody(text) {
  const i = String(text || "").indexOf('"body"');
  if (i < 0) return "";
  let j = i + 6;
  while (j < text.length && text[j] !== '"') j++;
  if (j >= text.length) return "";
  j++;
  let out = "";
  while (j < text.length) {
    const ch = text[j];
    if (ch === "\\") {
      const nx = text[j + 1];
      if (nx === undefined) break;
      out += nx === "n" ? "\n" : nx;
      j += 2;
      continue;
    }
    if (ch === '"') break;
    out += ch;
    j++;
  }
  return out;
}

/* 右栏的「正在写」实时预览：把已收到的正文部分当 Markdown 渲染出来 */
function showEnrichStream(onAbort) {
  const box = $("#detail");
  box.innerHTML = "";
  const wrap = el("div", "stream-md");
  wrap.id = "enrichStream";
  const headRow = el("div", "stream-head-row");
  headRow.appendChild(el("span", "stream-head", "⏳ AI 正在写这一节…"));
  if (onAbort) {
    const ab = el("button", "tiny", "中止");
    ab.title = "停止本次生成（已生成的内容不会写入）";
    ab.onclick = onAbort;
    headRow.appendChild(ab);
  }
  wrap.appendChild(headRow);
  const md = el("div", "md");
  md.id = "enrichStreamMd";
  wrap.appendChild(md);
  box.appendChild(wrap);
  return wrap;
}
function updateEnrichStream(acc, warn) {
  const md = document.getElementById("enrichStreamMd");
  if (!md) return;
  const partial = partialBody(acc);
  md.innerHTML = mdToHtml(partial || "（AI 正在组织内容…）");
  const head = document.querySelector("#enrichStream .stream-head");
  if (!head) return;
  head.textContent = warn || `⏳ AI 正在写这一节…（已 ${partial.length} 字）`;
}
function hideEnrichStream() {
  const w = document.getElementById("enrichStream");
  if (w) w.remove();
}

/* AI 展开本节点：让 AI 写「当前节点自己」的摘要与正文（与 aiExpand 的「生成子节点」互补）。
 * 同样走流式：dry_run 取 prompt → SSE 逐块收 → 实时渲染正文 → 解析后回传落库。 */
async function aiEnrich(id, instruction, btn) {
  const node = state.nodes[id];
  const demo = $("#demoChk") ? $("#demoChk").checked : false;
  const oldTxt = btn ? btn.textContent : "";
  if (btn) { btn.disabled = true; btn.textContent = "⏳ 展开中…"; }

  let out = null;
  try {
    if (demo) {
      toast("demo 模式，直接生成…");
      out = await API.enrich({ node_id: id, instruction: instruction || "", demo: true });
    } else {
      const webctx = await webContextFor(node ? node.title : "", instruction);
      const p = await API.enrich({ node_id: id, instruction: instruction || "", dry_run: true, web_context: webctx });
      const ctl = new AbortController();
      showEnrichStream(() => ctl.abort());
      let acc = "";
      try {
        acc = await streamComplete(p.system, p.prompt, updateEnrichStream, {
          signal: ctl.signal,
          onStall: (kind, gap) => updateEnrichStream(acc, kind === "idle"
            ? `⚠ 已 ${Math.round(gap / 1000)} 秒没有新输出，AI 可能卡住（服务端空闲超时会自动终止；总耗时不判死）`
            : "⚠ AI 似乎在重复输出同一段，可能已空转"),
        });
      } catch (se) {
        if (ctl.signal.aborted) { hideEnrichStream(); toast("已中止生成（未写入内容）"); return; }
        // AI 服务本身报错（key/端点/超时等）：转同步也没用，直接把真实原因报给用户
        if (String(se.message).startsWith("AI 服务报错")) {
          hideEnrichStream();
          toast(se.message + "（点左栏 ⚙ 检查 AI 配置）", true);
          return;
        }
        hideEnrichStream();
        toast("流式不可用（" + se.message + "），改用同步生成…（约 30–160 秒）");
        out = await API.enrich({ node_id: id, instruction: instruction || "", demo: false, web_context: webctx });
      }
      if (out === null) {
        const parsed = parseAIObject(acc);
        if (!parsed) {
          if (!acc.trim()) throw new Error("AI 没有返回任何内容（流为空）。请检查 ⚙ AI 配置里的 Key 与端点是否有效");
          throw new Error("未能解析出摘要/正文（AI 输出可能不符合 JSON 格式）。前 200 字符: " + acc.slice(0, 200));
        }
        out = await API.enrich({ node_id: id, result_json: JSON.stringify(parsed) });
      }
    }
    hideEnrichStream();
    if (node) { node.summary = out.summary || ""; node.body = out.body || ""; }
    renderDetail();   // 立即显示最终内容（阅读模式即 Markdown 渲染结果）
    const sl = (out.summary || "").length, bl = (out.body || "").length;
    toast(`已写入摘要（${sl} 字）与正文（${bl} 字）${demo ? "（demo）" : ""}`);
  } catch (e) { hideEnrichStream(); toast("展开失败：" + e.message, true); }
  finally { if (btn) { btn.disabled = false; btn.textContent = oldTxt || "展开本节点 ✨"; } }
}

/* ---------- 选择 / 增删 ---------- */
/* 取消选中（中栏点空白区、或按 Esc）。
 *
 * 为什么需要它：中栏加号按钮的语义挂在校中状态上（选中 → 「＋ 子节点」，
 * 未选中 → 「＋ 根节点」，见 updateMidHeader），若没有「取消选中」的入口，
 * 用户选中过任意节点后就再也回不到「加根节点」的状态了。
 */
function clearSelection() {
  if (state.selected == null) return;
  flushPendingSave();   // 先把右栏可能还在防抖中的编辑落地，再取消选中
  state.selected = null;
  LS.node = "";
  renderMiddle(); renderDetail(); updateMidHeader();
}

async function selectNode(id, remember) {
  flushPendingSave();   // 切走前同样先落地，避免快速点两个节点时丢掉前一个的编辑
  state.selected = id;
  if (remember) LS.node = id;
  const node = state.nodes[id];
  if (!node || node.body === undefined) {
    // 详情需要 body，node_children 不回 body → 用 node_get 补全
    try {
      const full = await API.nodeGet({ id });
      Object.assign(state.nodes[id], full);
    } catch (e) { toast("读取节点详情失败：" + e.message, true); }
  }
  // 标签：首次选中该节点时拉取
  if (state.nodes[id] && state.nodes[id]._tags === undefined) {
    try { const t = await API.tagList({ node_id: id }); state.nodes[id]._tags = t.tags || []; }
    catch (_) { state.nodes[id]._tags = []; }
  }
  renderMiddle(); renderDetail(); updateMidHeader();
}
async function addChild(parentId) {
  if (!state.currentTree) { toast("请先选择一棵思维树", true); return; }
  const title = await ask({
    message: parentId ? "新增子节点" : "新增根节点",
    input: true, placeholder: "输入节点标题",
  });
  if (title === null || !title) return;
  const id = genId();
  const payload = { id, tree_id: state.currentTree, title };
  if (parentId) payload.parent_id = parentId;
  try {
    await API.nodeCreate(payload);
    const parent = parentId ? state.nodes[parentId] : null;
    if (parent) { parent._childrenLoaded = false; parent._expanded = true; await ensureChildren(parentId); }
    else { await loadRoots(); await ensureSubtree(null, MAX_REL); }
    renderMiddle();
    selectNode(id, true);
  } catch (e) { toast("创建失败：" + e.message, true); }
}
async function deleteNode(id) {
  const node = state.nodes[id];
  const label = node ? node.title : id;
  if (!(await ask({
    message: `删除「${label}」及其所有子孙？\n此操作不可撤销。`,
    okText: "删除", danger: true,
  }))) return;
  try {
    await API.nodeDelete({ id });
    const parentId = node && node._parent;
    if (state.focusId === id) state.focusId = parentId || null;   // 删掉焦点根 → 焦点上移到父
    delete state.nodes[id];
    if (parentId && state.nodes[parentId]) { state.nodes[parentId]._childrenLoaded = false; await ensureChildren(parentId); }
    else { await loadRoots(); await ensureSubtree(null, MAX_REL); }
    if (state.selected === id) state.selected = null;
    renderMiddle(); renderDetail(); updateMidHeader();
    toast("已删除");
  } catch (e) { toast("删除失败：" + e.message, true); }
}

/* ---------- 键盘流 ---------- */
function onKey(e) {
  if (state.selected == null && !state.currentTree) return;
  const idx = state.visible.indexOf(state.selected);
  switch (e.key) {
    case "ArrowDown": {
      e.preventDefault();
      const nxt = state.visible[Math.min((idx < 0 ? -1 : idx) + 1, state.visible.length - 1)];
      if (nxt) selectNode(nxt, true);
      break;
    }
    case "ArrowUp": {
      e.preventDefault();
      const prv = state.visible[Math.max((idx < 0 ? 1 : idx) - 1, 0)];
      if (prv) selectNode(prv, true);
      break;
    }
    case "ArrowRight": {
      const node = state.nodes[state.selected];
      if (node && node.child_count > 0) { e.preventDefault(); drillInto(state.selected); }
      break;
    }
    case "ArrowLeft": {
      if (state.focusId) { e.preventDefault(); focusUp(); }
      else {
        const node = state.nodes[state.selected];
        const p = node && node._parent;
        if (p && state.visible.indexOf(p) >= 0) { e.preventDefault(); selectNode(p, true); }
      }
      break;
    }
    case "Tab": {
      e.preventDefault();
      addChild(state.selected);
      break;
    }
    case "Delete":
    case "Backspace": {
      if (state.selected != null) { e.preventDefault(); deleteNode(state.selected); }
      break;
    }
    case "Escape": {
      // 与「点中栏空白」同一语义：取消选中，回到可加根节点的状态
      if (state.selected != null) { e.preventDefault(); clearSelection(); }
      break;
    }
    case "Enter": {
      if (e.ctrlKey || e.metaKey) { e.preventDefault(); if (state.selected && !state.readMode) aiExpand(state.selected, 5, 1, $("#instr") ? $("#instr").value : ""); }
      break;
    }
  }
}

/* ---------- 启动 ---------- */
async function boot() {
  // 详情栏每次打开都默认阅读模式（东东 2026-09-24）：编辑是临时动作，不再跨会话持久化。
  state.readMode = true;
  localStorage.removeItem("mindstorm.mode");   // 清掉旧版本的 mode 持久化残留
  $("#newTree").onclick = newTree;
  $("#aiCfgBtn").onclick = openSettings;
  // 错误 toast 停留 8s，点击可立即关掉（配合 toast() 的时长区分）
  $("#toast").onclick = () => $("#toast").classList.remove("show");
  $("#addBtn").onclick = () => addChild(state.selected);
  $("#navUp").onclick = focusUp;
  $("#navDown").onclick = () => { if (state.selected) drillInto(state.selected); };
  $("#navHome").onclick = focusHome;
  $("#modeToggle").onclick = () => {
    state.readMode = !state.readMode;
    LS.mode = state.readMode ? "read" : "edit";
    renderDetail();
  };
  $("#searchBox").addEventListener("input", onSearchInput);
  $("#nodeTree").addEventListener("keydown", onKey);
  // 点击中栏：落在节点行上由行自己处理选中；落在空白处 = 取消选中
  // （否则「选中过节点之后就加不了根节点」——见 clearSelection 注释）
  $("#nodeTree").addEventListener("click", (e) => {
    $("#nodeTree").focus();
    if (!e.target.closest(".node-row")) clearSelection();
  });

  try {
    const hz = await fetch("/api/healthz").then((r) => r.json());
    if (!hz || hz.status !== "ok") throw new Error("后端状态异常");
    showBanner("");
  } catch (e) {
    showBanner("⚠ 无法连接后端（gear serve 未启动？）：" + e.message);
    return;
  }
  try {
    await API.init({});   // 幂等建表
    await loadSettings();
    await loadTrees();
    if (LS.tree && state.trees.some((t) => t.id === LS.tree)) {
      await selectTree(LS.tree);
    } else {
      renderTrees(); renderMiddle(); renderDetail(); updateMidHeader();
    }
  } catch (e) { showBanner("⚠ 初始化失败：" + e.message); }
}
boot();
