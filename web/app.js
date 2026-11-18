'use strict';

/* ============ 基础工具 ============ */
const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

function el(tag, cls, text) {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  if (text != null) node.textContent = text;
  return node;
}

function uid() {
  if (window.crypto && crypto.randomUUID) return crypto.randomUUID();
  return 's' + Date.now().toString(36) + Math.random().toString(36).slice(2, 8);
}

function fmt(t) {
  if (!Number.isFinite(t) || t < 0) t = 0;
  const m = Math.floor(t / 60);
  const s = t - m * 60;
  return String(m).padStart(2, '0') + ':' + s.toFixed(2).padStart(5, '0');
}

function clock() {
  const d = new Date();
  return String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
}

function isVideoFile(file) {
  return !!file && (file.type.startsWith('video/') || /\.(mp4|mov|m4v|webm|mkv|avi|mpg|mpeg|ts)$/i.test(file.name));
}

function hasFileDrag(e) {
  return !!e.dataTransfer && Array.from(e.dataTransfer.types || []).includes('Files');
}

function hasClipDrag(e) {
  return !!e.dataTransfer && Array.from(e.dataTransfer.types || []).includes('application/x-clip');
}

function isTypingTarget(target) {
  if (!target) return false;
  const tag = target.tagName;
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || target.isContentEditable;
}

async function api(path, opts = {}) {
  const { method = 'GET', body, raw } = opts;
  const headers = {};
  let payload;
  if (raw != null) {
    headers['Content-Type'] = 'application/json';
    payload = raw;
  } else if (body != null) {
    headers['Content-Type'] = 'application/json';
    payload = JSON.stringify(body);
  }
  const res = await (async () => {
    try {
      return await fetch(path, { method, headers, body: payload });
    } catch (e) {
      // fetch 直接抛错说明请求根本没有发出去（断网、服务没启动、被拦截）
      const err = new Error('请求没有发出去：网络中断或服务未启动（' + ((e && e.message) || 'fetch failed') + '）');
      err.network = true;
      throw err;
    }
  })();
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch (_) { /* ignore */ }
  if (!res.ok) {
    const err = new Error((data && data.error) || `请求失败（${res.status}）`);
    if (data && data.stage) err.stage = data.stage;
    throw err;
  }
  return data;
}

function uploadFile(file, url, onProgress, opts = {}) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('POST', url);
    if (opts.onXhr) opts.onXhr(xhr);
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable && onProgress) onProgress(e.loaded / e.total);
    };
    xhr.onload = () => {
      let data = null;
      try { data = JSON.parse(xhr.responseText); } catch (_) { /* ignore */ }
      if (xhr.status >= 200 && xhr.status < 300 && data) resolve(data);
      else reject(new Error((data && data.error) || `上传失败（${xhr.status}）`));
    };
    xhr.onerror = () => reject(new Error('网络错误，上传失败'));
    xhr.onabort = () => {
      const err = new Error('已取消上传');
      err.aborted = true;
      reject(err);
    };
    const fd = new FormData();
    fd.append('file', file);
    xhr.send(fd);
  });
}

/* ============ 上传队列（全局单队列，可暂停 / 取消 / 重试） ============ */
const uploads = { items: [], active: null, paused: false };

function queueUpload({ file, url, label, onProgress, onSuccess, onFail, onCancel }) {
  const key = url + '|' + file.name + '|' + file.size + '|' + (file.lastModified || 0);
  const found = uploads.items.find((it) => it.key === key);
  if (found) return found.handle;
  const item = {
    key,
    file,
    url,
    label: label || file.name,
    status: '等待中',
    progress: 0,
    onProgress,
    onSuccess,
    onFail,
    onCancel,
    started: false,
    done: false,
    failed: false,
  };
  item.handle = {
    cancel: () => cancelUpload(item),
    retry: () => retryUpload(item),
  };
  uploads.items.push(item);
  renderUploads();
  pumpUploads();
  return item.handle;
}

function pumpUploads() {
  if (uploads.active || uploads.paused) return;
  const next = uploads.items.find((it) => !it.started && !it.done && !it.failed);
  if (!next) return;
  next.started = true;
  uploads.active = next;
  next.status = '上传中 0%';
  updateUploadItem(next);
  uploadFile(next.file, next.url, (p) => {
    next.progress = p;
    next.status = '上传中 ' + Math.round(p * 100) + '%';
    updateUploadItem(next);
    if (next.onProgress) next.onProgress(p);
  }, {
    onXhr: (xhr) => {
      next.xhr = xhr;
      if (next.cancelled) xhr.abort();
    },
  })
    .then((data) => finishUpload(next, null, data))
    .catch((err) => finishUpload(next, err));
}

function finishUpload(item, err, data) {
  if (uploads.active === item) uploads.active = null;
  item.xhr = null;

  if (err) {
    // 取消是用户主动行为，直接移除；失败则保留条目，等待「重试」
    if (item.cancelled || err.aborted) {
      removeUpload(item);
      if (item.onCancel) item.onCancel(err);
      pumpUploads();
      return;
    }
    item.started = false;
    item.failed = true;
    item.progress = 0;
    item.status = '失败：' + (err.message || '未知错误');
    renderUploads();
    if (item.onFail) item.onFail(err);
    pumpUploads();
    return;
  }

  if (item.done) return;
  item.done = true;
  item.failed = false;
  item.progress = 1;
  item.status = '已完成';
  updateUploadItem(item);
  renderUploads();
  if (item.onSuccess) item.onSuccess(data);
  // 成功项稍后自动从列表清理
  setTimeout(() => removeUpload(item), 1500);
  pumpUploads();
}

function removeUpload(item) {
  const idx = uploads.items.indexOf(item);
  if (idx >= 0) uploads.items.splice(idx, 1);
  renderUploads();
}

// 取消与暂停互不干扰：暂停只阻止「开始下一个」，不打断正在上传的条目。
function cancelUpload(item) {
  if (!item) return;
  if (item.failed || item.done) {
    removeUpload(item);
    return;
  }
  item.cancelled = true;
  if (item.xhr) {
    try { item.xhr.abort(); } catch (_) { /* ignore */ }
    return;
  }
  removeUpload(item);
}

function retryUpload(item) {
  if (!item || !item.failed) return;
  item.failed = false;
  item.done = false;
  item.started = false;
  item.cancelled = false;
  item.progress = 0;
  item.status = uploads.paused ? '等待中（已暂停）' : '等待中';
  renderUploads();
  pumpUploads();
}

function setUploadsPaused(paused) {
  uploads.paused = paused;
  uploads.items.forEach((it) => {
    if (!it.started && !it.done && !it.failed) {
      it.status = paused ? '等待中（已暂停）' : '等待中';
      updateUploadItem(it);
    }
  });
  renderUploads();
  if (!paused) pumpUploads();
}

function renderUploads() {
  const show = uploads.items.length > 0 || !!uploads.active;
  uploadBox.classList.toggle('hidden', !show);
  uploadCount.textContent = uploads.items.length ? String(uploads.items.length) : '';
  if (uploadToggle) {
    uploadToggle.textContent = uploads.paused ? '继续' : '暂停';
    uploadToggle.classList.toggle('paused', uploads.paused);
  }
  uploadList.innerHTML = '';
  uploads.items.forEach((item) => uploadList.appendChild(buildUploadItem(item)));
}

function buildUploadItem(item) {
  const box = el('div', 'upload-item' + (item.failed ? ' failed' : ''));
  const top = el('div', 'upload-top');
  const name = el('span', 'upload-name', item.label);
  name.title = item.file.name;
  const actions = el('div', 'upload-actions');
  if (item.failed) {
    const retry = el('button', 'upload-retry', '重试');
    retry.type = 'button';
    retry.title = '用同一个文件重新上传';
    retry.addEventListener('click', () => retryUpload(item));
    actions.appendChild(retry);
  }
  const cancel = el('button', 'upload-cancel', '×');
  cancel.type = 'button';
  cancel.title = item.failed ? '从列表移除' : '取消上传';
  cancel.setAttribute('aria-label', '取消上传 ' + item.file.name);
  cancel.addEventListener('click', () => cancelUpload(item));
  actions.appendChild(cancel);
  top.appendChild(name);
  top.appendChild(actions);

  const meta = el('div', 'upload-meta', item.status);
  const bar = el('div', 'upload-bar');
  const fill = el('div');
  fill.style.width = Math.round(item.progress * 100) + '%';
  bar.appendChild(fill);

  box.appendChild(top);
  box.appendChild(meta);
  box.appendChild(bar);
  item.el = { box, meta, fill };
  return box;
}

function updateUploadItem(item) {
  if (!item.el || !item.el.box.isConnected) return;
  item.el.meta.textContent = item.status;
  item.el.fill.style.width = Math.round(item.progress * 100) + '%';
}

/* ============ 全局状态 ============ */
const state = {
  products: [],
  product: '',
  scripts: [],
  script: '',
  doc: null,
  dirty: false,
  clip: null,
  cuts: [],
  fps: 30,
  clipDuration: 0,
  toolsReady: false,
  splitting: false,
  // 第二批：脚本使用状态（后端 _脚本状态.json 里的 产品/脚本）
  used: new Set(),
  // 阶段三：协作
  regs: null,      // CRDT 寄存器（文档的真实状态）
  sess: null,      // 与服务器的实时连接
  peers: [],       // 同一脚本的其他在线同事
  myKey: '',       // 我正在编辑的单元格
  epoch: '',       // 文档世代，用来丢弃过期的本地缓存
};

const me = Collab.identity();

let rev = 0;
let loadSeq = 0;

/* ============ DOM 引用 ============ */
const productSelect = $('#productSelect');
const saveStatus = $('#saveStatus');
const scriptList = $('#scriptList');
const emptyState = $('#emptyState');
const editor = $('#editor');
const rowsEl = $('#rows');
const addRowBtn = $('#addRow');
const scriptTitle = $('#scriptTitle');
const crumbProduct = $('#crumbProduct');
const shotCount = $('#shotCount');
const addScriptBtn = $('#addScript');
const addProductBtn = $('#addProduct');
const delProductBtn = $('#delProduct');
const crumbSep = $('.crumb-sep');

const clipDrop = $('#clipDrop');
const clipFile = $('#clipFile');
const clipEditor = $('#clipEditor');
const clipVideo = $('#clipVideo');
const clipVideoWrap = $('#clipVideoWrap');
const clipName = $('#clipName');
const timeline = $('#timeline');
const tlProgress = $('#tlProgress');
const tlCuts = $('#tlCuts');
const clipTime = $('#clipTime');
const clearCutsBtn = $('#clearCuts');
const changeClipBtn = $('#changeClip');
const segmentsEl = $('#segments');
const segCount = $('#segCount');
const clipPlayHint = $('#clipPlayHint');
const smartCutBtn = $('#smartCut');
const cutSensitivity = $('#cutSensitivity');
const clipFps = $('#clipFps');
const ffStatus = $('#ffStatus');

const overlay = $('#overlay');
const overlayStage = $('#overlayStage');
const ovVideo = $('#ovVideo');
const ovLabel = $('#ovLabel');
const ovPlay = $('#ovPlay');
const ovTime = $('#ovTime');
const ovProgress = $('#ovProgress');

const modal = $('#modal');
const modalForm = $('#modalForm');
const modalTitle = $('#modalTitle');
const modalDesc = $('#modalDesc');
const modalInput = $('#modalInput');
const modalError = $('#modalError');
const modalConfirm = $('#modalConfirm');
const modalCancel = $('#modalCancel');

const toastEl = $('#toast');

const settingsModal = $('#settingsModal');
const settingsForm = $('#settingsForm');
const setFfmpeg = $('#setFfmpeg');
const setFfprobe = $('#setFfprobe');
const ffDetectInfo = $('#ffDetectInfo');
const settingsError = $('#settingsError');
const settingsCancel = $('#settingsCancel');

const peersEl = $('#peers');
const syncStateEl = $('#syncState');
const themeBtns = $$('[data-theme-btn]');
const themePanel = $('#themePanel');
const customBg = $('#customBg');
const customAccent = $('#customAccent');
const customText = $('#customText');
const customPanel = $('#customPanel');
const customRadius = $('#customRadius');
const customRadiusVal = $('#customRadiusVal');
const customFont = $('#customFont');
const customFontVal = $('#customFontVal');
const customImgPick = $('#customImgPick');
const customImgClear = $('#customImgClear');
const customImgFile = $('#customImgFile');
const customImgOpacity = $('#customImgOpacity');
const customImgOpacityVal = $('#customImgOpacityVal');
const customPalettes = $('#customPalettes');
const customSavePalette = $('#customSavePalette');
const customReset = $('#customReset');
const bgLayer = $('#bgLayer');
const sidebarEl = $('#sidebar');
const sidebarToggle = $('#sidebarToggle');
const scriptSearch = $('#scriptSearch');
const scriptFilter = $('#scriptFilter');
const searchResults = $('#searchResults');
const usageTabs = $$('#usageTabs .usage-tab');
const renumberBtn = $('#renumberBtn');
const batchBar = $('#batchBar');
const batchCount = $('#batchCount');
const batchSliceBtn = $('#batchSlice');
const batchDeleteBtn = $('#batchDelete');
const batchClearBtn = $('#batchClear');
const selectAll = $('#selectAll');
const clipPanel = $('#clipPanel');
const panelToggle = $('#panelToggle');
const panelClose = $('#panelClose');
const uploadBox = $('#uploadBox');
const uploadCount = $('#uploadCount');
const uploadList = $('#uploadList');
const uploadToggle = $('#uploadToggle');
const tableEl = $('.table');
const historyBtn = $('#historyBtn');
const historyModal = $('#historyModal');
const historyClose = $('#historyClose');
const historySub = $('#historySub');
const historyList = $('#historyList');
const exportBtn = $('#exportBtn');
const exportModal = $('#exportModal');
const exportClose = $('#exportClose');
const exportTarget = $('#exportTarget');
const exportPrecheck = $('#exportPrecheck');
const exportTemplateRow = $('#exportTemplateRow');
const exportTemplate = $('#exportTemplate');
const exportTemplateNote = $('#exportTemplateNote');
const exportDraftBtn = $('#exportDraftBtn');
const exportCsvBtn = $('#exportCsvBtn');
const exportRecheck = $('#exportRecheck');
const exportResult = $('#exportResult');
const exportPathBar = $('#exportPathBar');
const exportNasPath = $('#exportNasPath');
const exportWinPath = $('#exportWinPath');
const exportCopy = $('#exportCopy');
const setDraftRoot = $('#setDraftRoot');
const setNasPrefix = $('#setNasPrefix');
const setWinPrefix = $('#setWinPrefix');
const orphanModal = $('#orphanModal');
const orphanOpen = $('#orphanOpen');
const orphanClose = $('#orphanClose');
const orphanSummary = $('#orphanSummary');
const orphanAll = $('#orphanAll');
const orphanSelected = $('#orphanSelected');
const orphanList = $('#orphanList');
const orphanNote = $('#orphanNote');
const orphanRescan = $('#orphanRescan');
const orphanDelete = $('#orphanDelete');

/* 手机端：列表 / 镜头视图切换与「更多」菜单 */
const workspaceEl = $('.workspace');
const mobileBack = $('#mobileBack');
const mobileMenuBtn = $('#mobileMenuBtn');
const mobileMenu = $('#mobileMenu');
const mobileMenuClose = $('#mobileMenuClose');
const mobileThemeBtns = $$('[data-mobile-theme]');

/* ============ 主题（白天 / 夜晚 / 护眼 / 自定义） ============ */
const THEME_KEY = 'sm.theme';
const CUSTOM_KEY = 'sm.theme.custom';
const PALETTES_KEY = 'sm.theme.palettes';
const IMAGE_KEY = 'sm.theme.image';
const IMAGE_OPACITY_KEY = 'sm.theme.imageOpacity';
const THEMES = ['light', 'dark', 'sepia'];
const CUSTOM_DEFAULT = { bg: '#ffffff', accent: '#2f6ae0', text: '', panel: '', radius: 8, font: 13 };

function readTheme() {
  try {
    const saved = localStorage.getItem(THEME_KEY);
    return saved === 'custom' || THEMES.includes(saved) ? saved : 'light';
  } catch (e) {
    return 'light';
  }
}

/* ---- 自定义配色的色彩推导：只改背景与强调色，其余 token 按明暗自动配 ---- */
function hx2rgb(hex) {
  let h = String(hex || '').replace('#', '');
  if (h.length === 3) h = h.split('').map((c) => c + c).join('');
  const n = parseInt(h, 16) || 0;
  return { r: (n >> 16) & 255, g: (n >> 8) & 255, b: n & 255 };
}
function rgb2hex(c) {
  const p = (v) => Math.max(0, Math.min(255, Math.round(v))).toString(16).padStart(2, '0');
  return '#' + p(c.r) + p(c.g) + p(c.b);
}
function mixHex(a, b, t) {
  const A = hx2rgb(a);
  const B = hx2rgb(b);
  return rgb2hex({ r: A.r + (B.r - A.r) * t, g: A.g + (B.g - A.g) * t, b: A.b + (B.b - A.b) * t });
}
function rgbaHex(hex, alpha) {
  const c = hx2rgb(hex);
  return `rgba(${c.r}, ${c.g}, ${c.b}, ${alpha})`;
}
function relLum(hex) {
  const c = hx2rgb(hex);
  const f = (v) => {
    v /= 255;
    return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
  };
  return 0.2126 * f(c.r) + 0.7152 * f(c.g) + 0.0722 * f(c.b);
}

function readCustom() {
  try {
    const raw = JSON.parse(localStorage.getItem(CUSTOM_KEY) || 'null');
    if (raw && /^#[0-9a-f]{6}$/i.test(raw.bg) && /^#[0-9a-f]{6}$/i.test(raw.accent)) {
      return {
        name: typeof raw.name === 'string' ? raw.name : '',
        bg: raw.bg,
        accent: raw.accent,
        text: /^#[0-9a-f]{6}$/i.test(raw.text || '') ? raw.text : '',
        panel: /^#[0-9a-f]{6}$/i.test(raw.panel || '') ? raw.panel : '',
        radius: Number.isFinite(raw.radius) ? Math.max(0, Math.min(16, raw.radius)) : 8,
        font: Number.isFinite(raw.font) ? Math.max(11, Math.min(18, raw.font)) : 13,
      };
    }
  } catch (e) { /* 回退默认 */ }
  return Object.assign({}, CUSTOM_DEFAULT);
}

