'use strict';
/* 阶段三：多人协作编辑的最小 CRDT + 连接层。
 *
 * 文档拆成一堆「字段寄存器」，每个寄存器带 (t, s)：t=混合逻辑时钟(毫秒)，s=编辑者标识。
 * 合并规则：t 大者胜；t 相同则 s 大者胜；再相同比值的 JSON。合并幂等，所以重复/乱序到达都安全。
 * 键格式：s|<镜头ID>|content / s|<镜头ID>|order / s|<镜头ID>|dead / e|cuts
 *
 * 纯逻辑部分不碰 DOM，可直接在 Node 里跑测试；连接部分在浏览器里用 WebSocket，
 * 被代理拦住时自动退回 3 秒一次 HTTP 轮询。
 */
(function (root, factory) {
  var api = factory();
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  if (typeof window !== 'undefined') window.Collab = api;
})(typeof globalThis !== 'undefined' ? globalThis : this, function () {
  var SEP = '|';
  var COLORS = ['#4c8dff', '#f2994a', '#27ae60', '#9b51e0', '#eb5757', '#2d9cdb', '#f2c94c', '#00b8a9'];

  function colorFor(site) {
    var h = 0;
    for (var i = 0; i < site.length; i++) h = (h * 31 + site.charCodeAt(i)) % 100000;
    return COLORS[h % COLORS.length];
  }

  /* ---------------- 身份 ---------------- */

  function identity() {
    var site = '';
    try { site = sessionStorage.getItem('sm.collab.site') || ''; } catch (_) {}
    if (!site) {
      site = Math.random().toString(36).slice(2, 10);
      try { sessionStorage.setItem('sm.collab.site', site); } catch (_) {}
    }
    var name = '';
    try { name = localStorage.getItem('sm.collab.name') || ''; } catch (_) {}
    if (!name) name = '同事-' + site.slice(0, 4);
    return { site: site, name: name, color: colorFor(site) };
  }

  function setName(name) {
    try { localStorage.setItem('sm.collab.name', name); } catch (_) {}
  }

  /* ---------------- CRDT 状态 ---------------- */

  function createState(epoch) {
    return { epoch: epoch || '', regs: {}, seen: 0, offset: 0 };
  }

  // observeNow 用服务器时间校准本地时钟，避免本机时间偏太多导致自己的改动总是输
  function observeNow(st, now) {
    if (typeof now === 'number' && now > 0) {
      var delta = now - Date.now();
      if (Math.abs(delta) > 1500) st.offset = delta;
      if (now > st.seen) st.seen = now;
    }
  }

  function newer(a, b) {
    if (a.t !== b.t) return a.t > b.t;
    if (a.s !== b.s) return a.s > b.s;
    return JSON.stringify(a.v) > JSON.stringify(b.v);
  }

  // mergeRegs 把远端寄存器并进本地，返回真正变化的那些（用于局部刷新界面）
  function mergeRegs(st, incoming) {
    var changed = {};
    if (!incoming) return changed;
    for (var key in incoming) {
      if (!Object.prototype.hasOwnProperty.call(incoming, key)) continue;
      var reg = incoming[key];
      if (!reg || typeof reg.t !== 'number' || !reg.s) continue;
      if (reg.t > st.seen) st.seen = reg.t;
      var old = st.regs[key];
      if (old && !newer(reg, old)) continue;
      st.regs[key] = { t: reg.t, s: reg.s, v: reg.v === undefined ? null : reg.v };
      changed[key] = st.regs[key];
    }
    return changed;
  }

  function stamp(st) {
    var now = Date.now() + (st.offset || 0);
    st.seen = Math.max(now, st.seen + 1);
    return st.seen;
  }

  function setReg(st, key, value, site) {
    var reg = { t: stamp(st), s: site, v: value === undefined ? null : value };
    st.regs[key] = reg;
    return reg;
  }

  function shotKey(id, field) { return 's' + SEP + id + SEP + field; }
  function editKey(field) { return 'e' + SEP + field; }

  function newId() {
    if (typeof crypto !== 'undefined' && crypto.randomUUID) return crypto.randomUUID();
    return 'r' + Date.now().toString(36) + Math.random().toString(36).slice(2, 8);
  }

  // createShot 新建一行：order 决定行序，id 是合并时去重的依据
  function createShot(st, site, no) {
    var id = newId();
    var base = stamp(st);
    var regs = {};
    var put = function (field, value, t) {
      var reg = { t: t, s: site, v: value };
      st.regs[shotKey(id, field)] = reg;
      regs[shotKey(id, field)] = reg;
    };
    put('order', base, base);
    put('no', String(no == null ? '' : no), base + 1);
    return { id: id, no: String(no == null ? '' : no), regs: regs };
  }

  function deleteShot(st, site, id) {
    var reg = setReg(st, shotKey(id, 'dead'), true, site);
    var out = {};
    out[shotKey(id, 'dead')] = reg;
    return out;
  }

  function project(st, product, script) {
    var regs = st.regs || {};
    var rows = {};
    var order = [];
    var rowOf = function (id) {
      if (!rows[id]) {
        rows[id] = { id: id, order: 0, hasOrder: false, dead: false, no: '', content: '', narration: '', copy: '', video: null };
        order.push(id);
      }
      return rows[id];
    };
    for (var key in regs) {
      if (!Object.prototype.hasOwnProperty.call(regs, key)) continue;
      var parts = key.split(SEP);
      if (parts.length < 3 || parts[0] !== 's') continue;
      var v = regs[key].v;
      var row = rowOf(parts[1]);
      switch (parts[2]) {
        case 'dead':
          if (v === true) row.dead = true;
          break;
        case 'order':
          row.order = Number(v) || 0;
          row.hasOrder = true;
          break;
        case 'video':
          if (v && v.file) {
            row.video = {
              src: v.src === 'temp' ? 'temp' : 'script',
              file: String(v.file),
              in: Number(v.in) || 0,
              out: Number(v.out) || 0,
              label: v.label ? String(v.label) : '',
            };
          } else {
            row.video = null;
          }
          break;
        case 'no':
        case 'content':
        case 'narration':
        case 'copy':
          row[parts[2]] = typeof v === 'string' ? v : (v == null ? '' : String(v));
          break;
      }
    }

    var shots = [];
    for (var i = 0; i < order.length; i++) {
      var r = rows[order[i]];
      if (r.dead || !r.hasOrder) continue;
      shots.push({ id: r.id, no: r.no, content: r.content, narration: r.narration, copy: r.copy, video: r.video });
    }
    shots.sort(function (a, b) {
      var oa = rows[a.id].order;
      var ob = rows[b.id].order;
      if (oa !== ob) return oa - ob;
      return a.id < b.id ? -1 : 1;
    });

    var ev = function (field) { return regs['e' + SEP + field]; };
    var source = ev('source') && typeof ev('source').v === 'string' ? ev('source').v : '';
    var edit = null;
    if (source) {
      var cuts = ev('cuts');
      edit = {
        source: source,
        sourceName: ev('sourceName') ? String(ev('sourceName').v || '') : '',
        fps: ev('fps') ? Number(ev('fps').v) || 0 : 0,
        duration: ev('duration') ? Number(ev('duration').v) || 0 : 0,
        cuts: cuts && Array.isArray(cuts.v) ? cuts.v.map(Number).filter(function (n) { return isFinite(n); }) : [],
        currentTime: ev('currentTime') ? Number(ev('currentTime').v) || 0 : 0,
      };
    }
    return { version: 1, product: product, script: script, shots: shots, edit: edit };
  }

  /* ---------------- 本地缓存（断线期间刷新页面也不丢） ---------------- */

  function cacheKey(product, script) {
    return 'sm.collab.v1' + SEP + product + SEP + script;
  }

  function loadCache(product, script) {
    try {
      var raw = localStorage.getItem(cacheKey(product, script));
      if (!raw) return null;
      var data = JSON.parse(raw);
      if (!data || !data.epoch || !data.regs) return null;
      return data;
    } catch (_) {
      return null;
    }
  }

  function saveCache(product, script, epoch, regs, site) {
    try {
      var mine = {};
      for (var k in regs) {
        if (regs[k] && regs[k].s === site) mine[k] = regs[k];
      }
      localStorage.setItem(cacheKey(product, script), JSON.stringify({ epoch: epoch, regs: mine, at: Date.now() }));
    } catch (_) {}
  }

  function clearCache(product, script) {
    try { localStorage.removeItem(cacheKey(product, script)); } catch (_) {}
  }

  /* ---------------- 连接 ---------------- */

  function connect(opts) {
    var product = opts.product;
    var script = opts.script;
    var site = opts.site;
    var myName = opts.name || ('同事-' + site.slice(0, 4));
    var myColor = opts.color || colorFor(site);

    var ws = null;
    var closed = false;
    var retry = 0;
    var reconnectTimer = 0;
    var pollTimer = 0;
    var presenceTimer = 0;
    var cacheTimer = 0;
    var polling = false;
    var status = '';
    var cell = '';
    var epoch = opts.epoch || '';
    var queue = {};

    function q() {
      return 'product=' + encodeURIComponent(product) + '&script=' + encodeURIComponent(script);
    }

    function setStatus(s) {
      if (status === s) return;
      status = s;
      if (opts.onStatus) opts.onStatus(s);
    }

    function cacheLocal() {
      clearTimeout(cacheTimer);
      cacheTimer = setTimeout(persist, 250);
    }

    function persist() {
      var st = opts.getState && opts.getState();
      if (st) saveCache(product, script, epoch || st.epoch, st.regs, site);
    }

    function send(obj) {
      if (!ws || ws.readyState !== 1) return false;
      try {
        ws.send(JSON.stringify(obj));
        return true;
      } catch (_) {
        return false;
      }
    }

    function flushQueue() {
      var keys = Object.keys(queue);
      if (!keys.length) return;
      if (send({ t: 'merge', regs: queue })) queue = {};
    }

    function postMerge(regs) {
      if (!regs || !Object.keys(regs).length) return;
      fetch('/api/collab', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ product: product, script: script, site: site, name: myName, regs: regs }),
      })
        .then(function (r) { return r.json(); })
        .then(function (data) {
          if (data && data.epoch && opts.onSnapshot) {
            opts.onSnapshot({ epoch: data.epoch, now: data.now, regs: data.regs || {}, peers: data.peers || [], savedAt: data.savedAt });
          }
          if (opts.onAck) opts.onAck({ at: data && data.savedAt });
        })
        .catch(function () {});
    }

    function push(regs) {
      Object.assign(queue, regs);
      cacheLocal();
      if (polling) {
        var payload = queue;
        queue = {};
        postMerge(payload);
        return;
      }
      flushQueue();
    }

    function mineRegs() {
      var st = opts.getState && opts.getState();
      var out = {};
      if (!st) return out;
      for (var k in st.regs) {
        if (st.regs[k] && st.regs[k].s === site) out[k] = st.regs[k];
      }
      return out;
    }

    function applySnapshot(snap) {
      if (!snap || !snap.epoch) return;
      epoch = snap.epoch;
      if (opts.onSnapshot) opts.onSnapshot(snap);
      if (snap.peers && opts.onPeers) opts.onPeers(snap.peers);
    }

    function getJSON(url) {
      return fetch(url).then(function (r) {
        if (!r.ok) throw new Error('HTTP ' + r.status);
        return r.json();
      });
    }

    function startPolling() {
      if (polling || closed) return;
      polling = true;
      setStatus('polling');
      var tick = function () {
        if (closed) return;
        getJSON('/api/collab?' + q() + '&site=' + encodeURIComponent(site) + '&name=' + encodeURIComponent(myName))
          .then(applySnapshot)
          .catch(function () {});
        if (Object.keys(queue).length) {
          var payload = queue;
          queue = {};
          postMerge(payload);
        }
      };
      tick();
      pollTimer = setInterval(tick, 3000);
    }

    function stopPolling() {
      polling = false;
      clearInterval(pollTimer);
      pollTimer = 0;
    }

    function wsUrl() {
      var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
      return proto + '//' + location.host + '/api/collab/ws?' + q() +
        '&site=' + encodeURIComponent(site) + '&name=' + encodeURIComponent(myName) +
        '&color=' + encodeURIComponent(myColor);
    }

    function scheduleReconnect() {
      clearTimeout(reconnectTimer);
      retry += 1;
      if (retry >= 3) startPolling();
      setStatus(retry >= 3 ? 'polling' : 'reconnecting');
      var delay = Math.min(5000, 400 * Math.pow(1.7, Math.min(retry, 6)));
      reconnectTimer = setTimeout(open, delay);
    }

    function open() {
      if (closed) return;
      setStatus(retry ? 'reconnecting' : 'connecting');
      var sock;
      try {
        sock = new WebSocket(wsUrl());
      } catch (_) {
        scheduleReconnect();
        return;
      }
      ws = sock;
      sock.onopen = function () {
        retry = 0;
        stopPolling();
        setStatus('online');
        flushQueue();
      };
      sock.onmessage = function (ev) {
        var msg = null;
        try { msg = JSON.parse(ev.data); } catch (_) { return; }
        if (!msg || !msg.t) return;
        if (msg.t === 'hello') {
          applySnapshot({ epoch: msg.epoch, now: msg.now, regs: msg.regs || {}, peers: msg.peers || [], savedAt: msg.savedAt });
          // 把自己的（含离线期间的）改动补发一次，幂等合并不怕重复
          var mine = mineRegs();
          if (Object.keys(mine).length) send({ t: 'merge', regs: mine });
          if (cell) send({ t: 'presence', cell: cell });
        } else if (msg.t === 'merge') {
          if (opts.onMerge) opts.onMerge(msg.regs || {}, msg.from || '');
        } else if (msg.t === 'peers') {
          if (opts.onPeers) opts.onPeers(msg.peers || []);
        } else if (msg.t === 'ack') {
          if (opts.onAck) opts.onAck({ at: msg.savedAt });
        } else if (msg.t === 'reload') {
          getJSON('/api/collab?' + q() + '&site=' + encodeURIComponent(site)).then(applySnapshot).catch(function () {});
        } else if (msg.t === 'tree') {
          // 列表级变更（使用状态、脚本增删改），通知上层刷新左侧列表
          if (opts.onTree) opts.onTree();
        } else if (msg.t === 'moved') {
          // 当前脚本被同事重命名：房间已失效，停止重连并交给上层切换
          closed = true;
          if (opts.onMoved) opts.onMoved(msg);
          try { sock.close(); } catch (_) {}
        } else if (msg.t === 'gone') {
          closed = true;
          if (opts.onGone) opts.onGone(msg);
          try { sock.close(); } catch (_) {}
        }
      };
      sock.onclose = function () {
        ws = null;
        if (closed) return;
        scheduleReconnect();
      };
    }

    function presence(next) {
      if (cell === next) return;
      cell = next || '';
      if (!send({ t: 'presence', cell: cell })) return;
    }

    function rename(name) {
      var clean = String(name || '').trim().slice(0, 16);
      if (!clean) return;
      myName = clean;
      if (!send({ t: 'rename', name: clean })) return;
    }

    function flushNow() {
      if (status === 'online') flushQueue();
      else if (polling && Object.keys(queue).length) {
        var payload = queue;
        queue = {};
        postMerge(payload);
      }
      persist();
    }

    function stop() {
      closed = true;
      clearTimeout(reconnectTimer);
      clearInterval(pollTimer);
      clearInterval(presenceTimer);
      clearTimeout(cacheTimer);
      persist();
      if (ws) {
        try { ws.close(); } catch (_) {}
        ws = null;
      }
    }

    presenceTimer = setInterval(function () {
      if (cell) send({ t: 'presence', cell: cell });
    }, 25000);

    open();

    return {
      push: push,
      presence: presence,
      rename: rename,
      flushNow: flushNow,
      stop: stop,
      status: function () { return status; },
      epoch: function () { return epoch; },
    };
  }

  return {
    createState: createState,
    mergeRegs: mergeRegs,
    project: project,
    setReg: setReg,
    createShot: createShot,
    deleteShot: deleteShot,
    shotKey: shotKey,
    editKey: editKey,
    observeNow: observeNow,
    identity: identity,
    setName: setName,
    loadCache: loadCache,
    saveCache: saveCache,
    clearCache: clearCache,
    connect: connect,
    colorFor: colorFor,
  };
});