function buildCustomVars(bg, accent, opts) {
  const o = opts || {};
  const dark = relLum(bg) < 0.4;
  const ink = dark ? '#ffffff' : '#000000';
  const t = (n) => mixHex(bg, ink, n);
  const surface = (n) => mixHex(bg, dark ? '#ffffff' : '#000000', n);
  const panel = mixHex(bg, '#ffffff', dark ? 0.06 : 0.62);
  const onAccent = relLum(accent) > 0.55 ? '#10161f' : '#ffffff';
  const arrow = t(dark ? 0.56 : 0.56).replace('#', '%23');
  const vars = {
    '--bg': bg,
    '--bg-soft': surface(0.045),
    '--panel': panel,
    '--panel-2': surface(0.07),
    '--panel-3': surface(0.12),
    '--line': surface(0.17),
    '--line-soft': surface(0.09),

    '--text': t(dark ? 0.92 : 0.88),
    '--text-dim': t(dark ? 0.74 : 0.70),
    '--muted': t(dark ? 0.56 : 0.56),
    '--text-strong': dark ? '#ffffff' : t(0.92),
    '--active-text': dark ? '#ffffff' : mixHex(accent, '#000000', 0.30),

    '--accent': accent,
    '--accent-soft': rgbaHex(accent, dark ? 0.16 : 0.12),
    '--accent-soft-strong': rgbaHex(accent, dark ? 0.26 : 0.20),
    '--accent-faint': rgbaHex(accent, 0.05),
    '--accent-border': rgbaHex(accent, 0.35),
    '--accent-line': rgbaHex(accent, 0.48),
    '--accent-track': rgbaHex(accent, 0.20),
    '--accent-hover': dark ? mixHex(accent, '#ffffff', 0.14) : mixHex(accent, '#000000', 0.14),
    '--on-accent': onAccent,

    '--teal': dark ? '#35d6a4' : '#1f9f7a',
    '--teal-soft': rgbaHex(dark ? '#35d6a4' : '#1f9f7a', 0.10),
    '--teal-soft-strong': rgbaHex(dark ? '#35d6a4' : '#1f9f7a', 0.18),
    '--teal-border': rgbaHex(dark ? '#35d6a4' : '#1f9f7a', 0.35),
    '--warn': dark ? '#f5b544' : '#b7791f',
    '--warn-soft': rgbaHex(dark ? '#f5b544' : '#b7791f', 0.10),
    '--warn-border': rgbaHex(dark ? '#f5b544' : '#b7791f', 0.35),
    '--danger': dark ? '#ff6b6b' : '#d64545',
    '--danger-soft': rgbaHex(dark ? '#ff6b6b' : '#d64545', 0.10),
    '--danger-border': rgbaHex(dark ? '#ff6b6b' : '#d64545', 0.35),

    '--topbar-bg': `linear-gradient(180deg, ${panel}, ${dark ? surface(0.02) : bg})`,
    '--table-head-bg': surface(0.05),
    '--row-hover': surface(0.035),
    '--input-bg': dark ? mixHex(bg, '#000000', 0.22) : mixHex(bg, '#ffffff', 0.80),
    '--card-bg': `linear-gradient(180deg, ${surface(0.06)}, ${surface(0.02)})`,
    '--thumb-bg': `linear-gradient(135deg, ${surface(0.12)}, ${surface(0.04)})`,
    '--track-bg': dark ? mixHex(bg, '#000000', 0.25) : surface(0.08),
    '--segment-hover': surface(0.06),
    '--add-row-bg': surface(0.04),
    '--add-row-fg': t(0.36),
    '--toast-bg': rgbaHex(panel, 0.97),
    '--toast-shadow': dark ? '0 10px 30px rgba(0, 0, 0, 0.50)' : '0 10px 30px rgba(15, 30, 55, 0.16)',

    '--placeholder': t(dark ? 0.42 : 0.46),
    '--narration': dark ? mixHex(accent, '#ffffff', 0.55) : mixHex(accent, '#000000', 0.30),
    '--copy': dark ? '#cfe4d8' : '#3a6650',
    '--tip': t(dark ? 0.46 : 0.50),
    '--faint': t(dark ? 0.40 : 0.46),
    '--faint-2': t(dark ? 0.26 : 0.30),
    '--sep': surface(0.30),

    '--dashed': surface(0.18),
    '--dashed-hover': surface(0.34),
    '--icon-border': surface(0.16),
    '--icon-fg': t(dark ? 0.44 : 0.46),
    '--btn-hover-border': surface(0.32),
    '--scrollbar': surface(0.20),
    '--scrollbar-hover': surface(0.30),

    '--overlay-bg': dark ? 'rgba(4, 7, 11, 0.88)' : 'rgba(18, 26, 38, 0.72)',
    '--shadow-md': dark ? '0 20px 60px rgba(0, 0, 0, 0.55)' : '0 18px 48px rgba(15, 30, 55, 0.16)',
    '--shadow-lg': dark ? '0 24px 70px rgba(0, 0, 0, 0.60)' : '0 24px 70px rgba(15, 30, 55, 0.20)',

    '--brand-bg': `linear-gradient(135deg, ${mixHex(accent, '#ffffff', 0.18)}, ${mixHex(accent, '#000000', 0.08)})`,
    '--brand-shadow': `0 2px 10px ${rgbaHex(accent, 0.32)}`,
    '--select-arrow': `url("data:image/svg+xml;charset=utf-8,%3Csvg xmlns='http://www.w3.org/2000/svg' width='10' height='6'%3E%3Cpath d='M1 1l4 4 4-4' fill='none' stroke='${arrow}' stroke-width='1.4'/%3E%3C/svg%3E")`,
  };

  /* 面板色：只覆盖面板/卡片/表头，其余仍按背景推导，避免两套色互相打架 */
  if (o.panel) {
    const panel = o.panel;
    const pInk = dark ? '#ffffff' : '#000000';
    const pMix = (n) => mixHex(panel, pInk, n);
    vars['--panel'] = panel;
    vars['--panel-2'] = pMix(0.06);
    vars['--panel-3'] = pMix(0.12);
    vars['--table-head-bg'] = pMix(0.04);
    vars['--card-bg'] = `linear-gradient(180deg, ${pMix(0.03)}, ${pMix(0.01)})`;
    vars['--toast-bg'] = rgbaHex(panel, 0.97);
  }

  /* 文字色：派生次要文字与占位符，保证整页文字一起变 */
  if (o.text) {
    const text = o.text;
    vars['--text'] = text;
    vars['--text-dim'] = mixHex(text, bg, dark ? 0.22 : 0.26);
    vars['--muted'] = mixHex(text, bg, dark ? 0.42 : 0.46);
    vars['--text-strong'] = dark ? mixHex(text, '#ffffff', 0.30) : mixHex(text, '#000000', 0.22);
    vars['--placeholder'] = mixHex(text, bg, 0.42);
    vars['--tip'] = mixHex(text, bg, 0.50);
    vars['--faint'] = mixHex(text, bg, 0.46);
    vars['--faint-2'] = mixHex(text, bg, 0.32);
  }

  vars['--radius'] = (o.radius == null ? 8 : o.radius) + 'px';
  vars['--ui-font'] = (o.font == null ? 13 : o.font) + 'px';
  return vars;
}

let customKeys = [];

function clearCustomVars() {
  const root = document.documentElement;
  customKeys.forEach((k) => root.style.removeProperty(k));
  customKeys = [];
  root.style.colorScheme = '';
}

function applyCustomVars(colors, persist) {
  const root = document.documentElement;
  const cfg = {
    name: colors.name || '',
    bg: colors.bg,
    accent: colors.accent,
    text: colors.text || '',
    panel: colors.panel || '',
    radius: colors.radius == null ? 8 : colors.radius,
    font: colors.font == null ? 13 : colors.font,
  };
  const vars = buildCustomVars(cfg.bg, cfg.accent, cfg);
  clearCustomVars();
  customKeys = Object.keys(vars);
  customKeys.forEach((k) => root.style.setProperty(k, vars[k]));
  const base = relLum(cfg.bg) < 0.4 ? 'dark' : 'light';
  root.style.colorScheme = base;
  if (persist) {
    try {
      localStorage.setItem(CUSTOM_KEY, JSON.stringify(Object.assign({}, cfg, { base, vars })));
    } catch (e) { /* 配额不足时保持当前预览，不静默清空 */ }
  }
  syncBgLayer();
}

/* ---- 图片背景：单独存储，切到预设主题时只是隐藏，不删除 ---- */
function readImage() {
  try { return localStorage.getItem(IMAGE_KEY) || ''; } catch (e) { return ''; }
}

function readImageOpacity() {
  try {
    const n = Number(localStorage.getItem(IMAGE_OPACITY_KEY));
    return Number.isFinite(n) && n >= 4 && n <= 40 ? n : 18;
  } catch (e) { return 18; }
}

function syncBgLayer() {
  const img = readImage();
  const on = document.documentElement.dataset.theme === 'custom' && !!img;
  document.body.classList.toggle('has-bg', on);
  if (on) {
    bgLayer.style.backgroundImage = 'url("' + img + '")';
    bgLayer.style.opacity = String(readImageOpacity() / 100);
  } else {
    bgLayer.style.backgroundImage = '';
    bgLayer.style.opacity = '0';
  }
}

function normalizeHex(value, fallback) {
  const v = String(value || '').trim();
  if (/^#[0-9a-f]{6}$/i.test(v)) return v.toLowerCase();
  const m = v.match(/^rgba?\(([^)]+)\)$/i);
  if (m) {
    const parts = m[1].split(',').map((x) => parseFloat(x));
    if (parts.length >= 3 && parts.every((n) => Number.isFinite(n))) {
      return '#' + parts.slice(0, 3).map((n) => Math.max(0, Math.min(255, Math.round(n))).toString(16).padStart(2, '0')).join('');
    }
  }
  return fallback;
}

function applyThemeActive(theme) {
  themeBtns.forEach((btn) => {
    const on = btn.dataset.themeBtn === theme;
    btn.classList.toggle('active', on);
    btn.setAttribute('aria-pressed', String(on));
  });
}

function applyTheme(name, persist) {
  const theme = name === 'custom' ? 'custom' : (THEMES.includes(name) ? name : 'light');
  clearCustomVars();
  document.documentElement.dataset.theme = theme;
  if (theme === 'custom') applyCustomVars(readCustom(), true);
  applyThemeActive(theme);
  if (persist) {
    try { localStorage.setItem(THEME_KEY, theme); } catch (e) {}
  }
  syncBgLayer();
}

function openThemePanel() {
  themePanel.classList.remove('hidden');
}

function closeThemePanel() {
  themePanel.classList.add('hidden');
}

function fillThemeInputs() {
  const custom = readCustom();
  const cs = getComputedStyle(document.documentElement);
  const textFallback = normalizeHex(cs.getPropertyValue('--text'), '#1b2430');
  const panelFallback = normalizeHex(cs.getPropertyValue('--panel'), '#ffffff');
  customBg.value = custom.bg;
  customAccent.value = custom.accent;
  customText.value = custom.text || textFallback;
  customPanel.value = custom.panel || panelFallback;
  customRadius.value = String(custom.radius);
  customRadiusVal.textContent = String(custom.radius);
  customFont.value = String(custom.font);
  customFontVal.textContent = String(custom.font);
  const opacity = readImageOpacity();
  customImgOpacity.value = String(opacity);
  customImgOpacityVal.textContent = opacity + '%';
}

function customFromInputs() {
  return {
    name: readCustom().name,
    bg: normalizeHex(customBg.value, CUSTOM_DEFAULT.bg),
    accent: normalizeHex(customAccent.value, CUSTOM_DEFAULT.accent),
    text: normalizeHex(customText.value, ''),
    panel: normalizeHex(customPanel.value, ''),
    radius: Number(customRadius.value) || 0,
    font: Number(customFont.value) || 13,
  };
}

function applyCustomFromInputs() {
  const cfg = customFromInputs();
  document.documentElement.dataset.theme = 'custom';
  applyCustomVars(cfg, true);
  applyThemeActive('custom');
  try { localStorage.setItem(THEME_KEY, 'custom'); } catch (e) {}
  customRadiusVal.textContent = String(cfg.radius);
  customFontVal.textContent = String(cfg.font);
}

/* ---- 多套命名配色 ---- */
function readPalettes() {
  try {
    const list = JSON.parse(localStorage.getItem(PALETTES_KEY) || '[]');
    if (!Array.isArray(list)) return [];
    return list.filter((it) => it && typeof it.name === 'string' && it.name
      && /^#[0-9a-f]{6}$/i.test(it.bg || '') && /^#[0-9a-f]{6}$/i.test(it.accent || '')).slice(0, 8);
  } catch (e) { return []; }
}

function writePalettes(list) {
  try {
    localStorage.setItem(PALETTES_KEY, JSON.stringify(list));
    return true;
  } catch (e) {
    toast('本地存储已满，先删掉几套旧配色再保存', 4200);
    return false;
  }
}

function renderPalettes() {
  const list = readPalettes();
  customPalettes.innerHTML = '';
  if (!list.length) {
    customPalettes.appendChild(el('div', 'palette-empty', '还没有保存的配色'));
    return;
  }
  list.forEach((item) => {
    const row = el('div', 'palette-item');
    const swatch = el('span', 'palette-swatch');
    swatch.style.background = item.bg;
    swatch.style.borderColor = item.accent;
    const name = el('button', 'palette-name', item.name);
    name.type = 'button';
    name.title = '套用这套配色';
    name.addEventListener('click', () => {
      customBg.value = item.bg;
      customAccent.value = item.accent;
      customText.value = normalizeHex(item.text, relLum(item.bg) < 0.4 ? '#e8edf7' : '#1b2430');
      customPanel.value = normalizeHex(item.panel, item.bg);
      customRadius.value = String(item.radius == null ? 8 : item.radius);
      customFont.value = String(item.font == null ? 13 : item.font);
      const cfg = customFromInputs();
      cfg.name = item.name;
      document.documentElement.dataset.theme = 'custom';
      try { localStorage.setItem(THEME_KEY, 'custom'); } catch (e) {}
      applyCustomVars(cfg, true);
      applyThemeActive('custom');
      toast('已套用「' + item.name + '」');
    });
    const del = el('button', 'palette-del', '×');
    del.type = 'button';
    del.title = '删除这套配色';
    del.addEventListener('click', () => {
      const next = readPalettes().filter((it) => it.name !== item.name);
      if (writePalettes(next)) {
        renderPalettes();
        toast('已删除「' + item.name + '」');
      }
    });
    row.appendChild(swatch);
    row.appendChild(name);
    row.appendChild(del);
    customPalettes.appendChild(row);
  });
}

function savePalette() {
  const cfg = customFromInputs();
  openModal({
    title: '保存配色',
    desc: '给当前配色起个名字，之后可以在面板里一键切换。',
    placeholder: '例如：夜间低亮度',
    confirmText: '保存',
    onSubmit: (value) => {
      const name = String(value || '').trim().slice(0, 20);
      if (!name) return '名字不能为空';
      const list = readPalettes().filter((it) => it.name !== name);
      list.push({
        name,
        bg: cfg.bg,
        accent: cfg.accent,
        text: cfg.text,
        panel: cfg.panel,
        radius: cfg.radius,
        font: cfg.font,
      });
      if (list.length > 8) list.splice(0, list.length - 8);
      if (!writePalettes(list)) return false;
      renderPalettes();
      toast('已保存配色「' + name + '」');
      return true;
    },
  });
}

/* ---- 图片背景：先压缩，超配额明确提示 ---- */
function loadImageEl(file) {
  return new Promise((resolve, reject) => {
    const url = URL.createObjectURL(file);
    const img = new Image();
    img.onload = () => { URL.revokeObjectURL(url); resolve(img); };
    img.onerror = () => { URL.revokeObjectURL(url); reject(new Error('图片读取失败，换一张试试')); };
    img.src = url;
  });
}

async function compressImage(file) {
  if (!file.type || !/^image\//.test(file.type)) throw new Error('请选择图片文件');
  const img = await loadImageEl(file);
  const maxSide = 1600;
  let scale = Math.min(1, maxSide / Math.max(img.naturalWidth || 1, img.naturalHeight || 1));
  let out = '';
  for (let i = 0; i < 4; i++) {
    const w = Math.max(1, Math.round((img.naturalWidth || 1) * scale));
    const h = Math.max(1, Math.round((img.naturalHeight || 1) * scale));
    const canvas = document.createElement('canvas');
    canvas.width = w;
    canvas.height = h;
    const ctx = canvas.getContext('2d');
    if (!ctx) throw new Error('当前浏览器不支持图片压缩');
    ctx.drawImage(img, 0, 0, w, h);
    out = canvas.toDataURL('image/jpeg', 0.78 - i * 0.12);
    if (out.length <= 900 * 1024) break;
    scale *= 0.75;
  }
  return out;
}

function saveImageData(dataUrl) {
  const old = readImage();
  try {
    localStorage.setItem(IMAGE_KEY, dataUrl);
  } catch (e) {
    try {
      if (old) localStorage.setItem(IMAGE_KEY, old);
      else localStorage.removeItem(IMAGE_KEY);
    } catch (_) { /* 忽略回滚失败 */ }
    const mb = (dataUrl.length / 1024 / 1024).toFixed(1);
    throw new Error('图片太大，浏览器本地存储放不下（压缩后仍有约 ' + mb + 'MB），请换一张更小的图片。');
  }
}

function bindTheme() {
  applyTheme(readTheme(), false);
  fillThemeInputs();
  renderPalettes();

  themeBtns.forEach((btn) => {
    btn.addEventListener('click', () => {
      const name = btn.dataset.themeBtn;
      if (name === 'custom') {
        applyTheme('custom', true);
        fillThemeInputs();
        renderPalettes();
        openThemePanel();
        return;
      }
      closeThemePanel();
      applyTheme(name, true);
    });
  });

  [customBg, customText, customPanel, customAccent, customRadius, customFont].forEach((input) => {
    input.addEventListener('input', applyCustomFromInputs);
  });

  customReset.addEventListener('click', () => {
    customBg.value = CUSTOM_DEFAULT.bg;
    customAccent.value = CUSTOM_DEFAULT.accent;
    customText.value = '#1b2430';
    customPanel.value = '#ffffff';
    customRadius.value = '8';
    customFont.value = '13';
    applyCustomFromInputs();
    toast('已恢复默认配色');
  });

  customSavePalette.addEventListener('click', savePalette);

  customImgPick.addEventListener('click', () => customImgFile.click());

  customImgClear.addEventListener('click', () => {
    try { localStorage.removeItem(IMAGE_KEY); } catch (e) {}
    syncBgLayer();
    toast('已移除图片背景');
  });

  customImgFile.addEventListener('change', async () => {
    const file = customImgFile.files && customImgFile.files[0];
    customImgFile.value = '';
    if (!file) return;
    try {
      toast('正在压缩图片…');
      const dataUrl = await compressImage(file);
      saveImageData(dataUrl);
      if (document.documentElement.dataset.theme !== 'custom') {
        document.documentElement.dataset.theme = 'custom';
        applyCustomFromInputs();
      }
      syncBgLayer();
      toast('图片背景已启用');
    } catch (err) {
      toast(err.message || '图片处理失败', 4200);
    }
  });

  customImgOpacity.addEventListener('input', () => {
    const v = Number(customImgOpacity.value) || 18;
    customImgOpacityVal.textContent = v + '%';
    try { localStorage.setItem(IMAGE_OPACITY_KEY, String(v)); } catch (e) {}
    syncBgLayer();
  });

  document.addEventListener('mousedown', (e) => {
    if (!e.target.closest || !e.target.closest('.theme-wrap')) closeThemePanel();
  });
}

/* ============ 布局：侧栏折叠 / 剪辑面板常驻折叠 ============ */
const SIDEBAR_KEY = 'sm.sidebar';
// 新键名：抽屉时代的 sm.drawer 语义不同，避免旧值让面板一进来就是收起的
const PANEL_KEY = 'sm.clipPanel';

function setSidebar(collapsed, persist) {
  sidebarEl.classList.toggle('collapsed', collapsed);
  sidebarToggle.textContent = collapsed ? '»' : '«';
  sidebarToggle.title = collapsed ? '展开列表' : '收起列表';
  sidebarToggle.setAttribute('aria-label', sidebarToggle.title);
  if (persist) {
    try { localStorage.setItem(SIDEBAR_KEY, collapsed ? '1' : '0'); } catch (e) {}
  }
}

// 剪辑面板常驻在右侧，collapsed 只是让出宽度；默认展开，只有明确存过 0 才收起
function setClipPanel(open, persist) {
  clipPanel.classList.toggle('collapsed', !open);
  panelToggle.classList.toggle('active', open);
  panelToggle.setAttribute('aria-expanded', String(open));
  if (persist) {
    try { localStorage.setItem(PANEL_KEY, open ? '1' : '0'); } catch (e) {}
  }
}

/* ============ 手机端：列表 ↔ 镜头视图、更多菜单 ============ */
const MOBILE_MAX = 768;

function isMobileView() {
  return window.matchMedia(`(max-width: ${MOBILE_MAX}px)`).matches;
}

// 手机端分两屏：脚本列表 → 镜头视图。桌面端始终显示「列表 + 表格」两栏，detail 类不生效
function setMobileDetail(on) {
  if (!isMobileView()) {
    workspaceEl.classList.remove('detail');
    return;
  }
  workspaceEl.classList.toggle('detail', !!on);
  if (on) {
    openMobileMenu(false);
    const scroll = $('.table-scroll');
    if (scroll) scroll.scrollTop = 0;
  }
}

function openMobileMenu(open) {
  if (!mobileMenu) return;
  mobileMenu.classList.toggle('hidden', !open);
  if (mobileMenuBtn) mobileMenuBtn.setAttribute('aria-expanded', String(!!open));
}

function bindMobile() {
  if (mobileBack) {
    mobileBack.addEventListener('click', () => {
      setMobileDetail(false);
      setClipPanel(false, false);
    });
  }
  if (mobileMenuBtn) {
    mobileMenuBtn.addEventListener('click', () => {
      openMobileMenu(mobileMenu.classList.contains('hidden'));
    });
  }
  if (mobileMenuClose) mobileMenuClose.addEventListener('click', () => openMobileMenu(false));
  mobileMenu.addEventListener('click', (e) => {
    if (e.target === mobileMenu) openMobileMenu(false);
  });

  mobileThemeBtns.forEach((btn) => {
    if (btn.dataset.mobileTheme === readTheme()) btn.classList.add('active');
    btn.addEventListener('click', () => {
      const name = btn.dataset.mobileTheme;
      applyTheme(name, true);
      if (name === 'custom') fillThemeInputs();
      mobileThemeBtns.forEach((b) => b.classList.toggle('active', b === btn));
      openMobileMenu(false);
    });
  });

  const mExport = $('#mExport');
  if (mExport) {
    mExport.addEventListener('click', () => {
      openMobileMenu(false);
      openExport();
    });
  }
  $('#mPanel').addEventListener('click', () => {
    openMobileMenu(false);
    setClipPanel(true, false);
  });
  const mDelProduct = $('#mDelProduct');
  if (mDelProduct) {
    mDelProduct.addEventListener('click', () => {
      openMobileMenu(false);
      askDeleteProduct();
    });
  }
  $('#mHistory').addEventListener('click', () => {
    openMobileMenu(false);
    historyBtn.click();
  });
  $('#mRenumber').addEventListener('click', () => {
    openMobileMenu(false);
    renumberBtn.click();
  });
  $('#mFfmpeg').addEventListener('click', () => {
    openMobileMenu(false);
    ffStatus.click();
  });

  const mq = window.matchMedia(`(max-width: ${MOBILE_MAX}px)`);
  const onMq = () => {
    if (!mq.matches) {
      workspaceEl.classList.remove('detail');
      openMobileMenu(false);
      return;
    }
    // 切到手机尺寸时回到列表，并收起面板（面板在手机上会全屏覆盖）
    workspaceEl.classList.remove('detail');
    setSidebar(false, false);
    setClipPanel(false, false);
    openMobileMenu(false);
  };
  if (mq.addEventListener) mq.addEventListener('change', onMq);
  else if (mq.addListener) mq.addListener(onMq);
}

function bindLayout() {
  let collapsed = false;
  let panelOpen = true;
  try {
    collapsed = localStorage.getItem(SIDEBAR_KEY) === '1';
    panelOpen = localStorage.getItem(PANEL_KEY) !== '0';
  } catch (e) { /* 忽略 */ }
  // 手机端没有侧栏折叠与常驻面板：一律从「列表 + 收起面板」开始
  setSidebar(collapsed && !isMobileView(), false);
  setClipPanel(panelOpen && !isMobileView(), false);

  sidebarToggle.addEventListener('click', () => {
    setSidebar(!sidebarEl.classList.contains('collapsed'), true);
  });

  scriptSearch.addEventListener('input', () => {
    listUi.q = scriptSearch.value;
    listUi.closed = false;
    listUi.active = 0;
    renderScriptList();
    scheduleContentSearch();
  });
  scriptSearch.addEventListener('keydown', (e) => {
    /* 只有焦点在搜索框时才接管方向键，避免和剪辑面板 / 表格的快捷键冲突 */
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      const items = $$('.search-hit', searchResults);
      if (!items.length) return;
      e.preventDefault();
      e.stopPropagation();
      listUi.closed = false;
      listUi.active += e.key === 'ArrowDown' ? 1 : -1;
      if (listUi.active < 0) listUi.active = 0;
      if (listUi.active > items.length - 1) listUi.active = items.length - 1;
      applySearchActive();
      return;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      const items = $$('.search-hit', searchResults);
      if (items.length) {
        const target = items[Math.max(0, Math.min(listUi.active, items.length - 1))];
        target.click();
        return;
      }
      const first = scriptList.querySelector('.script-item');
      if (first) first.click();
      return;
    }
    if (e.key === 'Escape') {
      e.stopPropagation();
      if (!searchResults.classList.contains('hidden')) {
        listUi.closed = true;
        renderSearchResults();
        return;
      }
      listUi.searchSeq += 1;
      scriptSearch.value = '';
      listUi.q = '';
      listUi.hits = [];
      listUi.closed = false;
      renderScriptList();
      renderSearchResults();
      scriptSearch.blur();
    }
  });
  scriptFilter.addEventListener('change', () => {
    listUi.scope = scriptFilter.value;
    renderScriptList();
    scheduleContentSearch();
  });
  usageTabs.forEach((btn) => {
    btn.addEventListener('click', () => {
      listUi.usage = btn.dataset.usage || 'all';
      usageTabs.forEach((b) => b.classList.toggle('active', b === btn));
      renderScriptList();
    });
  });
  uploadToggle.addEventListener('click', () => setUploadsPaused(!uploads.paused));

  panelToggle.addEventListener('click', () => {
    setClipPanel(clipPanel.classList.contains('collapsed'), true);
  });
  panelClose.addEventListener('click', () => setClipPanel(false, true));
}

/* ============ 启动 ============ */
async function init() {
  bindTheme();
  bindLayout();
  bindMobile();
  bindColumnResize();
  bindHistory();
  bindExport();
  bindOrphans();
  bindGlobal();
  bindPanel();
  bindSettings();
  bindCollab();
  try {
    await refreshAll();
  } catch (err) {
    setSaveStatus('服务连接失败', 'err');
    toast('连接服务失败：' + err.message);
  }
  loadEnv().catch(() => {
    ffStatus.className = 'ff-chip err';
    ffStatus.textContent = 'ffmpeg 状态未知';
  });
}

/* ============ 数据加载 ============ */
let treeFetchedAt = 0;

async function loadTree() {
  const data = await api('/api/tree');
  state.products = data.products || [];
  state.used = new Set(Array.isArray(data.used) ? data.used : []);
  treeFetchedAt = Date.now();
}

/* 只刷新左侧列表（不动当前文档），用于同事改了使用状态 / 增删脚本 */
async function refreshListOnly() {
  try {
    await loadTree();
    const p = state.products.find((x) => x.name === state.product);
    if (p) state.scripts = (p.scripts || []).slice();
    renderProductSelect();
    renderScriptList();
  } catch (e) { /* 短暂断网时保持现状 */ }
}

let pendingMove = '';
let pendingDelete = '';
let pendingProduct = '';
let pendingClipReplace = null;

async function handleScriptMoved(msg) {
  if (pendingMove === msg.from) return;
  if (msg.product === state.product && state.script === msg.to) return;
  await refreshListOnly();
  if (msg.product === state.product && msg.from === state.script) {
    toast('「' + msg.from + '」已被重命名为「' + msg.to + '」', 3600);
    stopSession();
    state.script = msg.to;
    await refreshAll(msg.product, msg.to);
  } else {
    toast('有脚本被重命名');
  }
}

async function handleScriptGone(msg) {
  if (pendingDelete === msg.script) return;
  if (pendingProduct && pendingProduct === msg.product) return;
  await refreshListOnly();
  // 整个产品被删时会连续收到多条 gone：产品已经不在列表里就安静跳过，避免刷屏
  const productGone = !state.products.some((p) => p.name === msg.product);
  if (productGone && state.product !== msg.product) return;
  if (msg.product === state.product && msg.script === state.script) {
    toast('「' + msg.script + '」已被删除', 3600);
    stopSession();
    state.script = '';
    state.doc = null;
    await refreshAll(msg.product);
  } else {
    toast('有脚本被删除');
  }
}

async function refreshAll(preferProduct, preferScript) {
  await flushNow();
  await loadTree();
  const product = state.products.find((p) => p.name === (preferProduct || state.product)) || state.products[0];
  if (!product) {
    state.product = '';
    state.scripts = [];
    state.script = '';
    state.doc = null;
    resetClipPanel();
    renderAll();
    return;
  }
  state.product = product.name;
  state.scripts = (product.scripts || []).slice();
  renderProductSelect();
  const script = preferScript && state.scripts.includes(preferScript)
    ? preferScript
    : (state.scripts.includes(state.script) ? state.script : state.scripts[0] || '');
  renderScriptList();
  if (script) {
    state.script = script;
    await loadDocFor(product.name, script);
  } else {
    state.script = '';
    state.doc = null;
    resetClipPanel();
    renderAll();
  }
}

async function selectProduct(name) {
  if (!name || name === state.product) return;
  const product = state.products.find((p) => p.name === name);
  if (!product) return;
  await flushNow();
  /* 顶部切换产品时，把「指定产品」筛选收回，列表跟随当前产品 */
  if (listUi.scope && listUi.scope !== '__all') listUi.scope = '';
  state.product = name;
  state.scripts = (product.scripts || []).slice();
  state.script = state.scripts[0] || '';
  state.doc = null;
  renderAll();
  if (state.script) await loadDocFor(name, state.script);
  else resetClipPanel();
}

async function selectScript(name) {
  if (!state.product || !name) return;
  if (name === state.script && state.doc) return;
  await flushNow();
  state.script = name;
  renderScriptList();
  await loadDocFor(state.product, name);
}

/* 从「全部产品 / 指定产品」筛选结果里选中脚本，需要时先切产品 */
async function selectScriptOf(product, name) {
  if (!product || !name) return;
  if (product === state.product) {
    await selectScript(name);
    return;
  }
  const target = state.products.find((p) => p.name === product);
  if (!target) return;
  await flushNow();
  state.product = product;
  state.scripts = (target.scripts || []).slice();
  state.script = name;
  state.doc = null;
  renderProductSelect();
  renderAll();
  await loadDocFor(product, name);
}

async function loadDocFor(product, script) {
  const seq = ++loadSeq;
  pendingClipReplace = null;
  stopSession();
  setSaveStatus('加载中…', '');
  try {
    const snap = await api(`/api/collab?product=${encodeURIComponent(product)}&script=${encodeURIComponent(script)}` +
      `&site=${encodeURIComponent(me.site)}&name=${encodeURIComponent(me.name)}`);
    if (seq !== loadSeq) return;
    state.epoch = snap.epoch || '';
    state.regs = Collab.createState(state.epoch);
    Collab.observeNow(state.regs, snap.now);
    // 断线期间存在本机的改动先并进来（同一世代才可信），再合服务器快照
    const cached = Collab.loadCache(product, script);
    if (cached && cached.epoch === state.epoch) Collab.mergeRegs(state.regs, cached.regs);
    Collab.mergeRegs(state.regs, snap.regs || {});
    state.doc = normalizeDoc(snap.doc || Collab.project(state.regs, product, script), product, script);
    rev = 0;
    state.dirty = false;
    renderAll();
    setSaveStatus('已连接', 'ok');
    restoreEditor(state.doc.edit, seq);
    startSession(seq, product, script);
  } catch (err) {
    if (seq !== loadSeq) return;
    state.doc = null;
    state.regs = null;
    resetClipPanel();
    renderAll();
    setSaveStatus('加载失败', 'err');
    toast(err.message);
  }
}

function normalizeDoc(doc, product, script) {
  const out = doc && typeof doc === 'object' ? doc : {};
  out.product = product;
  out.script = script;
  out.shots = Array.isArray(out.shots) ? out.shots : [];
  if (!out.shots.length) out.shots.push(makeShot('1'));
  out.shots.forEach((shot, i) => {
    if (!shot.id) shot.id = uid();
    if (shot.no == null) shot.no = String(i + 1);
    shot.no = String(shot.no);
    shot.content = shot.content || '';
    shot.narration = shot.narration || '';
    shot.copy = shot.copy || '';
    if (!shot.video || !shot.video.file) {
      shot.video = null;
    } else {
      shot.video.src = shot.video.src === 'temp' ? 'temp' : 'script';
      shot.video.in = Number(shot.video.in) || 0;
      shot.video.out = Number(shot.video.out) || 0;
    }
  });
  if (out.edit && typeof out.edit === 'object' && out.edit.source) {
    out.edit.source = String(out.edit.source);
    out.edit.sourceName = String(out.edit.sourceName || '');
    out.edit.cuts = Array.isArray(out.edit.cuts)
      ? out.edit.cuts.map(Number).filter((n) => Number.isFinite(n) && n > 0).sort((a, b) => a - b)
      : [];
    out.edit.currentTime = Number(out.edit.currentTime) || 0;
    out.edit.fps = Number(out.edit.fps) || 0;
  } else {
    out.edit = null;
  }
  return out;
}

function makeShot(no) {
  return { id: uid(), no: String(no), content: '', narration: '', copy: '', video: null };
}

/* ============ 协作保存 ============ */
// 所有改动都以「单个字段寄存器」的形式提交，服务端按 (时间, 编辑者) 合并，
// 因此不同镜头、同一行的不同列不会互相覆盖。
function pushRegs(regs) {
  if (!regs || !Object.keys(regs).length) return;
  state.dirty = true;
  if (!state.sess) {
    setSaveStatus('未连接，改动已暂存本机', 'warn');
    return;
  }
  state.sess.push(regs);
  setSaveStatus('同步中…', '');
}

function editShot(shot, field, value) {
  if (!shot || !state.doc) return;
  shot[field] = value;
  if (!state.regs) return;
  const regs = {};
  // 空脚本首行只是界面上临时补的占位行，还没有排序寄存器；第一次编辑时补上，
  // 否则投影会因为没有 order 而丢掉这一行。
  const orderKey = Collab.shotKey(shot.id, 'order');
  if (!state.regs.regs[orderKey]) {
    const reg = Collab.setReg(state.regs, orderKey, 0, me.site);
    reg.v = reg.t; // 与 createShot 一致：用混合逻辑时钟作为行序
    regs[orderKey] = reg;
  }
  const noKey = Collab.shotKey(shot.id, 'no');
  if (!state.regs.regs[noKey]) {
    regs[noKey] = Collab.setReg(state.regs, noKey, String(shot.no == null ? '' : shot.no), me.site);
  }
  const key = Collab.shotKey(shot.id, field);
  regs[key] = Collab.setReg(state.regs, key, value, me.site);
  pushRegs(regs);
}

/* 剪辑面板状态随脚本同步：原片、帧率、时长、打点、时间轴位置 */
function syncEditState() {
  if (!state.regs) return null;
  const round = (v) => Math.round((Number(v) || 0) * 1000) / 1000;
  let values;
  if (!state.clip) {
    values = { source: null, sourceName: '', fps: 0, duration: 0, cuts: [], currentTime: 0 };
  } else {
    values = {
      source: state.clip.file,
      sourceName: state.clip.name || '',
      fps: state.fps > 1 ? round(state.fps) : 0,
      duration: round(state.clipDuration),
      cuts: state.cuts.map(round),
      currentTime: round(clipVideo.currentTime),
    };
  }
  const regs = {};
  Object.keys(values).forEach((field) => {
    const key = Collab.editKey(field);
    regs[key] = Collab.setReg(state.regs, key, values[field], me.site);
  });
  if (state.doc) state.doc.edit = Collab.project(state.regs, state.product, state.script).edit;
  return regs;
}

function saveEdit() {
  if (!state.doc) return;
  pushRegs(syncEditState());
}

async function flushNow() {
  if (state.sess) state.sess.flushNow();
}

function setSaveStatus(text, cls) {
  saveStatus.textContent = text;
  saveStatus.className = 'save-status' + (cls ? ' ' + cls : '');
}

/* ============ 协作会话 ============ */

function startSession(seq, product, script) {
  state.sess = Collab.connect({
    product,
    script,
    epoch: state.epoch,
    site: me.site,
    name: me.name,
    color: me.color,
    getState: () => state.regs,
    onStatus: (s) => setSyncStatus(s),
    onSnapshot: (snap) => {
      if (seq !== loadSeq || !state.regs) return;
      if (snap.epoch && state.regs.epoch && snap.epoch !== state.regs.epoch) {
        // 文档世代变了（服务端被重建 / 旧接口整体导入），按服务器状态重新开始
        state.regs = Collab.createState(snap.epoch);
      }
      if (snap.epoch) {
        state.regs.epoch = snap.epoch;
        state.epoch = snap.epoch;
      }
      Collab.observeNow(state.regs, snap.now);
      const changed = Collab.mergeRegs(state.regs, snap.regs || {});
      if (Object.keys(changed).length) {
        patchDoc(normalizeDoc(Collab.project(state.regs, product, script), product, script), changed);
      }
    },
    onMerge: (regs) => {
      if (!state.regs) return;
      const changed = Collab.mergeRegs(state.regs, regs);
      if (!Object.keys(changed).length) return;
      patchRemote(changed);
    },
    onPeers: (peers) => renderPeers(peers),
    onTree: () => refreshListOnly(),
    onMoved: (msg) => handleScriptMoved(msg),
    onGone: (msg) => handleScriptGone(msg),
    onAck: () => {
      state.dirty = false;
      setSaveStatus('已保存 ' + clock(), 'ok');
    },
  });
  setSyncStatus(state.sess.status());
}

function stopSession() {
  if (state.sess) {
    state.sess.stop();
    state.sess = null;
  }
  state.peers = [];
  state.myKey = '';
  renderPeers([]);
}

function setSyncStatus(s) {
  const map = {
    online: ['协作已连接', 'ok'],
    connecting: ['连接中…', ''],
    reconnecting: ['重连中…', 'warn'],
    polling: ['轮询同步', 'warn'],
    offline: ['离线', 'err'],
  };
  const info = map[s] || ['协作未知', ''];
  syncStateEl.textContent = info[0];
  syncStateEl.className = 'sync-chip' + (info[1] ? ' ' + info[1] : '');
}

function renderPeers(peers) {
  state.peers = (peers || []).filter((p) => p && p.site && p.site !== me.site);
  paintPresence();
  if (!peersEl) return;
  const all = [{ site: me.site, name: me.name, color: me.color, isMe: true }].concat(state.peers);
  peersEl.innerHTML = '';
  all.slice(0, 6).forEach((p) => {
    const dot = el('span', 'peer-dot' + (p.isMe ? ' me' : ''), (p.name || '同').slice(0, 1));
    dot.style.background = p.color || '#8b96a8';
    dot.title = (p.name || '同事') + (p.isMe ? '（我）' : '');
    peersEl.appendChild(dot);
  });
  if (all.length > 6) peersEl.appendChild(el('span', 'peer-more', '+' + (all.length - 6)));
  peersEl.title = all.map((p) => (p.name || '同事') + (p.isMe ? '（我）' : '')).join('、') + '\n点击可以修改我的名字';
}

// 在别人正在编辑的单元格上画彩色边框和名字标签
function paintPresence() {
  if (!rowsEl) return;
  const cells = $$('.cell[data-key]', rowsEl);
  cells.forEach((cell) => {
    cell.classList.remove('peer-cell');
    const tag = cell.querySelector('.peer-tag');
    if (tag) tag.remove();
  });
  state.peers.forEach((p) => {
    if (!p.cell) return;
    const cell = cells.find((c) => c.dataset.key === p.cell);
    if (!cell) return;
    cell.classList.add('peer-cell');
    cell.style.setProperty('--peer-color', p.color || '#8b96a8');
    const tag = el('span', 'peer-tag', p.name || '同事');
    tag.style.background = p.color || '#8b96a8';
    cell.appendChild(tag);
  });
}

function bindCollab() {
  if (rowsEl) {
    rowsEl.addEventListener('focusin', (e) => {
      const cell = e.target.closest ? e.target.closest('.cell[data-key]') : null;
      if (!cell || !state.sess) return;
      state.myKey = cell.dataset.key;
      state.sess.presence(state.myKey);
    });
    rowsEl.addEventListener('focusout', () => {
      if (!state.sess) return;
      setTimeout(() => {
        const active = document.activeElement;
        const cell = active && active.closest ? active.closest('.cell[data-key]') : null;
        if (cell && cell.dataset.key === state.myKey) return;
        state.myKey = '';
        state.sess.presence('');
      }, 200);
    });
  }
  if (peersEl) {
    peersEl.addEventListener('click', () => {
      openModal({
        title: '我的名字',
        desc: '同事会看到这个名字，用来区分谁在编辑哪一格。只保存在这台电脑上。',
        placeholder: me.name,
        onSubmit: async (name) => {
          if (name.length > 16) throw new Error('名字最多 16 个字');
          me.name = name;
          Collab.setName(name);
          if (state.sess) state.sess.rename(name);
          renderPeers(state.peers);
        },
      });
      modalInput.value = me.name;
    });
  }
  window.addEventListener('beforeunload', () => {
    if (state.sess) state.sess.flushNow();
  });
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'hidden' && state.sess) state.sess.flushNow();
  });
  /* 轮询模式下没有 tree 广播，回到页面时补一次列表刷新 */
  window.addEventListener('focus', () => {
    if (pendingMove || pendingDelete || pendingProduct) return;
    if (Date.now() - treeFetchedAt > 4000) refreshListOnly();
  });
}

// 把远端合并后的文档差异刷到界面上：结构变了重建表格，只改单元格就原地更新
function patchDoc(next, changed) {
  const prev = state.doc;
  state.doc = next;
  if (!prev) {
    renderAll();
    return;
  }
  const prevIds = prev.shots.map((s) => s.id).join(',');
  const nextIds = next.shots.map((s) => s.id).join(',');
  const structural = prevIds !== nextIds;
  const hasEdit = changed && Object.keys(changed).some((k) => k.indexOf('e|') === 0);
  if (structural || hasEdit) renderEditor();
  if (structural) {
    rerenderTable();
  } else {
    updateCells(prev, next);
  }
  applyRemoteEdit(prev, next, changed);
  paintPresence();
}

/* 增量远端合并：只是已有行的普通字段变化时，定点更新那几行，
   不做整篇投影、不重建表格；出现新行 / 删除 / 排序等结构变化才回退全量路径。 */
function patchRemote(changed) {
  if (!state.doc || !state.regs) return;
  const keys = Object.keys(changed || {});
  if (!keys.length) return;
  const FIELD_OK = { no: 1, content: 1, narration: 1, copy: 1, video: 1 };
  const byId = new Map(state.doc.shots.map((s) => [s.id, s]));
  const touched = new Map();
  let structural = false;
  let editDirty = false;
  for (const key of keys) {
    if (key.indexOf('e|') === 0) {
      editDirty = true;
      continue;
    }
    const p = key.split('|');
    if (p.length !== 3 || p[0] !== 's' || !FIELD_OK[p[2]] || !byId.has(p[1])) {
      structural = true;
      break;
    }
    if (!touched.has(p[1])) touched.set(p[1], {});
    touched.get(p[1])[p[2]] = true;
  }
  const project = () => normalizeDoc(Collab.project(state.regs, state.product, state.script), state.product, state.script);
  if (structural) {
    patchDoc(project(), changed);
    return;
  }
  touched.forEach((fields, id) => {
    const shot = byId.get(id);
    Object.keys(fields).forEach((f) => {
      const reg = changed[Collab.shotKey(id, f)];
      const v = reg ? reg.v : undefined;
      if (f === 'video') {
        shot.video = v && v.file
          ? { src: v.src === 'temp' ? 'temp' : 'script', file: String(v.file), label: v.label || '', in: Number(v.in) || 0, out: Number(v.out) || 0 }
          : null;
      } else if (f === 'no') {
        shot.no = String(v == null ? '' : v);
      } else {
        shot[f] = v == null ? '' : String(v);
      }
    });
    syncShotRow(shot, fields);
  });
  if (editDirty) {
    const prev = state.doc;
    const next = project();
    state.doc = next;
    renderEditor();
    applyRemoteEdit(prev, next, changed);
  }
}

/* 定点刷新受影响的一行：输入框只在未聚焦且值不同才写入，视频卡片按需重画 */
function syncShotRow(shot, fields) {
  const row = rowsEl.querySelector(`.row[data-id="${shot.id}"]`);
  if (!row) return;
  ['no', 'content', 'narration', 'copy'].forEach((field) => {
    if (!fields[field]) return;
    const input = row.querySelector(`[data-field="${field}"]`);
    if (!input || input === document.activeElement) return;
    if (input.value !== shot[field]) {
      input.value = shot[field];
      if (input.tagName === 'TEXTAREA') autoGrow(input);
    }
  });
  if (fields.video) {
    const cell = row.querySelector('.video-cell');
    if (cell && cell._drawVideo) cell._drawVideo();
  }
}

function updateCells(prev, next) {
  const prevById = {};
  prev.shots.forEach((s) => { prevById[s.id] = s; });
  next.shots.forEach((shot) => {
    const row = rowsEl.querySelector(`.row[data-id="${shot.id}"]`);
    if (!row) return;
    ['no', 'content', 'narration', 'copy'].forEach((field) => {
      const input = row.querySelector(`[data-field="${field}"]`);
      if (!input || input === document.activeElement) return;
      if (input.value !== shot[field]) {
        input.value = shot[field];
        if (input.tagName === 'TEXTAREA') autoGrow(input);
      }
    });
    const cell = row.querySelector('.video-cell');
    const before = prevById[shot.id];
    if (cell && cell._drawVideo && JSON.stringify(before && before.video || null) !== JSON.stringify(shot.video || null)) {
      cell._drawVideo();
    }
  });
}

function rerenderTable() {
  const active = document.activeElement;
  let key = '';
  let start = 0;
  let end = 0;
  if (active && active.closest) {
    const cell = active.closest('.cell[data-key]');
    if (cell) {
      key = cell.dataset.key;
      try {
        start = active.selectionStart;
        end = active.selectionEnd;
      } catch (_) {}
    }
  }
  renderTable();
  if (!key) return;
  const cells = $$('.cell[data-key]', rowsEl);
  const target = cells.find((c) => c.dataset.key === key);
  if (!target) return;
  const input = target.querySelector('input, textarea');
  if (!input) return;
  input.focus();
  try { input.setSelectionRange(start, end); } catch (_) {}
}

// 别人改了剪辑面板（原片 / 打点 / 帧率）时跟着更新，不动本地播放位置
function applyRemoteEdit(prev, next, changed) {
  if (changed) {
    const hasEdit = Object.keys(changed).some((k) => k.indexOf('e|') === 0);
    if (!hasEdit) return;
  }
  const before = prev.edit || null;
  const after = next.edit || null;
  if (before && after && before.source === after.source) {
    if (!state.clip) return;
    if (JSON.stringify(before.cuts) !== JSON.stringify(after.cuts)) {
      state.cuts = (after.cuts || []).slice().sort((a, b) => a - b);
      renderTimelineCuts();
      renderSegments();
    }
    if (after.fps > 1 && Math.abs(after.fps - state.fps) > 0.01) {
      state.fps = after.fps;
      updateFrameLabels();
      clipFps.textContent = after.fps.toFixed(2) + ' fps';
    }
    return;
  }
  if (!after) {
    if (state.clip) resetClipPanel();
    return;
  }
  if (!state.clip || state.clip.file !== after.source) {
    restoreEditor(after, loadSeq);
  }
}

/* ============ 渲染 ============ */
function renderAll() {
  renderProductSelect();
  renderScriptList();
  renderEditor();
  renderTable();
}

function renderProductSelect() {
  productSelect.innerHTML = '';
  renderScriptFilter();
  if (!state.products.length) {
    const opt = el('option', null, '尚无产品');
    opt.value = '';
    opt.disabled = true;
    opt.selected = true;
    productSelect.appendChild(opt);
    productSelect.disabled = true;
    return;
  }
  productSelect.disabled = false;
  state.products.forEach((p) => {
    const opt = el('option', null, p.name);
    opt.value = p.name;
    if (p.name === state.product) opt.selected = true;
    productSelect.appendChild(opt);
  });
}

/* 左侧筛选只使用 /api/tree 里真实存在的字段：产品名、脚本名、使用状态 */
const listUi = {
  q: '',
  scope: '',
  usage: 'all',
  hits: [],
  hitsTruncated: false,
  searching: false,
  searchSeq: 0,
  collapsed: new Set(), // 已折叠的脚本分组
  active: 0,            // ↑↓ 当前选中的命中项
  closed: false,        // Esc 收起结果面板
};
let searchTimer = 0;

function scriptKey(product, name) {
  return product + '/' + name;
}

function isUsed(product, name) {
  return state.used.has(scriptKey(product, name));
}

function renderScriptFilter() {
  scriptFilter.innerHTML = '';
  const cur = el('option', null, '当前产品');
  cur.value = '';
  scriptFilter.appendChild(cur);
  const all = el('option', null, '全部产品');
  all.value = '__all';
  scriptFilter.appendChild(all);
  state.products.forEach((p) => {
    const opt = el('option', null, p.name);
    opt.value = p.name;
    scriptFilter.appendChild(opt);
  });
  scriptFilter.value = listUi.scope;
  /* 产品被删除等情况下，筛选值会回退到「当前产品」 */
  listUi.scope = scriptFilter.value;
}

function visibleScripts() {
  const q = listUi.q.trim().toLowerCase();
  const entries = [];
  if (listUi.scope === '__all') {
    state.products.forEach((p) => {
      (p.scripts || []).forEach((name) => entries.push({ product: p.name, name }));
    });
  } else {
    const productName = listUi.scope || state.product;
    const p = state.products.find((x) => x.name === productName);
    const names = p ? (p.scripts || []) : (productName === state.product ? state.scripts : []);
    names.forEach((name) => entries.push({ product: productName, name }));
  }
  let list = entries;
  if (listUi.usage === 'used') list = list.filter((e) => isUsed(e.product, e.name));
  else if (listUi.usage === 'unused') list = list.filter((e) => !isUsed(e.product, e.name));
  if (!q) return list;
  return list.filter((e) => (e.name + ' ' + e.product).toLowerCase().includes(q));
}

function renderScriptList() {
  scriptList.innerHTML = '';
  if (!state.products.length) {
    scriptList.appendChild(el('div', 'sidebar-empty', '还没有产品。\n先在左上角新建产品。'));
    return;
  }
  if (!listUi.scope && !listUi.q && !state.scripts.length) {
    scriptList.appendChild(el('div', 'sidebar-empty', '这个产品还没有脚本。\n点击上方「+ 新建」创建。'));
    return;
  }
  const entries = visibleScripts();
  if (!entries.length) {
    let text = '这个产品还没有脚本。\n点击上方「+ 新建」创建。';
    if (listUi.q) text = '没有匹配的脚本。\n换个关键词，或清空搜索。';
    else if (listUi.usage === 'used') text = '还没有标记为「使用」的脚本。';
    else if (listUi.usage === 'unused') text = '所有脚本都已标记为「使用」。';
    scriptList.appendChild(el('div', 'sidebar-empty', text));
    return;
  }
  entries.forEach((entry) => {
    const active = entry.product === state.product && entry.name === state.script;
    const used = isUsed(entry.product, entry.name);
    const row = el('div', 'script-row' + (active ? ' active' : '') + (used ? ' used' : ''));

    const btn = el('button', 'script-item' + (active ? ' active' : ''));
    btn.type = 'button';
    btn.title = entry.product + ' / ' + entry.name + '\n双击名称可重命名';
    if (used) btn.appendChild(el('span', 'used-dot', '✓'));
    const nameEl = el('span', 'name', entry.name);
    nameEl.title = '双击重命名';
    nameEl.addEventListener('dblclick', (e) => {
      e.stopPropagation();
      startRename(row, entry, nameEl);
    });
    btn.appendChild(nameEl);
    if (entry.product !== state.product) {
      btn.appendChild(el('span', 'item-tag', entry.product));
    }
    btn.addEventListener('click', () => {
      setMobileDetail(true);
      selectScriptOf(entry.product, entry.name);
    });

    const acts = el('div', 'script-actions');
    const useBtn = el('button', 'item-act use' + (used ? ' on' : ''), used ? '取消' : '使用');
    useBtn.type = 'button';
    useBtn.title = used ? '标记为未使用' : '标记为使用';
    useBtn.addEventListener('click', (e) => {
      e.stopPropagation();
      toggleUsage(entry, !used);
    });
    const delBtn = el('button', 'item-act del', '删除');
    delBtn.type = 'button';
    delBtn.title = '删除这个脚本（需点两次确认）';
    delBtn.addEventListener('click', (e) => {
      e.stopPropagation();
      askDeleteScript(row, delBtn, entry);
    });
    acts.appendChild(useBtn);
    acts.appendChild(delBtn);

    row.appendChild(btn);
    row.appendChild(acts);
    scriptList.appendChild(row);
  });
}

/* ---- 双击重命名：回车保存、Esc 取消、空名不允许 ---- */
function startRename(row, entry, nameEl) {
  if (row.classList.contains('renaming')) return;
  row.classList.add('renaming');
  const input = el('input', 'name-input');
  input.type = 'text';
  input.value = entry.name;
  input.maxLength = 80;
  input.setAttribute('aria-label', '重命名脚本');
  nameEl.replaceWith(input);
  input.focus();
  input.select();

  let done = false;
  const finish = async (commit) => {
    if (done) return;
    const next = input.value.trim();
    if (!commit) {
      done = true;
      input.replaceWith(nameEl);
      row.classList.remove('renaming');
      return;
    }
    if (!next) {
      toast('脚本名不能为空');
      input.focus();
      input.select();
      return;
    }
    if (next === entry.name) {
      done = true;
      input.replaceWith(nameEl);
      row.classList.remove('renaming');
      return;
    }
    done = true;
    pendingMove = entry.name;
    try {
      const data = await api('/api/script/rename', {
        method: 'POST',
        body: { product: entry.product, script: entry.name, name: next },
      });
      row.classList.remove('renaming');
      toast('已重命名为「' + data.name + '」');
      const wasCurrent = entry.product === state.product && entry.name === state.script;
      if (wasCurrent) {
        stopSession();
        await refreshAll(entry.product, data.name);
      } else {
        await loadTree();
        renderScriptList();
      }
      pendingMove = '';
    } catch (err) {
      pendingMove = '';
      done = false;
      toast(err.message, 4200);
      input.focus();
      input.select();
    }
  };

  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      finish(true);
    } else if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      finish(false);
    }
  });
  input.addEventListener('blur', () => {
    if (!done) finish(input.value.trim() !== '');
  });
  input.addEventListener('click', (e) => e.stopPropagation());
  input.addEventListener('dblclick', (e) => e.stopPropagation());
}

/* ---- 使用 / 未使用：后端持久化，并广播给所有在线同事 ---- */
async function toggleUsage(entry, used) {
  try {
    await api('/api/usage', {
      method: 'POST',
      body: { product: entry.product, script: entry.name, used, name: me && me.name },
    });
    const key = scriptKey(entry.product, entry.name);
    if (used) state.used.add(key);
    else state.used.delete(key);
    renderScriptList();
    toast(used ? '已标记为使用' : '已标记为未使用');
  } catch (err) {
    toast(err.message, 4200);
  }
}

/* ---- 删除脚本：弹二次确认，说明会连同文件夹与视频一起删除 ---- */
async function askDeleteScript(row, btn, entry) {
  let pre = null;
  try {
    pre = await api(
      '/api/delete/precheck?type=script&product=' +
        encodeURIComponent(entry.product) +
        '&script=' +
        encodeURIComponent(entry.name)
    );
  } catch (_) {
    pre = null;
  }
  openConfirm({
    title: '删除脚本「' + entry.name + '」？',
    desc: buildDeleteDesc('script', pre),
    confirmText: '确认删除',
    onConfirm: () => deleteScript(entry),
  });
}

function buildDeleteDesc(kind, pre) {
  const lines = [];
  if (kind === 'product') {
    lines.push('会同时删除该产品文件夹及其中的全部脚本、视频文件，不可恢复。');
  } else {
    lines.push('会同时删除该脚本文件夹及其中的视频文件，不可恢复。');
  }
  if (pre) {
    lines.push('将删除 ' + pre.files + ' 个文件，合计 ' + humanSize(pre.size) + '（其中视频 ' + pre.videos + ' 个）。');
    lines.push('目录：' + pre.path);
  } else {
    lines.push('文件数量与大小暂时无法统计，请确认下方目录后再删除。');
  }
  lines.push('仍被其他脚本或剪辑素材引用的视频会保留；剪映草稿目录不会被动到。');
  return lines.join('\n');
}

async function deleteScript(entry) {
  const current = entry.product === state.product && entry.name === state.script;
  pendingDelete = entry.name;
  try {
    const res = await api('/api/script/delete', {
      method: 'POST',
      body: { product: entry.product, script: entry.name, name: me && me.name },
    });
    state.used.delete(scriptKey(entry.product, entry.name));
    toast(res.message || '已删除脚本「' + entry.name + '」', 4200);
    if (current) {
      stopSession();
      state.script = '';
      state.doc = null;
      await refreshAll(entry.product);
    } else {
      await loadTree();
      renderScriptList();
    }
  } finally {
    pendingDelete = '';
  }
}

async function askDeleteProduct() {
  const name = state.product;
  if (!name) {
    toast('请先选择产品', 2600);
    return;
  }
  let pre = null;
  try {
    pre = await api('/api/delete/precheck?type=product&product=' + encodeURIComponent(name));
  } catch (_) {
    pre = null;
  }
  openConfirm({
    title: '删除产品「' + name + '」？',
    desc: buildDeleteDesc('product', pre),
    confirmText: '确认删除',
    onConfirm: () => deleteProduct(name),
  });
}

async function deleteProduct(name) {
  pendingProduct = name;
  try {
    const res = await api('/api/product/delete', {
      method: 'POST',
      body: { product: name, name: me && me.name },
    });
    state.used = new Set(Array.from(state.used).filter((key) => key.indexOf(name + '/') !== 0));
    if (state.product === name) {
      stopSession();
      state.product = '';
      state.script = '';
      state.doc = null;
    }
    await refreshAll();
    toast(res.message || '已删除产品「' + name + '」', 4200);
  } finally {
    pendingProduct = '';
  }
}

/* ---- 全文搜索：名称过滤之外，再按镜头内容 / 口播 / 文案检索 ---- */
function scheduleContentSearch() {
  clearTimeout(searchTimer);
  const q = listUi.q.trim();
  if (!q) {
    listUi.searchSeq += 1;
    listUi.hits = [];
    listUi.hitsTruncated = false;
    listUi.searching = false;
    renderSearchResults();
    return;
  }
  searchTimer = setTimeout(() => runContentSearch(q), 260);
}

async function runContentSearch(q) {
  const seq = ++listUi.searchSeq;
  listUi.searching = true;
  renderSearchResults();
  try {
    const product = listUi.scope === '__all' ? '' : (listUi.scope || state.product);
    const url = '/api/search?q=' + encodeURIComponent(q) + (product ? '&product=' + encodeURIComponent(product) : '');
    const data = await api(url);
    if (seq !== listUi.searchSeq) return;
    listUi.hits = data.items || [];
    listUi.hitsTruncated = !!data.truncated;
  } catch (err) {
    if (seq !== listUi.searchSeq) return;
    listUi.hits = [];
    listUi.hitsTruncated = false;
  } finally {
    if (seq === listUi.searchSeq) {
      listUi.searching = false;
      renderSearchResults();
    }
  }
}

function appendHighlighted(container, text, q) {
  const needle = q.toLowerCase();
  if (!needle) {
    container.textContent = text;
    return;
  }
  const lower = text.toLowerCase();
  let from = 0;
  for (;;) {
    const idx = lower.indexOf(needle, from);
    if (idx < 0) {
      container.appendChild(document.createTextNode(text.slice(from)));
      break;
    }
    if (idx > from) container.appendChild(document.createTextNode(text.slice(from, idx)));
    container.appendChild(el('mark', null, text.slice(idx, idx + needle.length)));
    from = idx + needle.length;
  }
}

function renderSearchResults() {
  const q = listUi.q.trim();
  if (!q || listUi.closed) {
    searchResults.classList.add('hidden');
    searchResults.innerHTML = '';
    return;
  }
  searchResults.classList.remove('hidden');
  searchResults.innerHTML = '';
  const head = el('div', 'search-results-head');
  head.appendChild(el('span', null, '镜头内容命中'));
  head.appendChild(el('span', null, listUi.searching ? '搜索中…' : String(listUi.hits.length)));
  searchResults.appendChild(head);

  if (!listUi.hits.length) {
    searchResults.appendChild(el('div', 'search-empty', listUi.searching ? '正在检索镜头内容…' : '没有匹配的镜头内容'));
    return;
  }

  /* 按脚本分组，组头可折叠；↑↓ 只在当前展开的命中项之间移动 */
  const groups = [];
  const index = new Map();
  listUi.hits.forEach((hit) => {
    const key = hit.product + '/' + hit.script;
    let g = index.get(key);
    if (!g) {
      g = { key, product: hit.product, script: hit.script, hits: [] };
      index.set(key, g);
      groups.push(g);
    }
    g.hits.push(hit);
  });

  const fieldName = { content: '镜头内容', narration: '口播', copy: '文案' };
  groups.forEach((g) => {
    const closed = listUi.collapsed.has(g.key);
    const group = el('div', 'search-group');
    const groupHead = el('button', 'search-group-head');
    groupHead.type = 'button';
    groupHead.title = closed ? '展开 ' + g.script : '折叠 ' + g.script;
    groupHead.appendChild(el('span', 'search-caret', closed ? '▸' : '▾'));
    groupHead.appendChild(el('span', 'search-group-name', g.script));
    if (g.product) groupHead.appendChild(el('span', 'search-group-prod', g.product));
    groupHead.appendChild(el('span', 'search-group-count', String(g.hits.length)));
    groupHead.addEventListener('click', () => {
      if (closed) listUi.collapsed.delete(g.key);
      else listUi.collapsed.add(g.key);
      listUi.closed = false;
      renderSearchResults();
      scriptSearch.focus();
    });
    group.appendChild(groupHead);

    if (!closed) {
      g.hits.forEach((hit) => {
        const item = el('button', 'search-hit');
        item.type = 'button';
        item.title = hit.product + ' / ' + hit.script + ' · 镜头 ' + (hit.no || '?');
        const top = el('div', 'search-hit-top');
        top.appendChild(el('span', 'search-hit-no', '镜头 ' + (hit.no || '?')));
        top.appendChild(el('span', 'search-hit-field', fieldName[hit.field] || hit.field));
        item.appendChild(top);
        const body = el('div', 'search-hit-text');
        appendHighlighted(body, hit.snippet, q);
        item.appendChild(body);
        item.addEventListener('click', () => focusShot(hit));
        group.appendChild(item);
      });
    }
    searchResults.appendChild(group);
  });

  if (listUi.hitsTruncated) {
    searchResults.appendChild(el('div', 'search-empty', '结果较多，仅显示前 80 条'));
  }
  searchResults.appendChild(el('div', 'search-hint', '↑↓ 选择 · Enter 跳到镜头 · Esc 关闭'));
  applySearchActive();
}

function applySearchActive() {
  const items = $$('.search-hit', searchResults);
  if (!items.length) return;
  if (listUi.active < 0) listUi.active = 0;
  if (listUi.active > items.length - 1) listUi.active = items.length - 1;
  items.forEach((it, i) => it.classList.toggle('active', i === listUi.active));
  items[listUi.active].scrollIntoView({ block: 'nearest' });
}

async function focusShot(hit) {
  setMobileDetail(true);
  await selectScriptOf(hit.product, hit.script);
  requestAnimationFrame(() => flashShot(hit.shotId));
}

function flashShot(id) {
  const row = rowsEl.querySelector('.row[data-id="' + id + '"]');
  if (!row) {
    toast('该镜头已不存在，可能被删除了');
    return;
  }
  row.scrollIntoView({ block: 'center', behavior: 'smooth' });
  row.classList.add('row-flash');
  setTimeout(() => row.classList.remove('row-flash'), 1700);
}

function renderEditor() {
  const hasDoc = !!state.doc;
  editor.classList.toggle('hidden', !hasDoc);
  emptyState.classList.toggle('hidden', hasDoc);
  if (hasDoc) {
    scriptTitle.textContent = state.script;
    crumbProduct.textContent = state.product;
    crumbSep.style.display = '';
    shotCount.textContent = `${state.doc.shots.length} 个镜头`;
  }
}

/* ---- 行首多选：批量删除 / 批量打点切片 / 重排序号 ---- */
const selection = new Set();

function pruneSelection() {
  if (!state.doc) {
    selection.clear();
    return;
  }
  const ids = new Set(state.doc.shots.map((s) => s.id));
  Array.from(selection).forEach((id) => {
    if (!ids.has(id)) selection.delete(id);
  });
}

function renderBatchBar() {
  const n = selection.size;
  batchBar.classList.toggle('hidden', n === 0);
  if (n) batchCount.textContent = `已选 ${n} 个镜头`;
}

function syncSelectAll() {
  const ids = state.doc ? state.doc.shots.map((s) => s.id) : [];
  const selected = ids.filter((id) => selection.has(id)).length;
  selectAll.checked = ids.length > 0 && selected === ids.length;
  selectAll.indeterminate = selected > 0 && selected < ids.length;
}

function applySelectionToRows() {
  $$('.row', rowsEl).forEach((row) => {
    const on = selection.has(row.dataset.id);
    row.classList.toggle('selected', on);
    const box = row.querySelector('.pick-box');
    if (box) box.checked = on;
  });
}

function batchDeleteShots() {
  if (!state.doc || !state.regs) return;
  const shots = state.doc.shots.filter((s) => selection.has(s.id));
  if (!shots.length) return;
  if (state.doc.shots.length - shots.length <= 0) {
    toast('至少保留一个镜头');
    return;
  }
  openConfirm({
    title: `删除选中的 ${shots.length} 个镜头？`,
    desc: '镜头内容、口播、文案和视频引用会一起删除，同事那边也会同步收起。此操作无法撤销。',
    confirmText: '批量删除',
    onConfirm: () => {
      const regs = {};
      shots.forEach((shot) => {
        Object.assign(regs, Collab.deleteShot(state.regs, me.site, shot.id));
      });
      pushRegs(regs);
      selection.clear();
      state.doc = normalizeDoc(Collab.project(state.regs, state.product, state.script), state.product, state.script);
      renderEditor();
      renderTable();
      toast(`已删除 ${shots.length} 个镜头`);
    },
  });
}

/* 批量打点切片：按当前行顺序，把剪辑面板的片段依次切到选中镜头 */
let batchSlicing = false;

async function batchSliceShots() {
  if (batchSlicing) return;
  if (!state.doc || !state.regs) return;
  const shots = state.doc.shots.filter((s) => selection.has(s.id));
  if (!shots.length) return;
  if (!state.clip) {
    setClipPanel(true, true);
    toast('请先在剪辑面板导入原片并打点');
    return;
  }
  if (!state.toolsReady) {
    openSettings();
    toast('未找到 ffmpeg，无法切片');
    return;
  }
  const segs = segmentList();
  if (!segs.length) {
    setClipPanel(true, true);
    toast('还没有可用片段，先在剪辑面板打点或智能裁切');
    return;
  }
  const total = Math.min(shots.length, segs.length);
  const skipped = shots.length - total;
  const docRef = state.doc;
  let ok = 0;
  const failed = [];
  batchSlicing = true;
  try {
    for (let i = 0; i < total; i++) {
      const shot = shots[i];
      const seg = segs[i];
      const no = String(shot.no || '').trim();
      batchCount.textContent = `切片中 ${i + 1}/${total}…`;
      if (!no) {
        failed.push(`第 ${i + 1} 个（缺镜头序号）`);
        continue;
      }
      try {
        const data = await api('/api/clip', {
          method: 'POST',
          body: {
            product: state.product,
            script: state.script,
            shot: no,
            src: 'temp',
            file: state.clip.file,
            in: seg.start,
            out: seg.end,
          },
        });
        if (state.doc !== docRef) return;
        editShot(shot, 'video', {
          src: 'script',
          file: data.file,
          in: 0,
          out: 0,
          label: `镜头 ${no}`,
        });
        ok++;
      } catch (err) {
        failed.push(`镜头 ${no}（${err.message}）`);
      }
    }
  } finally {
    batchSlicing = false;
  }
  if (state.doc !== docRef) return;
  state.doc = normalizeDoc(Collab.project(state.regs, state.product, state.script), state.product, state.script);
  renderEditor();
  renderTable();
  const tail = skipped > 0 ? `；片段只有 ${segs.length} 个，剩余 ${skipped} 个未处理` : '';
  if (failed.length) {
    toast(`批量切片：成功 ${ok} 个，失败 ${failed.length} 个（${failed.slice(0, 2).join('；')}）${tail}`, 5200);
    if (/ffmpeg/i.test(failed.join(' '))) openSettings();
  } else {
    toast(`批量切片完成：${ok} 个镜头已按序号落盘${tail}`, skipped ? 4600 : 3000);
  }
}

/* 重排序号：按当前表格行顺序把序号改成 1..N，并同步给协作端 */
function renumberShots() {
  if (!state.doc || !state.regs) return;
  const shots = state.doc.shots.slice();
  if (!shots.length) return;
  const regs = {};
  shots.forEach((shot, i) => {
    const key = Collab.shotKey(shot.id, 'no');
    regs[key] = Collab.setReg(state.regs, key, String(i + 1), me.site);
  });
  pushRegs(regs);
  state.doc = normalizeDoc(Collab.project(state.regs, state.product, state.script), state.product, state.script);
  renderEditor();
  renderTable();
  toast(`已按当前顺序重排为 1–${shots.length}`);
}

function renderTable() {
  rowsEl.innerHTML = '';
  if (!state.doc) {
    emptyState.innerHTML = '';
    const card = el('div', 'empty-card');
    if (!state.products.length) {
      card.appendChild(el('h2', null, '还没有产品分组'));
      card.appendChild(el('p', null, '「脚本管理」目录会按产品分文件夹。\n先创建一个产品，再开始写脚本。'));
      const btn = el('button', 'btn primary', '新建产品');
      btn.type = 'button';
      btn.addEventListener('click', createProduct);
      card.appendChild(btn);
    } else if (state.script) {
      card.appendChild(el('h2', null, '脚本加载失败'));
      card.appendChild(el('p', null, `无法读取「${state.script}」的 文案.json，\n请检查文件是否存在或稍后重试。`));
      const btn = el('button', 'btn primary', '重新加载');
      btn.type = 'button';
      btn.addEventListener('click', () => loadDocFor(state.product, state.script));
      card.appendChild(btn);
    } else {
      card.appendChild(el('h2', null, '这个产品还没有脚本'));
      card.appendChild(el('p', null, `当前产品「${state.product}」下还没有文案脚本。\n新建后会自动生成 文案.json。`));
      const btn = el('button', 'btn primary', '新建脚本');
      btn.type = 'button';
      btn.addEventListener('click', createScript);
      card.appendChild(btn);
    }
    emptyState.appendChild(card);
    pruneSelection();
    renderBatchBar();
    syncSelectAll();
    return;
  }
  pruneSelection();
  rowsEl.classList.toggle('virtualized', state.doc.shots.length > 150);
  const created = renderRowsDiff(state.doc.shots);
  renderBatchBar();
  syncSelectAll();
  requestAnimationFrame(() => {
    created.forEach((row) => {
      $$('.cell-input', row).forEach(autoGrow);
      $$('.no-input', row).forEach(fitNoInput);
    });
    paintPresence();
  });
}

/* 行级 diff：id 相同的行复用原 DOM 只更新值，增减的行单独处理，顺序按 shots 对齐。
   rowsEl 为空时（首次加载 / 切换脚本）自然退化为全量构建。 */
function renderRowsDiff(shots) {
  const nextIds = new Set();
  shots.forEach((s) => nextIds.add(s.id));
  const existing = new Map();
  Array.from(rowsEl.children).forEach((node) => {
    if (node.dataset && node.dataset.id && nextIds.has(node.dataset.id)) existing.set(node.dataset.id, node);
    else node.remove();
  });
  const created = [];
  let anchor = null;
  shots.forEach((shot) => {
    let row = existing.get(shot.id);
    if (row) {
      refreshRowValues(row, shot);
    } else {
      row = buildRow(shot);
      created.push(row);
    }
    const wanted = anchor ? anchor.nextSibling : rowsEl.firstChild;
    if (row !== wanted) rowsEl.insertBefore(row, wanted);
    anchor = row;
  });
  return created;
}

/* 把行闭包引用的旧 shot 对象就地更新，再按需刷新行内控件 */
function refreshRowValues(row, shot) {
  const cur = row._shot || shot;
  ['no', 'content', 'narration', 'copy'].forEach((f) => { cur[f] = shot[f]; });
  const videoChanged = JSON.stringify(cur.video || null) !== JSON.stringify(shot.video || null);
  if (videoChanged) cur.video = shot.video;
  const box = row.querySelector('.pick-box');
  if (box) {
    box.checked = selection.has(shot.id);
    row.classList.toggle('selected', box.checked);
    box.setAttribute('aria-label', '选择镜头 ' + shot.no);
  }
  const del = row.querySelector('.shot-del');
  if (del) del.setAttribute('aria-label', '删除镜头 ' + shot.no);
  ['no', 'content', 'narration', 'copy'].forEach((field) => {
    const input = row.querySelector(`[data-field="${field}"]`);
    if (!input || input === document.activeElement) return;
    if (input.value !== cur[field]) {
      input.value = cur[field];
      if (input.tagName === 'TEXTAREA') autoGrow(input);
    }
  });
  if (videoChanged) {
    const cell = row.querySelector('.video-cell');
    if (cell && cell._drawVideo) cell._drawVideo();
  }
}

function buildRow(shot) {
  const row = el('div', 'row');
  row.dataset.id = shot.id;
  row._shot = shot;

  const pickCell = el('div', 'cell pick-cell');
  const box = el('input', 'pick-box');
  box.type = 'checkbox';
  box.checked = selection.has(shot.id);
  box.setAttribute('aria-label', '选择镜头 ' + shot.no);
  if (box.checked) row.classList.add('selected');
  box.addEventListener('change', () => {
    if (box.checked) selection.add(shot.id);
    else selection.delete(shot.id);
    row.classList.toggle('selected', box.checked);
    renderBatchBar();
    syncSelectAll();
  });
  pickCell.appendChild(box);
  row.appendChild(pickCell);

  const noCell = el('div', 'cell no-wrap');
  noCell.dataset.key = Collab.shotKey(shot.id, 'no');
  const del = el('button', 'shot-del', '×');
  del.type = 'button';
  del.title = '删除这一镜头';
  del.setAttribute('aria-label', '删除镜头 ' + shot.no);
  del.addEventListener('click', () => confirmDeleteShot(shot));
  const noInput = el('input', 'no-input');
  noInput.type = 'text';
  noInput.dataset.field = 'no';
  noInput.value = shot.no;
  noInput.setAttribute('aria-label', '镜头序号');
  noInput.addEventListener('input', () => {
    editShot(shot, 'no', noInput.value);
  });
  noCell.appendChild(del);
  noCell.appendChild(noInput);

  row.appendChild(noCell);
  row.appendChild(buildTextCell(shot, 'content', '画面里发生什么'));
  row.appendChild(buildTextCell(shot, 'narration', '口播逐字稿', 'narration'));
  row.appendChild(buildTextCell(shot, 'copy', '屏幕字幕 / 标题文案', 'copy'));
  row.appendChild(buildVideoCell(shot));
  return row;
}

function buildTextCell(shot, field, placeholder, extraCls) {
  const cell = el('div', 'cell');
  cell.dataset.key = Collab.shotKey(shot.id, field);
  const ta = el('textarea', 'cell-input' + (extraCls ? ' ' + extraCls : ''));
  ta.rows = 1;
  ta.dataset.field = field;
  ta.placeholder = placeholder;
  ta.value = shot[field] || '';
  ta.addEventListener('input', () => {
    editShot(shot, field, ta.value);
    autoGrow(ta);
  });
  cell.appendChild(ta);
  return cell;
}

function autoGrow(ta) {
  ta.style.height = 'auto';
  const h = Math.min(320, Math.max(38, ta.scrollHeight));
  ta.style.height = h + 'px';
  ta.style.overflowY = ta.scrollHeight > 320 ? 'auto' : 'hidden';
}

function fitNoInput(input) {
  input.style.width = 'auto';
}

/* 删除镜头：复用协作层的墓碑寄存器（dead），旧数据同步回来也不会复活这一行 */
function confirmDeleteShot(shot) {
  if (!state.doc || !state.regs) return;
  if (state.doc.shots.length <= 1) {
    toast('至少保留一个镜头');
    return;
  }
  const label = String(shot.no || '').trim();
  openConfirm({
    title: label ? `删除镜头 ${label}？` : '删除这个镜头？',
    desc: '这一行的镜头内容、口播、文案和视频引用会一起删除，同事那边也会同步收起。此操作无法撤销。',
    confirmText: '删除镜头',
    onConfirm: () => {
      const regs = Collab.deleteShot(state.regs, me.site, shot.id);
      selection.delete(shot.id);
      pushRegs(regs);
      state.doc = normalizeDoc(Collab.project(state.regs, state.product, state.script), state.product, state.script);
      renderEditor();
      renderTable();
      toast('已删除镜头' + (label ? ' ' + label : ''));
    },
  });
}

/* ============ 视频单元格 ============ */
function buildVideoCell(shot) {
  const cell = el('div', 'cell video-cell');
  cell.dataset.key = Collab.shotKey(shot.id, 'video');
  bindCellDrop(cell, shot);

  const draw = () => drawVideoCell(cell, shot);
  cell._drawVideo = draw;
  draw();
  return cell;
}

/* 视频懒加载：只有快滚进视口的卡片才真正发起 metadata 请求，
   几百行时打开脚本不会再出现 Range 请求风暴。 */
let videoLazyIO = null;

function loadLazyVideo(videoEl) {
  const src = videoEl.dataset.src;
  if (!src || videoEl.src) return;
  delete videoEl.dataset.src;
  videoEl.preload = 'metadata';
  videoEl.src = src;
}

function observeLazyVideo(videoEl) {
  if (!('IntersectionObserver' in window)) {
    loadLazyVideo(videoEl);
    return;
  }
  if (!videoLazyIO) {
    videoLazyIO = new IntersectionObserver((entries) => {
      entries.forEach((entry) => {
        if (!entry.isIntersecting) return;
        loadLazyVideo(entry.target);
        videoLazyIO.unobserve(entry.target);
      });
    }, { root: null, rootMargin: '400px 0px' });
  }
  videoLazyIO.observe(videoEl);
}

function drawVideoCell(cell, shot) {
  cell.innerHTML = '';
  cell.classList.remove('drag-over');

  if (!shot.video) {
    const dz = el('div', 'clip-drop');
    dz.appendChild(el('div', 'dz-main', '拖入视频'));
    dz.appendChild(el('div', null, '或点击选择'));
    dz.addEventListener('click', () => pickVideoFile((file) => attachExternalVideo(shot, file, cell)));
    cell.appendChild(dz);
    return;
  }

  const ref = shot.video;
  const card = el('div', 'clip-card');
  const video = document.createElement('video');
  video.muted = true;
  video.playsInline = true;
  video.preload = 'none';
  video.dataset.src = mediaUrl(ref);
  observeLazyVideo(video);
  video.addEventListener('loadedmetadata', () => {
    const t = ref.in || 0;
    if (t > 0 && Number.isFinite(video.duration)) {
      video.currentTime = Math.min(t + 0.05, Math.max(0, video.duration - 0.05));
    }
  }, { once: true });
  card.appendChild(video);
  card.appendChild(el('div', 'card-shade'));
  card.appendChild(el('div', 'card-play', '▶'));

  if (ref.out > ref.in) {
    card.appendChild(el('div', 'range-tag', `${fmt(ref.in)}–${fmt(ref.out)}`));
  }
  card.appendChild(el('div', 'card-label', ref.label || ref.file));
  card.title = '点击预览';

  const del = el('button', 'card-del', '×');
  del.type = 'button';
  del.title = '移除视频';
  del.addEventListener('click', (e) => {
    e.stopPropagation();
    editShot(shot, 'video', null);
    drawVideoCell(cell, shot);
  });
  card.appendChild(del);

  card.addEventListener('click', () => {
    openOverlay({
      url: mediaUrl(ref),
      start: ref.in || 0,
      end: ref.out || 0,
      label: ref.label || `镜头 ${shot.no}`,
    });
  });

  cell.appendChild(card);
}

function bindCellDrop(cell, shot) {
  cell.addEventListener('dragover', (e) => {
    if (hasFileDrag(e) || hasClipDrag(e)) {
      e.preventDefault();
      e.dataTransfer.dropEffect = 'copy';
      cell.classList.add('drag-over');
    }
  });
  cell.addEventListener('dragleave', (e) => {
    if (!cell.contains(e.relatedTarget)) cell.classList.remove('drag-over');
  });
  cell.addEventListener('drop', (e) => {
    const clipRaw = e.dataTransfer ? e.dataTransfer.getData('application/x-clip') : '';
    const file = e.dataTransfer && e.dataTransfer.files ? e.dataTransfer.files[0] : null;
    if (!clipRaw && !file) return;
    e.preventDefault();
    e.stopPropagation();
    cell.classList.remove('drag-over');

    if (clipRaw) {
      let clip = null;
      try {
        clip = JSON.parse(clipRaw);
        if (!clip || !clip.file) throw new Error('bad clip');
      } catch (_) {
        toast('片段数据无效');
        return;
      }
      applySegmentToShot(shot, cell, clip);
      return;
    }
    attachExternalVideo(shot, file, cell);
  });
}

/* 把剪辑面板的片段按镜头序号切成独立文件落盘 */
async function applySegmentToShot(shot, cell, clip) {
  if (!state.doc) return;
  const docRef = state.doc;
  const no = String(shot.no || '').trim();
  if (!no) {
    toast('请先填写这个镜头的镜头序号');
    return;
  }
  if (!state.toolsReady) {
    openSettings();
    toast('未找到 ffmpeg，无法把片段按镜头序号落盘');
    return;
  }
  cell.innerHTML = '';
  const busy = el('div', 'clip-drop uploading');
  busy.appendChild(el('div', 'dz-main', '切片中…'));
  busy.appendChild(el('div', null, `镜头 ${no}`));
  cell.appendChild(busy);
  try {
    const data = await api('/api/clip', {
      method: 'POST',
      body: {
        product: state.product,
        script: state.script,
        shot: no,
        src: clip.src || 'temp',
        file: clip.file,
        in: Number(clip.in) || 0,
        out: Number(clip.out) || 0,
      },
    });
    if (state.doc !== docRef) return;
    const length = (Number(clip.out) || 0) - (Number(clip.in) || 0);
    editShot(shot, 'video', {
      src: 'script',
      file: data.file,
      in: 0,
      out: 0,
      label: `镜头 ${no}`,
    });
    if (cell._drawVideo) cell._drawVideo();
    toast(length > 0
      ? `已按镜头 ${no} 落盘：${data.file}（${fmt(length)}）`
      : `已按镜头 ${no} 落盘：${data.file}`);
  } catch (err) {
    toast(err.message);
    if (/ffmpeg/i.test(err.message)) openSettings();
    if (cell._drawVideo) cell._drawVideo();
  }
}

async function attachExternalVideo(shot, file, cell) {
  if (!isVideoFile(file)) {
    toast('请拖入视频文件');
    return;
  }
  const docRef = state.doc;
  const product = state.product;
  const script = state.script;
  const dz = cell.querySelector('.clip-drop');
  if (dz) {
    dz.classList.add('uploading');
    dz.innerHTML = '<div class="dz-main">排队上传…</div>';
  }
  const params = new URLSearchParams({ product, script, shot: shot.no || 'clip' });
  const job = queueUpload({
    file,
    url: '/api/upload?' + params.toString(),
    label: `镜头 ${shot.no || ''} · ${file.name}`,
    onProgress: (p) => {
      const main = dz && dz.querySelector('.dz-main');
      if (main) main.textContent = '上传中 ' + Math.round(p * 100) + '%';
    },
    onSuccess: (data) => {
      if (state.doc !== docRef) {
        toast('视频已上传，但脚本已切换，请重新拖入');
        return;
      }
      shot.video = { src: 'script', file: data.file, in: 0, out: 0, label: file.name };
      editShot(shot, 'video', shot.video);
      if (cell._drawVideo) cell._drawVideo();
      toast('视频已放入镜头 ' + shot.no);
    },
    onFail: (err) => {
      toast('上传失败：' + err.message + '（可在上传队列点「重试」）', 4200);
      if (cell._drawVideo) cell._drawVideo();
    },
    onCancel: () => {
      toast('已取消上传');
      if (cell._drawVideo) cell._drawVideo();
    },
  });
  if (dz) {
    const cancel = el('button', 'upload-cancel-btn', '取消');
    cancel.type = 'button';
    cancel.addEventListener('click', (e) => {
      e.stopPropagation();
      job.cancel();
    });
    dz.appendChild(cancel);
  }
}

function mediaUrl(ref) {
  if (!ref || !ref.file) return '';
  if (ref.src === 'temp') return '/media/_temp/' + encodeURIComponent(ref.file);
  return '/media/' + encodeURIComponent(state.product) + '/' + encodeURIComponent(state.script) + '/' + encodeURIComponent(ref.file);
}

/* ============ 文件选择 ============ */
let pickCallback = null;
const filePicker = document.createElement('input');
filePicker.type = 'file';
filePicker.accept = 'video/*';
filePicker.style.display = 'none';
document.body.appendChild(filePicker);
filePicker.addEventListener('change', () => {
  const file = filePicker.files && filePicker.files[0];
  filePicker.value = '';
  const cb = pickCallback;
  pickCallback = null;
  if (file && cb) cb(file);
});

function pickVideoFile(cb) {
  pickCallback = cb;
  filePicker.click();
}

/* ============ 新建产品 / 脚本 ============ */
function createProduct() {
  openModal({
    title: '新建产品',
    desc: '产品会成为「脚本管理」下的第一层文件夹。',
    placeholder: '例如：新品发布会',
    onSubmit: async (name) => {
      await api('/api/product', { method: 'POST', body: { name } });
      await refreshAll(name);
      toast('产品已创建');
    },
  });
}

function createScript() {
  if (!state.product) {
    createProduct();
    return;
  }
  const product = state.product;
  openModal({
    title: '新建脚本',
    desc: `将在「${product}」下创建同名文件夹与 文案.json。`,
    placeholder: '例如：开场口播',
    onSubmit: async (name) => {
      await api('/api/script', { method: 'POST', body: { product, name } });
      /* 新建后清掉搜索与筛选，保证新脚本一定出现在列表里 */
      listUi.q = '';
      listUi.scope = '';
      scriptSearch.value = '';
      await refreshAll(product, name);
      setMobileDetail(true);
      toast('脚本已创建');
    },
  });
}

/* ============ 弹窗 ============ */
let modalSubmit = null;
let modalMode = 'prompt';

function resetModalChrome() {
  modal.classList.remove('confirm');
  modalInput.classList.remove('hidden');
  modalConfirm.textContent = '创建';
  modalConfirm.className = 'btn primary';
}

function openModal({ title, desc, placeholder, onSubmit, confirmText }) {
  modalMode = 'prompt';
  resetModalChrome();
  if (confirmText) modalConfirm.textContent = confirmText;
  modalTitle.textContent = title;
  modalDesc.textContent = desc || '';
  modalInput.placeholder = placeholder || '';
  modalInput.value = '';
  modalError.textContent = '';
  modalError.classList.add('hidden');
  modalConfirm.disabled = false;
  modal.classList.remove('hidden');
  modalSubmit = onSubmit;
  setTimeout(() => modalInput.focus(), 0);
}

/* 轻确认弹窗：复用同一套 #modal 样式，没有输入框 */
function openConfirm({ title, desc, confirmText = '删除', onConfirm }) {
  modalMode = 'confirm';
  resetModalChrome();
  modal.classList.add('confirm');
  modalInput.classList.add('hidden');
  modalTitle.textContent = title;
  modalDesc.textContent = desc || '';
  modalError.textContent = '';
  modalError.classList.add('hidden');
  modalConfirm.textContent = confirmText;
  modalConfirm.className = 'btn danger';
  modalConfirm.disabled = false;
  modal.classList.remove('hidden');
  modalSubmit = onConfirm;
  setTimeout(() => modalConfirm.focus(), 0);
}

function closeModal() {
  modal.classList.add('hidden');
  modalSubmit = null;
}

modalForm.addEventListener('submit', async (e) => {
  e.preventDefault();
  if (!modalSubmit) return;
  const value = modalInput.value.trim();
  if (modalMode !== 'confirm' && !value) {
    modalError.textContent = '请输入名称';
    modalError.classList.remove('hidden');
    return;
  }
  modalConfirm.disabled = true;
  try {
    await modalSubmit(value);
    closeModal();
  } catch (err) {
    modalError.textContent = err.message;
    modalError.classList.remove('hidden');
  } finally {
    modalConfirm.disabled = false;
  }
});
modalCancel.addEventListener('click', closeModal);
modal.addEventListener('mousedown', (e) => {
  if (e.target === modal) closeModal();
});

/* ============ 预览浮层 ============ */
const ovState = { start: 0, end: 0, open: false };

function openOverlay({ url, start = 0, end = 0, label = '' }) {
  if (!url) return;
  ovState.start = start || 0;
  ovState.end = end || 0;
  ovState.open = true;
  overlay.classList.remove('hidden');
  overlay.classList.remove('playing');
  ovLabel.textContent = label || '';

  const abs = new URL(url, location.href).href;
  const applyStart = () => {
    try { ovVideo.currentTime = ovState.start; } catch (_) { /* ignore */ }
    updateOverlayTime();
  };
  if (ovVideo.src !== abs) {
    ovVideo.src = url;
    ovVideo.addEventListener('loadedmetadata', applyStart, { once: true });
  } else if (ovVideo.readyState >= 1) {
    applyStart();
  } else {
    ovVideo.addEventListener('loadedmetadata', applyStart, { once: true });
  }
  const p = ovVideo.play();
  if (p && p.catch) p.catch(() => { /* 等待用户点击 */ });
}

function closeOverlay() {
  ovState.open = false;
  ovVideo.pause();
  overlay.classList.add('hidden');
  overlay.classList.remove('playing');
  ovVideo.removeAttribute('src');
  ovVideo.load();
}

function toggleOverlayPlay() {
  if (ovVideo.paused) {
    if (ovState.end > 0 && ovVideo.currentTime >= ovState.end - 0.03) {
      ovVideo.currentTime = ovState.start;
    }
    const p = ovVideo.play();
    if (p && p.catch) p.catch(() => {});
  } else {
    ovVideo.pause();
  }
}

function updateOverlayTime() {
  const t = ovVideo.currentTime || 0;
  const d = ovVideo.duration || 0;
  ovTime.textContent = fmt(t) + ' / ' + fmt(d);
  ovProgress.style.width = d ? Math.min(100, (t / d) * 100) + '%' : '0%';
}

ovVideo.addEventListener('click', toggleOverlayPlay);
overlay.addEventListener('click', (e) => {
  if (e.target === overlay || e.target === overlayStage) closeOverlay();
});
ovVideo.addEventListener('play', () => {
  overlay.classList.add('playing');
  ovPlay.textContent = '❚❚';
});
ovVideo.addEventListener('pause', () => {
  overlay.classList.remove('playing');
  ovPlay.textContent = '▶';
});
ovVideo.addEventListener('timeupdate', () => {
  updateOverlayTime();
  if (ovState.end > 0 && ovVideo.currentTime >= ovState.end - 0.02) {
    ovVideo.pause();
    if (Math.abs(ovVideo.currentTime - ovState.end) > 0.05) {
      try { ovVideo.currentTime = ovState.end; } catch (_) { /* ignore */ }
    }
  }
});
ovVideo.addEventListener('loadedmetadata', updateOverlayTime);

/* ============ 剪辑面板 ============ */
function bindPanel() {
  clipDrop.addEventListener('click', () => clipFile.click());
  clipFile.addEventListener('change', () => {
    const file = clipFile.files && clipFile.files[0];
    clipFile.value = '';
    if (file) loadClipFile(file);
  });
  clipDrop.addEventListener('dragover', (e) => {
    if (!hasFileDrag(e)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'copy';
    clipDrop.classList.add('drag-over');
  });
  clipDrop.addEventListener('dragleave', () => clipDrop.classList.remove('drag-over'));
  clipDrop.addEventListener('drop', (e) => {
    const file = e.dataTransfer && e.dataTransfer.files ? e.dataTransfer.files[0] : null;
    if (!file) return;
    e.preventDefault();
    e.stopPropagation();
    clipDrop.classList.remove('drag-over');
    loadClipFile(file);
  });

  clipVideoWrap.addEventListener('click', toggleClipPlay);
  clipVideo.addEventListener('play', () => {
    clipVideoWrap.classList.add('playing');
    clipPlayHint.style.display = 'none';
  });
  clipVideo.addEventListener('pause', () => {
    clipVideoWrap.classList.remove('playing');
    clipPlayHint.style.display = '';
  });
  clipVideo.addEventListener('timeupdate', updateClipTime);
  clipVideo.addEventListener('loadedmetadata', () => {
    state.clipDuration = clipVideo.duration || state.clipDuration;
    updateClipTime();
    renderTimelineCuts();
    renderSegments();
  });
  clipVideo.addEventListener('durationchange', () => {
    renderTimelineCuts();
  });

  $$('[data-clip-act]').forEach((btn) => {
    btn.addEventListener('click', () => {
      const act = btn.dataset.clipAct;
      if (act === 'mark') markCut();
      else if (act === 'back') stepFrame(-1);
      else if (act === 'fwd') stepFrame(1);
    });
  });

  clearCutsBtn.addEventListener('click', () => {
    if (!state.cuts.length) return;
    state.cuts = [];
    renderTimelineCuts();
    renderSegments();
    saveEdit();
    toast('已清除全部打点');
  });

  changeClipBtn.addEventListener('click', () => {
    // 换片前保存现场：新原片上传成功才清理旧文件，失败/取消时恢复旧剪辑状态
    pendingClipReplace = state.clip
      ? {
          clip: { file: state.clip.file, url: state.clip.url, name: state.clip.name },
          cuts: state.cuts.slice(),
          fps: state.fps,
          duration: state.clipDuration,
          product: state.product,
          script: state.script,
        }
      : null;
    resetClipPanel();
    saveEdit();
  });

  smartCutBtn.addEventListener('click', smartCut);

  /* 时间轴拖动 */
  let seeking = false;
  const seekTo = (e) => {
    if (!state.clip) return;
    const rect = timeline.getBoundingClientRect();
    const ratio = Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width));
    const d = clipVideo.duration || 0;
    clipVideo.currentTime = ratio * d;
    updateClipTime();
  };
  timeline.addEventListener('pointerdown', (e) => {
    seeking = true;
    timeline.setPointerCapture(e.pointerId);
    seekTo(e);
  });
  timeline.addEventListener('pointermove', (e) => {
    if (seeking) seekTo(e);
  });
  timeline.addEventListener('pointerup', (e) => {
    seeking = false;
    try { timeline.releasePointerCapture(e.pointerId); } catch (_) { /* ignore */ }
    saveEdit();
  });
  timeline.addEventListener('pointercancel', () => { seeking = false; });
}

async function loadClipFile(file) {
  if (!isVideoFile(file)) {
    toast('请选择视频文件');
    return;
  }
  // 上传前记下正在用的原片：上传成功后如果换成了新文件，旧文件按引用计数决定是否清理
  const snap = pendingClipReplace && pendingClipReplace.product === state.product && pendingClipReplace.script === state.script
    ? pendingClipReplace
    : null;
  const previous = snap
    ? snap.clip
    : (state.clip && state.clip.file ? { file: state.clip.file, name: state.clip.name || state.clip.file } : null);
  const ctx = snap
    ? { product: snap.product, script: snap.script }
    : { product: state.product, script: state.script };
  clipDrop.classList.add('uploading');
  const cancel = el('button', 'upload-cancel-btn', '取消上传');
  cancel.type = 'button';
  cancel.addEventListener('click', (e) => {
    e.stopPropagation();
    job.cancel();
  });
  clipDrop.appendChild(cancel);
  const finish = () => {
    cancel.remove();
    clipDrop.classList.remove('uploading');
  };
  const job = queueUpload({
    file,
    url: '/api/temp',
    label: '导入原片 · ' + file.name,
    onSuccess: (data) => {
      pendingClipReplace = null;
      resetClipPanel();
      state.clip = {
        file: data.file,
        url: '/media/_temp/' + encodeURIComponent(data.file),
        name: file.name,
      };
      state.cuts = [];
      state.fps = 30;
      state.clipDuration = 0;
      clipVideo.src = state.clip.url;
      clipVideo.load();
      clipName.textContent = file.name;
      clipName.title = file.name;
      clipDrop.classList.add('hidden');
      clipEditor.classList.remove('hidden');
      updateClipTime();
      renderSegments();
      updateSmartCut();
      toast('视频已载入：可用「智能裁切」或按 S 打点');
      probeClip(data.file);
      saveEdit();
      finish();
      if (previous && previous.file !== data.file) releaseOldSource(previous, ctx);
    },
    onFail: (err) => {
      toast('导入失败：' + err.message + '（旧原片保持不变，可在上传队列点「重试」）', 4200);
      restoreClipReplace();
      finish();
    },
    onCancel: () => {
      toast('已取消导入，旧原片保持不变');
      restoreClipReplace();
      finish();
    },
  });
}

// 换片失败 / 取消时，把面板恢复到点击「更换视频」之前的样子。
function restoreClipReplace() {
  const snap = pendingClipReplace;
  pendingClipReplace = null;
  if (!snap || !snap.clip || snap.product !== state.product || snap.script !== state.script) return;
  state.clip = { file: snap.clip.file, url: snap.clip.url, name: snap.clip.name };
  state.cuts = snap.cuts.slice();
  state.fps = snap.fps > 1 ? snap.fps : 30;
  state.clipDuration = Number(snap.duration) || 0;
  clipName.textContent = state.clip.name || state.clip.file;
  clipName.title = state.clip.name || state.clip.file;
  clipDrop.classList.add('hidden');
  clipEditor.classList.remove('hidden');
  clipVideo.src = state.clip.url;
  clipVideo.load();
  renderTimelineCuts();
  renderSegments();
  updateFrameLabels();
  updateSmartCut();
  saveEdit();
}

// 更换原片后清理旧文件：服务端按全库引用计数判断，仍被镜头或其它脚本引用就保留。
async function releaseOldSource(prev, ctx) {
  if (!prev || !prev.file || !ctx || !ctx.product || !ctx.script) return;
  try {
    await flushNow();
  } catch (_) { /* ignore */ }
  try {
    const res = await api('/api/media/release-temp', {
      method: 'POST',
      body: {
        product: ctx.product,
        script: ctx.script,
        file: prev.file,
        name: me && me.name,
        waitMs: 2500,
      },
    });
    if (!res) return;
    if (res.deleted) {
      toast('已清理旧原片，释放 ' + humanSize(res.freed), 3600);
    } else if (Number(res.refs) > 0) {
      toast('旧原片仍被 ' + res.refs + ' 处引用，已保留', 3600);
    }
  } catch (err) {
    toast('旧原片清理失败：' + err.message, 4200);
  }
}

/* 重置剪辑面板（不保存），用于切换脚本 / 更换视频 */
function resetClipPanel() {
  clipVideo.pause();
  clipVideo.removeAttribute('src');
  clipVideo.load();
  state.clip = null;
  state.cuts = [];
  state.clipDuration = 0;
  state.fps = 30;
  clipEditor.classList.add('hidden');
  clipDrop.classList.remove('hidden');
  clipName.textContent = '';
  clipName.title = '';
  segmentsEl.innerHTML = '';
  segCount.textContent = '0';
  tlCuts.innerHTML = '';
  tlProgress.style.width = '0%';
  clipTime.textContent = '00:00.00 / 00:00.00';
  clipFps.textContent = '';
  clipPlayHint.style.display = '';
  updateFrameLabels();
  updateSmartCut();
}

/* 从文案.json 恢复上次的原片、打点与时间轴位置 */
async function restoreEditor(edit, seq) {
  resetClipPanel();
  if (!edit || !edit.source) return;
  const url = '/media/_temp/' + encodeURIComponent(edit.source);
  let exists = false;
  try {
    const res = await fetch(url, { method: 'HEAD' });
    exists = res.ok;
  } catch (_) { /* 视作不存在 */ }
  if (seq !== loadSeq) return;
  if (!exists) {
    toast('上次的原片已被清理，剪辑面板已重置');
    return;
  }
  state.clip = { file: edit.source, url, name: edit.sourceName || edit.source };
  state.cuts = (edit.cuts || []).slice().sort((a, b) => a - b);
  state.fps = Number(edit.fps) > 1 ? Number(edit.fps) : 30;
  state.clipDuration = Number(edit.duration) || 0;
  clipName.textContent = state.clip.name;
  clipName.title = state.clip.name;
  clipDrop.classList.add('hidden');
  clipEditor.classList.remove('hidden');
  clipVideo.src = url;
  clipVideo.load();
  renderTimelineCuts();
  renderSegments();
  updateFrameLabels();
  updateSmartCut();
  const resume = Number(edit.currentTime) || 0;
  const onMeta = () => {
    state.clipDuration = clipVideo.duration || state.clipDuration;
    if (resume > 0) {
      try {
        clipVideo.currentTime = Math.min(resume, Math.max(0, (clipVideo.duration || resume) - 0.05));
      } catch (_) { /* ignore */ }
    }
    updateClipTime();
    renderTimelineCuts();
    renderSegments();
  };
  if (clipVideo.readyState >= 1) onMeta();
  else clipVideo.addEventListener('loadedmetadata', onMeta, { once: true });
  probeClip(edit.source);
}

/* 用 ffprobe 取真实帧率，A / D 按帧步进 */
async function probeClip(file) {
  try {
    const res = await api('/api/probe', { method: 'POST', body: { src: 'temp', file } });
    if (!state.clip || state.clip.file !== file) return;
    if (res.fps > 1) {
      state.fps = res.fps;
      updateFrameLabels();
    }
    if (res.duration > 0) {
      state.clipDuration = res.duration;
      updateClipTime();
    }
    clipFps.textContent = res.fps > 1 ? res.fps.toFixed(2) + ' fps' : '';
    saveEdit();
  } catch (_) {
    /* ffprobe 不可用时退回 30fps 估算 */
    clipFps.textContent = '';
    updateFrameLabels();
  }
}

function updateFrameLabels() {
  const fps = state.fps > 1 ? state.fps : 30;
  $$('[data-clip-act="back"]').forEach((b) => { b.title = `左一帧（A，按 ${fps.toFixed(2)} fps 步进）`; });
  $$('[data-clip-act="fwd"]').forEach((b) => { b.title = `右一帧（D，按 ${fps.toFixed(2)} fps 步进）`; });
}

function toggleClipPlay() {
  if (!state.clip) return;
  if (clipVideo.paused) {
    const p = clipVideo.play();
    if (p && p.catch) p.catch(() => {});
  } else {
    clipVideo.pause();
  }
}

function stepFrame(dir) {
  if (!state.clip) {
    toast('先在右侧拖入一个视频');
    return;
  }
  clipVideo.pause();
  const d = state.clipDuration || clipVideo.duration || 0;
  if (!d) return;
  const fps = state.fps > 1 ? state.fps : 30;
  const cur = clipVideo.currentTime || 0;
  let target = (Math.round(cur * fps) + dir) / fps;
  target = Math.max(0, Math.min(d - 0.001, target));
  clipVideo.currentTime = target;
  updateClipTime();
  saveEdit();
}

function updateClipTime() {
  const t = clipVideo.currentTime || 0;
  const d = clipVideo.duration || 0;
  clipTime.textContent = fmt(t) + ' / ' + fmt(d);
  tlProgress.style.width = d ? Math.min(100, (t / d) * 100) + '%' : '0%';
}

function markCut() {
  if (!state.clip) {
    toast('先在右侧拖入一个视频');
    return;
  }
  const d = clipVideo.duration || 0;
  const t = clipVideo.currentTime || 0;
  if (!d) {
    toast('视频还在加载，请稍后再试');
    return;
  }
  if (t < 0.08 || t > d - 0.08) {
    toast('太靠近首尾，无法打点');
    return;
  }
  if (state.cuts.some((c) => Math.abs(c - t) < 0.05)) {
    toast('这个位置已经打过点');
    return;
  }
  state.cuts.push(t);
  state.cuts.sort((a, b) => a - b);
  renderTimelineCuts();
  renderSegments();
  saveEdit();
  toast('已打点 ' + fmt(t));
}

function renderTimelineCuts() {
  tlCuts.innerHTML = '';
  const d = clipVideo.duration || 0;
  if (!d) return;
  state.cuts.forEach((t) => {
    const mark = el('div', 'tl-cut');
    mark.style.left = (t / d) * 100 + '%';
    tlCuts.appendChild(mark);
  });
}

function segmentList() {
  const d = clipVideo.duration || 0;
  if (!d) return [];
  const points = [0, ...state.cuts.filter((c) => c > 0.05 && c < d - 0.05), d];
  const segs = [];
  for (let i = 0; i < points.length - 1; i++) {
    const start = points[i];
    const end = points[i + 1];
    if (end - start > 0.05) {
      segs.push({ index: segs.length + 1, start, end });
    }
  }
  return segs;
}

function renderSegments() {
  segmentsEl.innerHTML = '';
  const segs = segmentList();
  segCount.textContent = String(segs.length);
  if (!segs.length) {
    segmentsEl.appendChild(el('div', 'seg-empty', '还没有可用片段。\n点「智能裁切」按画面自动识别，或播放视频按 S 打点。'));
    return;
  }
  segs.forEach((seg) => {
    const item = el('div', 'segment');
    item.draggable = true;

    const thumb = el('div', 'seg-thumb');
    const ph = el('span', 'ph', '···');
    thumb.appendChild(ph);
    const mid = (seg.start + seg.end) / 2;
    getThumb(state.clip.url, mid).then((dataUrl) => {
      if (!dataUrl) return;
      thumb.innerHTML = '';
      const img = document.createElement('img');
      img.src = dataUrl;
      img.alt = '';
      thumb.appendChild(img);
    }).catch(() => {});

    const info = el('div', 'seg-info');
    info.appendChild(el('b', null, '片段 ' + seg.index));
    info.appendChild(el('span', null, fmt(seg.start) + ' – ' + fmt(seg.end)));
    const dragTip = el('div', 'seg-drag', '拖到镜头 →');

    item.appendChild(thumb);
    item.appendChild(info);
    item.appendChild(dragTip);

    item.title = '点击预览';
    item.addEventListener('click', () => {
      openOverlay({
        url: state.clip.url,
        start: seg.start,
        end: seg.end,
        label: `片段 ${seg.index} · ${fmt(seg.start)}–${fmt(seg.end)}`,
      });
    });
    item.addEventListener('dragstart', (e) => {
      const payload = {
        src: 'temp',
        file: state.clip.file,
        in: seg.start,
        out: seg.end,
        label: `片段 ${seg.index} · ${fmt(seg.start)}–${fmt(seg.end)}`,
      };
      e.dataTransfer.setData('application/x-clip', JSON.stringify(payload));
      e.dataTransfer.setData('text/plain', payload.label);
      e.dataTransfer.effectAllowed = 'copy';
    });

    segmentsEl.appendChild(item);
  });
}

/* ============ 智能裁切（ffmpeg 画面变化检测） ============ */
async function smartCut() {
  if (!state.clip) {
    toast('先在右侧拖入一个视频');
    return;
  }
  if (!state.toolsReady) {
    openSettings();
    toast('未找到 ffmpeg，请先指定路径');
    return;
  }
  if (state.splitting) return;
  const clipFile = state.clip.file;
  state.splitting = true;
  smartCutBtn.classList.add('running');
  smartCutBtn.disabled = true;
  smartCutBtn.textContent = '分析中…';
  try {
    const threshold = parseFloat(cutSensitivity.value) || 0.3;
    const created = await api('/api/split', {
      method: 'POST',
      body: { src: 'temp', file: clipFile, threshold },
    });
    let job = null;
    for (;;) {
      await new Promise((r) => setTimeout(r, 600));
      job = await api('/api/split/' + encodeURIComponent(created.id));
      if (!state.clip || state.clip.file !== clipFile) throw new Error('原片已更换，已停止分析');
      if (job.state !== 'running') break;
      const pct = Math.max(0, Math.min(100, Math.round((job.progress || 0) * 100)));
      smartCutBtn.textContent = '分析中 ' + pct + '%';
      if (job.message) smartCutBtn.title = job.message;
    }
    if (job.state === 'error') throw new Error(job.error || '画面分析失败');
    const segs = Array.isArray(job.segments) ? job.segments : [];
    state.cuts = segs
      .map((s) => Number(s.start))
      .filter((t) => Number.isFinite(t) && t > 0.05)
      .map((t) => Math.round(t * 1000) / 1000);
    renderTimelineCuts();
    renderSegments();
    saveEdit();
    toast(`智能裁切完成：${segs.length} 个片段`);
  } catch (err) {
    toast(err.message);
    if (/ffmpeg|未找到/i.test(err.message)) openSettings();
  } finally {
    state.splitting = false;
    smartCutBtn.classList.remove('running');
    smartCutBtn.title = '按画面变化自动识别切点';
    updateSmartCut();
  }
}

/* ============ ffmpeg 状态与设置 ============ */
async function loadEnv() {
  const data = await api('/api/env');
  applyEnv(data);
  return data;
}

function applyEnv(data) {
  const ff = (data && data.ffmpeg) || {};
  state.toolsReady = !!(data && data.ready);
  ffStatus.className = 'ff-chip ' + (state.toolsReady ? 'ok' : 'err');
  ffStatus.textContent = state.toolsReady ? 'ffmpeg 就绪' : '未找到 ffmpeg';
  ffStatus.title = state.toolsReady
    ? [ff.path, ff.version].filter(Boolean).join('\n')
    : (ff.hint || '点击设置 ffmpeg 路径');
  updateSmartCut();
}

function updateSmartCut() {
  smartCutBtn.disabled = !state.clip || state.splitting;
  if (!state.splitting) smartCutBtn.textContent = '智能裁切';
}

function bindSettings() {
  ffStatus.addEventListener('click', openSettings);
  settingsCancel.addEventListener('click', closeSettings);
  settingsModal.addEventListener('mousedown', (e) => {
    if (e.target === settingsModal) closeSettings();
  });
  settingsForm.addEventListener('submit', async (e) => {
    e.preventDefault();
    settingsError.classList.add('hidden');
    try {
      const data = await api('/api/env', {
        method: 'POST',
        body: {
          ffmpeg: setFfmpeg.value.trim(),
          ffprobe: setFfprobe.value.trim(),
          draftRoot: setDraftRoot.value.trim(),
          nasPrefix: setNasPrefix.value.trim(),
          winPrefix: setWinPrefix.value.trim(),
        },
      });
      applyEnv(data);
      renderDetectInfo(data);
      const draftReady = !!(setDraftRoot.value.trim() && setNasPrefix.value.trim() && setWinPrefix.value.trim());
      if (data.ready || draftReady) {
        closeSettings();
        toast(data.ready ? '设置已保存，ffmpeg 就绪' : '设置已保存（ffmpeg 仍未就绪）');
      } else {
        settingsError.textContent = (data.ffmpeg && data.ffmpeg.hint) || '仍未找到 ffmpeg，请检查路径';
        settingsError.classList.remove('hidden');
      }
    } catch (err) {
      settingsError.textContent = err.message;
      settingsError.classList.remove('hidden');
    }
  });
}

async function openSettings() {
  let data = null;
  try {
    data = await loadEnv();
  } catch (_) { /* ignore */ }
  setFfmpeg.value = (data && data.settings && data.settings.ffmpeg) || '';
  setFfprobe.value = (data && data.settings && data.settings.ffprobe) || '';
  const draft = (data && data.draft) || {};
  setDraftRoot.value = (data && data.settings && data.settings.draftRoot) || draft.root || '';
  setNasPrefix.value = (data && data.settings && data.settings.nasPrefix) || draft.nas || '';
  setWinPrefix.value = (data && data.settings && data.settings.winPrefix) || draft.win || '';
  settingsError.classList.add('hidden');
  renderDetectInfo(data);
  settingsModal.classList.remove('hidden');
  setTimeout(() => setFfmpeg.focus(), 0);
}

function closeSettings() {
  settingsModal.classList.add('hidden');
}

function renderDetectInfo(data) {
  ffDetectInfo.innerHTML = '';
  if (!data) return;
  [['ffmpeg', data.ffmpeg], ['ffprobe', data.ffprobe]].forEach(([name, info]) => {
    const box = el('div');
    box.appendChild(el('b', null, name + '：'));
    if (info && info.ok) {
      box.appendChild(el('span', 'ok', '可用'));
      if (info.version) box.appendChild(el('div', null, info.version));
      if (info.path) box.appendChild(el('div', null, info.path));
      if (info.source) box.appendChild(el('div', null, '来源：' + info.source));
    } else {
      box.appendChild(el('span', 'err', '未找到'));
      if (info && info.hint) box.appendChild(el('div', null, info.hint));
    }
    ffDetectInfo.appendChild(box);
  });
}

/* ============ 缩略图 ============ */
const thumbVideo = document.createElement('video');
thumbVideo.muted = true;
thumbVideo.playsInline = true;
thumbVideo.preload = 'auto';
const thumbCanvas = document.createElement('canvas');
const thumbCache = new Map();
let thumbChain = Promise.resolve();

function getThumb(url, time) {
  const key = url + '#' + time.toFixed(2);
  if (thumbCache.has(key)) return Promise.resolve(thumbCache.get(key));
  const job = thumbChain.then(async () => {
    if (thumbCache.has(key)) return;
    await ensureThumbSource(url);
    await seekThumb(thumbVideo, time);
    const vw = thumbVideo.videoWidth || 320;
    const vh = thumbVideo.videoHeight || 180;
    const w = 160;
    const h = Math.max(1, Math.round(w * (vh / vw)));
    thumbCanvas.width = w;
    thumbCanvas.height = h;
    const ctx = thumbCanvas.getContext('2d');
    ctx.drawImage(thumbVideo, 0, 0, w, h);
    thumbCache.set(key, thumbCanvas.toDataURL('image/jpeg', 0.62));
  }).catch(() => {});
  thumbChain = job;
  return job.then(() => thumbCache.get(key));
}

function ensureThumbSource(url) {
  const abs = new URL(url, location.href).href;
  if (thumbVideo.src === abs && thumbVideo.readyState >= 2) return Promise.resolve();
  return new Promise((resolve, reject) => {
    const done = () => {
      thumbVideo.onloadeddata = null;
      thumbVideo.onerror = null;
      resolve();
    };
    thumbVideo.onloadeddata = done;
    thumbVideo.onerror = () => {
      thumbVideo.onloadeddata = null;
      thumbVideo.onerror = null;
      reject(new Error('thumb load failed'));
    };
    thumbVideo.src = abs;
    thumbVideo.load();
  });
}

function seekThumb(video, t) {
  return new Promise((resolve, reject) => {
    if (Math.abs((video.currentTime || 0) - t) < 0.01) {
      resolve();
      return;
    }
    const timer = setTimeout(() => {
      video.removeEventListener('seeked', onSeek);
      reject(new Error('seek timeout'));
    }, 4000);
    const onSeek = () => {
      clearTimeout(timer);
      video.removeEventListener('seeked', onSeek);
      resolve();
    };
    video.addEventListener('seeked', onSeek);
    try {
      video.currentTime = t;
    } catch (err) {
      clearTimeout(timer);
      video.removeEventListener('seeked', onSeek);
      reject(err);
    }
  });
}

/* ============ 键盘 ============ */
document.addEventListener('keydown', (e) => {
  if (e.isComposing) return;

  if (!overlay.classList.contains('hidden')) {
    if (e.code === 'Space') {
      e.preventDefault();
      toggleOverlayPlay();
    } else if (e.key === 'Escape') {
      e.preventDefault();
      closeOverlay();
    }
    return;
  }

  if (!settingsModal.classList.contains('hidden')) {
    if (e.key === 'Escape') {
      e.preventDefault();
      closeSettings();
    }
    return;
  }

  if (!modal.classList.contains('hidden')) {
    if (e.key === 'Escape') {
      e.preventDefault();
      closeModal();
    }
    return;
  }

  /* Ctrl / Cmd + K：聚焦左侧搜索框（在输入框里也生效） */
  if ((e.ctrlKey || e.metaKey) && (e.key === 'k' || e.key === 'K')) {
    e.preventDefault();
    if (sidebarEl.classList.contains('collapsed')) setSidebar(false, true);
    scriptSearch.focus();
    scriptSearch.select();
    return;
  }

  if (e.key === 'Escape') {
    if (!themePanel.classList.contains('hidden')) {
      e.preventDefault();
      closeThemePanel();
      return;
    }
    // 只有焦点在剪辑面板里时 Esc 才收起，避免在表格里按 Esc 把面板关掉
    if (!clipPanel.classList.contains('collapsed') && clipPanel.contains(document.activeElement)) {
      e.preventDefault();
      setClipPanel(false, true);
      return;
    }
  }

  if (isTypingTarget(e.target)) return;
  if (e.ctrlKey || e.metaKey || e.altKey) return;

  if (e.key === 'f' || e.key === 'F') {
    e.preventDefault();
    setClipPanel(clipPanel.classList.contains('collapsed'), true);
  } else if (e.code === 'Space') {
    if (!state.clip) return;
    e.preventDefault();
    toggleClipPlay();
  } else if (e.key === 's' || e.key === 'S') {
    if (!state.clip) return;
    e.preventDefault();
    markCut();
  } else if (e.key === 'a' || e.key === 'A') {
    if (!state.clip) return;
    e.preventDefault();
    stepFrame(-1);
  } else if (e.key === 'd' || e.key === 'D') {
    if (!state.clip) return;
    e.preventDefault();
    stepFrame(1);
  } else if (e.key === 'Escape') {
    if (state.clip) clipVideo.pause();
  }
});

/* ============ 列宽拖拽与首列冻结 ============ */
const COLS_KEY = 'sm.cols';
const COL_DEFS = [
  { key: 'pick', min: 30, max: 64 },
  { key: 'no', min: 72, max: 220 },
  { key: 'content', min: 120, max: 900 },
  { key: 'narration', min: 120, max: 900 },
  { key: 'copy', min: 120, max: 900 },
  { key: 'video', min: 180, max: 900 },
];
let colWidths = {};

function readColWidths() {
  try {
    const raw = JSON.parse(localStorage.getItem(COLS_KEY) || '{}');
    const out = {};
    COL_DEFS.forEach((c) => {
      const n = Number(raw[c.key]);
      if (Number.isFinite(n) && n >= c.min && n <= c.max) out[c.key] = Math.round(n);
    });
    return out;
  } catch (_) {
    return {};
  }
}

function saveColWidths() {
  try {
    localStorage.setItem(COLS_KEY, JSON.stringify(colWidths));
  } catch (_) { /* ignore */ }
}

function applyColWidths() {
  if (!tableEl) return;
  COL_DEFS.forEach((c) => {
    const w = colWidths[c.key];
    if (Number.isFinite(w)) tableEl.style.setProperty('--col-' + c.key, w + 'px');
    else tableEl.style.removeProperty('--col-' + c.key);
  });
}

function bindColumnResize() {
  if (!tableEl) return;
  colWidths = readColWidths();
  applyColWidths();
  const ths = $$('.thead > .th', tableEl);
  ths.forEach((th, i) => {
    if (i >= COL_DEFS.length - 1) return; // 最后一列不留把手
    const def = COL_DEFS[i];
    const handle = el('span', 'col-resizer');
    handle.title = '拖动调整列宽，双击恢复默认';
    th.appendChild(handle);
    handle.addEventListener('pointerdown', (e) => {
      e.preventDefault();
      e.stopPropagation();
      const startX = e.clientX;
      const startW = th.getBoundingClientRect().width;
      document.body.classList.add('col-resizing');
      const move = (ev) => {
        const w = Math.max(def.min, Math.min(def.max, startW + ev.clientX - startX));
        colWidths[def.key] = Math.round(w);
        tableEl.style.setProperty('--col-' + def.key, colWidths[def.key] + 'px');
      };
      const up = () => {
        document.removeEventListener('pointermove', move);
        document.removeEventListener('pointerup', up);
        document.body.classList.remove('col-resizing');
        saveColWidths();
      };
      document.addEventListener('pointermove', move);
      document.addEventListener('pointerup', up);
    });
    handle.addEventListener('dblclick', (e) => {
      e.preventDefault();
      delete colWidths[def.key];
      tableEl.style.removeProperty('--col-' + def.key);
      saveColWidths();
    });
  });
}

/* ============ 修改历史 ============ */
function bindHistory() {
  if (!historyBtn) return;
  historyBtn.addEventListener('click', openHistory);
  historyClose.addEventListener('click', closeHistory);
  historyModal.addEventListener('mousedown', (e) => {
    if (e.target === historyModal) closeHistory();
  });
}

function closeHistory() {
  historyModal.classList.add('hidden');
}

async function openHistory() {
  if (!state.product || !state.script) return;
  historyModal.classList.remove('hidden');
  historySub.textContent = state.product + ' / ' + state.script;
  historyList.innerHTML = '';
  historyList.appendChild(el('div', 'history-empty', '加载中…'));
  try {
    const qs = 'product=' + encodeURIComponent(state.product) + '&script=' + encodeURIComponent(state.script);
    const data = await api('/api/history?' + qs);
    renderHistory(data.items || []);
  } catch (err) {
    historyList.innerHTML = '';
    historyList.appendChild(el('div', 'history-empty', '加载失败：' + err.message));
  }
}

function fmtHistoryTime(t) {
  const d = new Date(t);
  if (!Number.isFinite(d.getTime())) return '';
  const p = (n) => String(n).padStart(2, '0');
  return (d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
}

function renderHistory(items) {
  historyList.innerHTML = '';
  if (!items.length) {
    historyList.appendChild(el('div', 'history-empty', '还没有修改记录。协作编辑与「使用 / 删除」都会自动记录。'));
    return;
  }
  const kindLabel = { add: '新增', del: '删除', text: '文案', no: '序号', video: '素材', status: '状态', media: '清理', rollback: '回滚' };
  items.forEach((it) => {
    const row = el('div', 'history-row');
    const top = el('div', 'history-row-top');
    top.appendChild(el('span', 'history-time', fmtHistoryTime(it.t)));
    top.appendChild(el('span', 'history-by', it.by || '同事'));
    top.appendChild(el('span', 'history-kind', kindLabel[it.kind] || '修改'));
    row.appendChild(top);
    row.appendChild(el('div', 'history-text', it.text || ''));
    if (it.prev || it.next) {
      const diff = el('div', 'history-diff');
      diff.appendChild(el('span', 'history-prev', it.prev || '（空）'));
      diff.appendChild(el('span', 'history-arrow', '→'));
      diff.appendChild(el('span', 'history-next', it.next || '（空）'));
      row.appendChild(diff);
    }
    if (it.kind !== 'status' && it.kind !== 'rollback' && it.kind !== 'media') {
      const btn = el('button', 'mini-btn history-rollback', '回滚到此前');
      btn.type = 'button';
      btn.addEventListener('click', () => askRollback(it));
      row.appendChild(btn);
    }
    historyList.appendChild(row);
  });
}

function askRollback(entry) {
  openConfirm({
    title: '回滚到这条记录之前？',
    desc: '这条记录以及其后的改动都会被撤回，在线同事会同步看到结果。',
    confirmText: '回滚',
    onConfirm: async () => {
      try {
        const data = await api('/api/history/rollback', {
          method: 'POST',
          body: { product: state.product, script: state.script, id: entry.id, name: me && me.name },
        });
        toast('已回滚，恢复 ' + (data.applied || 0) + ' 个字段');
        await openHistory();
      } catch (err) {
        toast('回滚失败：' + err.message, 3600);
      }
    },
  });
}

/* ============ 导出（剪映草稿 / CSV 兜底） ============ */
let exportBusy = false;
let exportLastCheck = null;
let exportTimer = null;

function bindExport() {
  // 逐个绑定，任何一个元素缺失都不影响其他入口；避免整段静默 return
  if (exportModal) {
    exportModal.addEventListener('mousedown', (e) => {
      if (e.target === exportModal) closeExport();
    });
  }
  if (exportBtn) exportBtn.addEventListener('click', openExport);
  if (exportClose) exportClose.addEventListener('click', closeExport);
  if (exportRecheck) exportRecheck.addEventListener('click', runExportPrecheck);
  if (exportDraftBtn) exportDraftBtn.addEventListener('click', exportDraft);
  if (exportCsvBtn) exportCsvBtn.addEventListener('click', exportFallback);
  if (exportCopy) exportCopy.addEventListener('click', copyExportPaths);
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && exportModal && !exportModal.classList.contains('hidden')) closeExport();
  });
}

function openExport() {
  exportTarget.textContent = state.product && state.script
    ? '当前脚本：' + state.product + ' / ' + state.script
    : '还没有选中脚本：请先在左侧点开一个脚本，再回来导出';
  exportResult.classList.add('hidden');
  exportPathBar.classList.add('hidden');
  exportModal.classList.remove('hidden');
  runExportPrecheck();
}

function closeExport() {
  if (exportBusy) {
    toast('导出还在进行，请等它结束；成功或失败都会有提示', 4200);
    return;
  }
  exportModal.classList.add('hidden');
}

function showExportResult(text, kind) {
  exportResult.textContent = text;
  exportResult.className = 'export-result ' + kind;
  exportResult.classList.remove('hidden');
}

function precheckBlocking(data) {
  return ((data && data.items) || []).filter((it) => !it.ok && it.blocking);
}

function renderPrecheck(data) {
  exportPrecheck.innerHTML = '';
  const items = (data && data.items) || [];
  if (!items.length) {
    exportPrecheck.textContent = '预检没有返回任何结果，请重试或查看容器日志';
    return;
  }
  items.forEach((it) => {
    const cls = it.ok ? 'ok' : (it.blocking ? 'err' : 'warn');
    const icon = it.ok ? '✓' : (it.blocking ? '✕' : '!');
    const row = el('div', 'export-pc-item ' + cls);
    row.appendChild(el('span', 'export-pc-icon', icon));
    const body = el('div', 'export-pc-body');
    body.appendChild(el('div', 'export-pc-label', it.label || it.key || '检查项'));
    if (it.detail) body.appendChild(el('div', 'export-pc-detail', it.detail));
    if (!it.ok && it.hint) body.appendChild(el('div', 'export-pc-hint', '怎么补：' + it.hint));
    row.appendChild(body);
    exportPrecheck.appendChild(row);
  });
  if (data.summary) exportPrecheck.appendChild(el('div', 'export-pc-summary', data.summary));
}

function renderTemplatePicker(data) {
  if (!exportTemplateRow || !exportTemplate) return;
  const list = (data && data.templates) || [];
  const usable = list.filter((t) => t.usable);
  const prev = exportTemplate.value;
  exportTemplate.innerHTML = '';
  if (!usable.length) {
    exportTemplateRow.classList.add('hidden');
    exportTemplateNote.classList.add('hidden');
    return;
  }
  usable.forEach((t) => {
    const opt = document.createElement('option');
    opt.value = t.name;
    opt.textContent = t.name + (t.hasVideo ? '（含视频）' : '（无视频）');
    exportTemplate.appendChild(opt);
  });
  if (prev && usable.some((t) => t.name === prev)) {
    exportTemplate.value = prev;
  } else if (data.defaultTemplate) {
    exportTemplate.value = data.defaultTemplate;
  }
  exportTemplateRow.classList.remove('hidden');
  const bad = list.filter((t) => !t.usable);
  if (bad.length) {
    exportTemplateNote.textContent = '以下草稿不能当模板（已跳过）：\n'
      + bad.map((t) => '· ' + t.name + '：' + (t.reason || '原因未知')).join('\n')
      + '\n新版剪映的模板可能已加密；请用旧版剪映（如 5.x）另存一份，或改用下面的 CSV 兜底。';
    exportTemplateNote.classList.remove('hidden');
  } else {
    exportTemplateNote.classList.add('hidden');
  }
}

async function runExportPrecheck() {
  exportLastCheck = null;
  exportPrecheck.textContent = '正在预检：草稿目录 / 模板草稿 / 路径映射 / 当前脚本…';
  if (exportTemplateRow) exportTemplateRow.classList.add('hidden');
  if (exportTemplateNote) exportTemplateNote.classList.add('hidden');
  exportDraftBtn.disabled = true;
  exportCsvBtn.disabled = true;
  if (!state.product || !state.script) {
    exportPrecheck.textContent = '请先在左侧点开一个脚本，再导出。';
    exportDraftBtn.disabled = false;
    exportCsvBtn.disabled = false;
    return;
  }
  const q = '?product=' + encodeURIComponent(state.product) + '&script=' + encodeURIComponent(state.script);
  try {
    const data = await api('/api/export/precheck' + q);
    exportLastCheck = data;
    renderPrecheck(data);
    renderTemplatePicker(data);
    if (!data.ok) {
      const bad = precheckBlocking(data);
      toast('预检发现 ' + bad.length + ' 个必须解决的问题，按提示补齐后再导出', 5200);
    }
  } catch (err) {
    exportPrecheck.textContent = '预检失败：' + err.message;
    toast('预检失败：' + err.message, 5200);
  } finally {
    exportDraftBtn.disabled = !!exportBusy;
    exportCsvBtn.disabled = !!exportBusy;
  }
}

async function exportDraft() {
  if (exportBusy) return;
  if (!state.product || !state.script) {
    showExportResult('请先在左侧点开一个脚本，再导出', 'err');
    toast('导出没有开始：还没有选中脚本', 4200);
    return;
  }
  const blocking = precheckBlocking(exportLastCheck);
  if (blocking.length) {
    showExportResult('导出没有开始：预检还有 ' + blocking.length + ' 个问题\n'
      + blocking.map((it) => '· ' + (it.label || it.key)).join('\n')
      + '\n\n上面每一条都写了缺什么、去哪里补；补齐后点「重新预检」。', 'err');
    toast('导出没有开始：' + (blocking[0].label || '预检未通过') + ' 等 ' + blocking.length + ' 个问题未解决', 5600);
    return;
  }

  exportBusy = true;
  exportDraftBtn.disabled = true;
  exportCsvBtn.disabled = true;
  exportPathBar.classList.add('hidden');
  const started = Date.now();
  const tick = () => {
    const sec = Math.floor((Date.now() - started) / 1000);
    showExportResult('正在导出，已用时 ' + sec + ' 秒…\n步骤：读取脚本 → 找模板草稿 → 复制素材 → 写时间轴 → 生成目录', 'ok');
  };
  tick();
  clearInterval(exportTimer);
  exportTimer = setInterval(tick, 1000);

  try {
    const data = await api('/api/export/draft', {
      method: 'POST',
      body: {
        product: state.product,
        script: state.script,
        template: exportTemplate ? exportTemplate.value : '',
      },
    });
    const name = data.draft || '（服务端没有返回草稿名）';
    const lines = [
      '✓ 草稿已生成：' + name,
      '视频 ' + (data.clips || 0) + ' 段 · 文案 ' + (data.texts || 0) + ' 段',
    ];
    if (data.warnings && data.warnings.length) {
      lines.push('注意：' + data.warnings.slice(0, 5).join('；'));
    }
    lines.push('在剪映草稿页刷新即可看到；若没出现，完全退出剪映再打开（它启动时才读取索引）。');
    showExportResult(lines.join('\n'), 'ok');
    exportNasPath.textContent = data.nasPath || '';
    exportWinPath.textContent = data.winPath || '';
    exportPathBar.classList.remove('hidden');
    toast('草稿已生成：' + name + '（' + (data.nasPath || '') + '）', 6500);
  } catch (err) {
    const stage = err.stage ? ('步骤：' + err.stage) : (err.network ? '请求没有发出去' : '步骤未知');
    const tail = err.stage === '模板'
      ? '\n\n可改用下面的「下载 CSV + 时间码文案」兜底，或在上面换一份能解析的模板后再试。'
      : '\n\n目录或权限问题：点右上角 ffmpeg 状态按钮，在「设置 → 剪映草稿导出」里检查草稿根目录 / NAS 前缀 / Windows 前缀，'
        + '并确认 compose 对草稿目录是读写挂载。';
    showExportResult('✕ 导出失败（' + stage + '）\n' + err.message + tail, 'err');
    toast('导出失败（' + stage + '）：' + err.message, 7000);
  } finally {
    clearInterval(exportTimer);
    exportTimer = null;
    exportBusy = false;
    exportDraftBtn.disabled = false;
    exportCsvBtn.disabled = false;
  }
}

async function exportFallback() {
  if (!state.product || !state.script) {
    showExportResult('请先在左侧点开一个脚本，再导出', 'err');
    toast('下载没有开始：还没有选中脚本', 4200);
    return;
  }
  const name = state.product + '-' + state.script + '.zip';
  showExportResult('正在打包 CSV / 时间码文案…', 'ok');
  try {
    const url = '/api/export/fallback?product=' + encodeURIComponent(state.product)
      + '&script=' + encodeURIComponent(state.script);
    const res = await fetch(url);
    if (!res.ok) {
      let data = null;
      try { data = await res.json(); } catch (_) { /* 非 JSON 错误页 */ }
      const stage = data && data.stage ? '（' + data.stage + '）' : '';
      throw new Error('HTTP ' + res.status + stage + '：' + ((data && data.error) || '服务端拒绝了这次下载'));
    }
    const blob = await res.blob();
    const href = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = href;
    a.download = name;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(href), 4000);
    showExportResult('✓ 已开始下载：' + name + '\n包含 文案-时间码.txt / 镜头文案.csv / 素材清单.csv / 导入说明.txt', 'ok');
    toast('已开始下载：' + name, 5000);
  } catch (err) {
    showExportResult('✕ 下载失败：' + err.message, 'err');
    toast('下载失败：' + err.message, 6500);
  }
}

async function copyExportPaths() {
  const text = [exportNasPath.textContent, exportWinPath.textContent].filter(Boolean).join('\n');
  if (!text) {
    toast('还没有可复制的路径', 3600);
    return;
  }
  try {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(text);
    } else {
      const ta = document.createElement('textarea');
      ta.value = text;
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      document.execCommand('copy');
      ta.remove();
    }
    toast('路径已复制', 3000);
  } catch (_) {
    toast('复制失败，请手动选中路径复制', 4200);
  }
}

/* ============ 全局事件 ============ */
function bindGlobal() {
  addProductBtn.addEventListener('click', createProduct);
  if (delProductBtn) delProductBtn.addEventListener('click', askDeleteProduct);
  addScriptBtn.addEventListener('click', createScript);
  productSelect.addEventListener('change', () => selectProduct(productSelect.value));

  renumberBtn.addEventListener('click', renumberShots);
  batchDeleteBtn.addEventListener('click', batchDeleteShots);
  batchSliceBtn.addEventListener('click', batchSliceShots);
  batchClearBtn.addEventListener('click', () => {
    selection.clear();
    applySelectionToRows();
    renderBatchBar();
    syncSelectAll();
  });
  selectAll.addEventListener('change', () => {
    if (!state.doc) return;
    selection.clear();
    if (selectAll.checked) state.doc.shots.forEach((s) => selection.add(s.id));
    applySelectionToRows();
    renderBatchBar();
    syncSelectAll();
  });

  addRowBtn.addEventListener('click', () => {
    if (!state.doc || !state.regs) return;
    const nums = state.doc.shots
      .map((s) => parseInt(s.no, 10))
      .filter((n) => Number.isFinite(n));
    const no = String(nums.length ? Math.max(...nums) + 1 : state.doc.shots.length + 1);
    // 行 ID 在本地生成，重连补发也不会出现重复行
    const made = Collab.createShot(state.regs, me.site, no);
    pushRegs(made.regs);
    state.doc = normalizeDoc(Collab.project(state.regs, state.product, state.script), state.product, state.script);
    renderTable();
    const last = rowsEl.lastElementChild;
    if (last) {
      last.scrollIntoView({ block: 'nearest' });
      const ta = last.querySelector('.cell-input') || last.querySelector('textarea');
      if (ta) ta.focus();
    }
  });

  /* 防止把文件拖到页面其它地方时浏览器直接打开文件 */
  window.addEventListener('dragover', (e) => {
    if (hasFileDrag(e) || hasClipDrag(e)) e.preventDefault();
  });
  window.addEventListener('drop', (e) => {
    if (e.defaultPrevented) return;
    if (e.target.closest('.video-cell, .clip-drop, .segment')) return;
    if (hasFileDrag(e)) e.preventDefault();
  });
}

/* ============ 清理未引用的视频 ============ */
const orphanUi = { files: [], selected: new Set(), busy: false };

function bindOrphans() {
  const mOrphans = $('#mOrphans');
  if (mOrphans) {
    mOrphans.addEventListener('click', () => {
      openMobileMenu(false);
      openOrphans();
    });
  }
  if (orphanOpen) orphanOpen.addEventListener('click', openOrphans);
  if (orphanClose) orphanClose.addEventListener('click', closeOrphans);
  if (orphanRescan) orphanRescan.addEventListener('click', scanOrphans);
  if (orphanDelete) orphanDelete.addEventListener('click', confirmDeleteOrphans);
  if (orphanAll) {
    orphanAll.addEventListener('change', () => {
      orphanUi.selected.clear();
      if (orphanAll.checked) orphanUi.files.forEach((f) => orphanUi.selected.add(f.path));
      renderOrphanSelection();
    });
  }
  if (orphanModal) {
    orphanModal.addEventListener('mousedown', (e) => {
      if (e.target === orphanModal) closeOrphans();
    });
  }
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && orphanModal && !orphanModal.classList.contains('hidden')) closeOrphans();
  });
}

function openOrphans() {
  if (!orphanModal) return;
  orphanModal.classList.remove('hidden');
  scanOrphans();
}

function closeOrphans() {
  if (!orphanModal) return;
  if (orphanUi.busy) {
    toast('扫描或删除还在进行，请稍等', 2600);
    return;
  }
  orphanModal.classList.add('hidden');
}

function humanSize(n) {
  n = Number(n) || 0;
  if (n < 1024) return n + ' B';
  const units = ['KB', 'MB', 'GB', 'TB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return v.toFixed(1) + ' ' + units[i];
}

function showOrphanNote(text, kind) {
  if (!orphanNote) return;
  orphanNote.textContent = text;
  orphanNote.className = 'orphan-note' + (kind ? ' ' + kind : '');
  orphanNote.classList.remove('hidden');
}

async function scanOrphans() {
  if (!orphanModal || orphanUi.busy) return;
  orphanUi.busy = true;
  orphanUi.files = [];
  orphanUi.selected.clear();
  orphanList.innerHTML = '';
  if (orphanAll) orphanAll.checked = false;
  if (orphanNote) orphanNote.classList.add('hidden');
  orphanSummary.textContent = '正在扫描数据目录…';
  renderOrphanSelection();
  try {
    const data = await api('/api/media/orphans');
    orphanUi.files = Array.isArray(data.files) ? data.files : [];
    renderOrphanRows(data);
  } catch (err) {
    orphanSummary.textContent = '扫描失败：' + err.message;
    showOrphanNote('扫描失败：' + err.message, 'err');
  } finally {
    orphanUi.busy = false;
    renderOrphanSelection();
  }
}

function renderOrphanRows(data) {
  orphanList.innerHTML = '';
  const files = orphanUi.files;
  const total = files.reduce((sum, f) => sum + (Number(f.size) || 0), 0);
  if (!files.length) {
    orphanSummary.textContent = '没有发现未被引用的视频。';
    orphanList.appendChild(el('div', 'orphan-empty', '数据目录很干净，暂时不需要清理。'));
  } else {
    orphanSummary.textContent = `发现 ${files.length} 个未被引用的视频，合计 ${humanSize(total)}。`;
  }
  const recent = (data && data.recentCount) || 0;
  const minAge = (data && data.minAgeMinutes) || 10;
  if (recent > 0) {
    orphanSummary.textContent += ` 另有 ${recent} 个最近 ${minAge} 分钟内上传的文件暂不清理。`;
  }
  const residue = (data && data.residueCount) || 0;
  if (residue > 0) {
    orphanSummary.textContent += ` 其中 ${residue} 个是上传 / 写入中断留下的临时残留文件。`;
  }
  files.forEach((f) => {
    const row = el('label', 'orphan-row');
    const box = document.createElement('input');
    box.type = 'checkbox';
    box.checked = orphanUi.selected.has(f.path);
    box.addEventListener('change', () => {
      if (box.checked) orphanUi.selected.add(f.path);
      else orphanUi.selected.delete(f.path);
      renderOrphanSelection();
    });
    row.appendChild(box);
    const main = el('span', 'orphan-main');
    main.appendChild(el('span', 'orphan-name', f.name || f.path));
    const where = f.scope === 'residue'
      ? '临时残留（上传 / 写入中断留下）'
      : f.scope === 'temp'
        ? '剪辑素材（_剪辑素材）'
        : (f.product || '') + ' / ' + (f.script || '');
    main.appendChild(el('span', 'orphan-sub', f.path + ' · ' + where));
    row.appendChild(main);
    row.appendChild(el('span', 'orphan-size', humanSize(f.size)));
    orphanList.appendChild(row);
  });
  renderOrphanSelection();
  if (data && data.recent && data.recent.length) {
    const last = data.recent[0];
    showOrphanNote('最近一次清理：' + (last.at || '') + ' · ' + (last.by || '同事') + ' · ' + (last.text || ''), '');
  }
}

function renderOrphanSelection() {
  if (!orphanSelected) return;
  const n = orphanUi.selected.size;
  const size = orphanUi.files
    .filter((f) => orphanUi.selected.has(f.path))
    .reduce((sum, f) => sum + (Number(f.size) || 0), 0);
  orphanSelected.textContent = n ? `已选 ${n} 个 · ${humanSize(size)}` : '未选择文件';
  if (orphanAll) {
    orphanAll.checked = orphanUi.files.length > 0 && n === orphanUi.files.length;
    orphanAll.indeterminate = n > 0 && n < orphanUi.files.length;
  }
  if (orphanDelete) orphanDelete.disabled = orphanUi.busy || n === 0;
}

function confirmDeleteOrphans() {
  const paths = Array.from(orphanUi.selected);
  if (!paths.length) return;
  const size = orphanUi.files
    .filter((f) => orphanUi.selected.has(f.path))
    .reduce((sum, f) => sum + (Number(f.size) || 0), 0);
  openConfirm({
    title: `删除选中的 ${paths.length} 个视频？`,
    desc: `将释放约 ${humanSize(size)}。删除后无法恢复；回滚到旧版本也不会恢复已删除的视频文件。剪映草稿目录不会被删除。`,
    confirmText: '确认删除',
    onConfirm: () => deleteOrphans(paths),
  });
}

async function deleteOrphans(paths) {
  orphanUi.busy = true;
  renderOrphanSelection();
  let message = '';
  try {
    const data = await api('/api/media/orphans', {
      method: 'POST',
      body: { paths, confirm: true, name: me && me.name },
    });
    message = data.message || '已清理';
    const failed = data.failed || [];
    const skipped = data.skipped || [];
    if (failed.length) message += `；${failed.length} 个删除失败（可能被占用或没有权限），已记录日志`;
    if (skipped.length) message += `；${skipped.length} 个已跳过`;
    toast(message, 4200);
    orphanUi.busy = false;
    await scanOrphans();
    let detail = message;
    if (failed.length) detail += '\n' + failed.map((f) => '· ' + f.path + '：' + f.error).join('\n');
    if (skipped.length) detail += '\n' + skipped.map((f) => '· ' + f.path + '：' + f.error).join('\n');
    showOrphanNote(detail, failed.length ? 'err' : '');
  } catch (err) {
    toast('清理失败：' + err.message, 3600);
    showOrphanNote('清理失败：' + err.message, 'err');
  } finally {
    orphanUi.busy = false;
    renderOrphanSelection();
  }
}

/* ============ Toast ============ */
let toastTimer = 0;

function showToast(msg, ms = 2200) {
  toastEl.textContent = msg;
  toastEl.classList.remove('hidden');
  clearTimeout(toastTimer);
  if (ms > 0) {
    toastTimer = setTimeout(hideToast, ms);
  }
}

function hideToast() {
  clearTimeout(toastTimer);
  toastEl.classList.add('hidden');
}

function toast(msg, ms = 2200) {
  showToast(msg, ms);
}

/* 放在文件末尾启动，确保所有常量都已初始化 */
init();
