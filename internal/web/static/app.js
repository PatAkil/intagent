// intagent dashboard.
//
// Everything the API returns was reported by agents (member names, tasks,
// branches, paths, notes) and is untrusted. It only ever reaches the page
// through textContent, createElement and setAttribute on inert attributes;
// nothing here assigns innerHTML.
'use strict';

(function () {
  const REFRESH_MS = 15000;
  const RELTIME_MS = 10000;
  const DEBOUNCE_MS = 300; // after the first event of a burst
  const RELOAD_GAP_MS = 1000; // between board reloads, at least
  const RELOAD_JITTER_MS = 500;
  const FEED_CAP = 200;
  const FEED_REDRAW_MS = 250; // between redraws of the feed, at least
  const FILES_SHOWN = 8;
  const HOT_SHOWN = 8;
  const STORE_KEY = 'intagent.repo';
  const FILTER_KEY = 'intagent.feedFilter';

  const AGENTS = { 'claude-code': 'Claude Code', codex: 'Codex', cursor: 'Cursor', copilot: 'Copilot CLI', gemini: 'Gemini CLI', watch: 'watch', cli: 'CLI' };

  const STATES = {
    working: { icon: 'dot', hint: 'A turn is in progress.' },
    waiting: { icon: 'pause', hint: 'The turn ended; the agent is waiting for its person.' },
    stalled: { icon: 'warn', hint: 'Working, but silent for too long, or stuck inside one tool call.' },
    gone: { icon: 'xcircle', hint: 'Silent for hours without a clean end: crashed, killed or abandoned.' },
    ended: { icon: 'stop', hint: 'The session ended cleanly.' },
  };

  const S = {
    me: null, // member name, or '' when reading without a token
    version: '',
    policy: null,
    repos: [],
    reposKey: '',
    repo: null,
    view: null,
    viewKey: '',
    feed: new Map(),
    fresh: new Set(),
    filter: 'all',
    expanded: new Set(),
    hotExpanded: false,
    es: null,
    conn: 'connecting',
    everLive: false,
    retry: 0,
    reconnectTimer: 0,
    debounce: 0,
    inflight: false,
    again: false,
    lastFetchAt: 0, // when the latest board reload started, by tick()
    lastFetchMs: 0, // and how long it took
    etag: '', // the validator of the board in S.view
    feedTimer: 0,
    feedAt: 0, // when the feed was last drawn, by tick()
    skew: 0,
    epoch: '', // the server process the board came from
    boardError: '',
    serverNote: '',
    timers: [],
  };

  // --- small DOM helpers ----------------------------------------------------

  const $ = (id) => document.getElementById(id);

  function el(tag, props, ...kids) {
    const n = document.createElement(tag);
    if (props) {
      for (const k of Object.keys(props)) {
        const v = props[k];
        if (v == null || v === false) continue;
        if (k === 'class') n.className = v;
        else if (k === 'text') n.textContent = String(v);
        else if (k === 'data') for (const d of Object.keys(v)) n.dataset[d] = String(v[d]);
        else if (k === 'on') for (const ev of Object.keys(v)) n.addEventListener(ev, v[ev]);
        else n.setAttribute(k, v === true ? '' : String(v));
      }
    }
    add(n, kids);
    return n;
  }

  function add(n, kids) {
    for (const k of kids) {
      if (k == null || k === false || k === '') continue;
      if (Array.isArray(k)) add(n, k);
      else if (typeof k === 'string' || typeof k === 'number') n.appendChild(document.createTextNode(String(k)));
      else n.appendChild(k);
    }
  }

  const SVGNS = 'http://www.w3.org/2000/svg';
  const F = { fill: 'currentColor', stroke: 'none' };
  const ICONS = {
    dot: [['circle', Object.assign({ cx: 8, cy: 8, r: 4 }, F)]],
    ring: [['circle', { cx: 8, cy: 8, r: 4 }]],
    pause: [['rect', Object.assign({ x: 4.5, y: 3.5, width: 2.5, height: 9, rx: 0.6 }, F)],
      ['rect', Object.assign({ x: 9, y: 3.5, width: 2.5, height: 9, rx: 0.6 }, F)]],
    warn: [['path', { d: 'M8 2.2 14.3 13.3H1.7Z' }], ['path', { d: 'M8 6.3v3.2' }],
      ['circle', Object.assign({ cx: 8, cy: 11.4, r: 0.85 }, F)]],
    xcircle: [['circle', { cx: 8, cy: 8, r: 5.8 }], ['path', { d: 'M5.9 5.9l4.2 4.2M10.1 5.9l-4.2 4.2' }]],
    stop: [['rect', Object.assign({ x: 4, y: 4, width: 8, height: 8, rx: 1 }, F)]],
    play: [['path', Object.assign({ d: 'M5 3.2 12.8 8 5 12.8Z' }, F)]],
    lock: [['rect', { x: 3.5, y: 7, width: 9, height: 6.5, rx: 1.2 }], ['path', { d: 'M5.5 7V5.3a2.5 2.5 0 0 1 5 0V7' }]],
    unlock: [['rect', { x: 3.5, y: 7, width: 9, height: 6.5, rx: 1.2 }], ['path', { d: 'M5.5 7V5.3a2.5 2.5 0 0 1 4.8-1' }]],
    shared: [['circle', { cx: 6, cy: 8, r: 3.6 }], ['circle', { cx: 10, cy: 8, r: 3.6 }]],
    pencil: [['path', { d: 'M3 13l.7-2.8 7.2-7.2 2.1 2.1-7.2 7.2Z' }], ['path', { d: 'M9.5 4.4l2.1 2.1' }]],
    branch: [['circle', { cx: 5, cy: 3.8, r: 1.6 }], ['circle', { cx: 5, cy: 12.2, r: 1.6 }],
      ['circle', { cx: 11, cy: 5.2, r: 1.6 }], ['path', { d: 'M5 5.4v5.2M11 6.8c0 2.8-6 1.6-6 3.8' }]],
    sync: [['path', { d: 'M3.2 7.4A4.9 4.9 0 0 1 12 5.2M12.8 8.6A4.9 4.9 0 0 1 4 10.8' }],
      ['path', { d: 'M12.3 2.4v2.9H9.4M3.7 13.6v-2.9h2.9' }]],
    flag: [['path', { d: 'M4 14V2.5' }], ['path', { d: 'M4 3h7.5l-1.8 2.6 1.8 2.6H4' }]],
    check: [['path', { d: 'M3.2 8.6l3.1 3 6.5-7.2' }]],
    clock: [['circle', { cx: 8, cy: 8, r: 5.8 }], ['path', { d: 'M8 4.8V8l2.2 1.6' }]],
    note: [['path', { d: 'M2.5 3.5h11v7.2H8l-3.2 2.6v-2.6H2.5Z' }]],
    deny: [['path', { d: 'M5.6 1.9h4.8l3.7 3.7v4.8l-3.7 3.7H5.6l-3.7-3.7V5.6Z' }], ['path', { d: 'M5.3 8h5.4' }]],
    overlap: [['rect', { x: 2, y: 2, width: 7.5, height: 7.5, rx: 1 }], ['rect', { x: 6.5, y: 6.5, width: 7.5, height: 7.5, rx: 1 }]],
    nearby: [['circle', Object.assign({ cx: 8, cy: 8, r: 1.8 }, F)], ['circle', { cx: 8, cy: 8, r: 5.6, 'stroke-dasharray': '2.2 2' }]],
    ask: [['circle', { cx: 8, cy: 8, r: 5.8 }], ['path', { d: 'M6.3 6.4a1.8 1.8 0 1 1 2.6 1.6c-.6.3-.9.7-.9 1.3' }],
      ['circle', Object.assign({ cx: 8, cy: 11.2, r: 0.8 }, F)]],
    recover: [['path', { d: 'M3.2 8.6a4.9 4.9 0 1 0 1.6-4.4' }], ['path', { d: 'M3.6 2.4v2.9h2.9' }]],
    mail: [['rect', { x: 2, y: 3.8, width: 12, height: 8.6, rx: 1.2 }], ['path', { d: 'M2.4 4.5 8 8.8l5.6-4.3' }]],
    git: [['circle', { cx: 8, cy: 8, r: 2.2 }], ['path', { d: 'M1.8 8h4M10.2 8h4' }]],
    eye: [['path', { d: 'M1.6 8s2.4-4.4 6.4-4.4S14.4 8 14.4 8s-2.4 4.4-6.4 4.4S1.6 8 1.6 8Z' }], ['circle', { cx: 8, cy: 8, r: 1.9 }]],
    chevron: [['path', { d: 'M5 6.5 8 9.5l3-3' }]],
  };

  function icon(name, cls) {
    const s = document.createElementNS(SVGNS, 'svg');
    s.setAttribute('viewBox', '0 0 16 16');
    s.setAttribute('class', 'ic' + (cls ? ' ' + cls : ''));
    s.setAttribute('aria-hidden', 'true');
    s.setAttribute('focusable', 'false');
    for (const [tag, attrs] of ICONS[name] || ICONS.dot) {
      const c = document.createElementNS(SVGNS, tag);
      for (const k of Object.keys(attrs)) c.setAttribute(k, String(attrs[k]));
      s.appendChild(c);
    }
    return s;
  }

  function code(text, cls) {
    return el('code', { class: cls || null }, text);
  }

  // A repo-relative path, its directory quieter than its base name.
  // It may wrap after a slash, so a narrow screen does not split a name.
  function pathNode(p) {
    const i = p.lastIndexOf('/');
    const dir = i >= 0 ? p.slice(0, i + 1).split('/').slice(0, -1).flatMap((seg) => [seg + '/', el('wbr')]) : [];
    return el('code', { class: 'path', title: p },
      i >= 0 ? el('span', { class: 'path-dir' }, dir) : null,
      el('span', { class: 'path-base' }, i >= 0 ? p.slice(i + 1) : p));
  }

  // --- storage, wrapped: private windows and blocked storage throw ---------

  function storeGet(k) {
    try { return window.localStorage.getItem(k); } catch (_) { return null; }
  }

  function storeSet(k, v) {
    try { window.localStorage.setItem(k, v); } catch (_) { /* not available: the choice lasts for this page */ }
  }

  // --- time ------------------------------------------------------------------

  function tsOf(s) {
    const t = Date.parse(s);
    // Go writes a zero time as year 1; treat anything before 2000 as unset.
    return Number.isFinite(t) && t > 946684800000 ? t : NaN;
  }

  const serverNow = () => Date.now() + S.skew;

  // tick is the clock that paces reloads and redraws. It only moves forward:
  // the wall clock can step either way (a time sync, a clock set by hand),
  // and a wait measured on it could then last an hour.
  const tick = typeof performance === 'object' && performance && typeof performance.now === 'function'
    ? () => performance.now()
    : () => Date.now();

  function span(ms) {
    if (!(ms >= 0)) ms = 0;
    if (ms < 60000) return Math.floor(ms / 1000) + 's';
    if (ms < 3600000) return Math.floor(ms / 60000) + 'm';
    if (ms < 48 * 3600000) {
      const h = Math.floor(ms / 3600000);
      const m = Math.floor((ms % 3600000) / 60000);
      return h < 10 && m ? h + 'h ' + m + 'm' : h + 'h';
    }
    return Math.floor(ms / 86400000) + 'd';
  }

  function fmtTime(t, fmt) {
    if (!Number.isFinite(t)) return '';
    const d = serverNow() - t;
    switch (fmt) {
      case 'dur': return span(d);
      case 'seen': return d < 5000 ? 'seen just now' : 'seen ' + span(d) + ' ago';
      default: return d < 5000 ? 'just now' : span(d) + ' ago';
    }
  }

  // One formatter for every tooltip: toLocaleString with options builds a new
  // one on each call, and a busy board shows thousands of times.
  let absFormat = null;

  function absTime(t) {
    try {
      absFormat = absFormat || new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'medium' });
      return absFormat.format(new Date(t));
    } catch (_) {
      return new Date(t).toISOString();
    }
  }

  // A <time> that keeps itself current: updateTimes() rewrites every one.
  function timeNode(s, fmt, prefix) {
    const t = tsOf(s);
    if (!Number.isFinite(t)) return null;
    return el('time', {
      datetime: new Date(t).toISOString(),
      title: (prefix ? prefix + ' ' : '') + absTime(t),
      data: { ts: t, fmt: fmt || 'ago' },
    }, fmtTime(t, fmt));
  }

  function updateTimes() {
    for (const n of document.querySelectorAll('time[data-ts]')) {
      const s = fmtTime(Number(n.dataset.ts), n.dataset.fmt);
      if (n.textContent !== s) n.textContent = s;
    }
  }

  // --- glob matching, as internal/glob does it -------------------------------
  // Segments split on '/'; '**' matches any number of segments; a pattern that
  // runs out first covers everything below it; each other segment follows
  // Go's path.Match (*, ?, [classes], \ escapes).

  const segCache = new Map();

  function segRegExp(seg) {
    if (segCache.has(seg)) return segCache.get(seg);
    const esc = (c) => c.replace(/[\\^$.*+?()[\]{}|/]/g, '\\$&');
    const escClass = (c) => c.replace(/[\\\]\[^-]/g, '\\$&');
    const cs = Array.from(seg);
    let out = '^';
    let ok = true;
    for (let i = 0; i < cs.length && ok;) {
      const c = cs[i];
      if (c === '*') { out += '[^/]*'; i++; }
      else if (c === '?') { out += '[^/]'; i++; }
      else if (c === '\\') {
        if (i + 1 >= cs.length) ok = false;
        else { out += esc(cs[i + 1]); i += 2; }
      } else if (c === '[') {
        let j = i + 1;
        let neg = false;
        if (cs[j] === '^') { neg = true; j++; }
        let cls = '';
        let closed = false;
        while (j < cs.length) {
          let lo = cs[j];
          if (lo === ']') { closed = cls !== ''; j++; break; }
          if (lo === '\\') { j++; lo = cs[j]; }
          if (lo === undefined) break;
          j++;
          cls += escClass(lo);
          if (cs[j] === '-' && cs[j + 1] !== undefined && cs[j + 1] !== ']') {
            let hi = cs[j + 1];
            j += 2;
            if (hi === '\\') { hi = cs[j]; j++; }
            if (hi === undefined) break;
            cls += '-' + escClass(hi);
          }
        }
        if (!closed) ok = false;
        else { out += '[' + (neg ? '^' : '') + cls + ']'; i = j; }
      } else { out += esc(c); i++; }
    }
    let re = null;
    if (ok) {
      try { re = new RegExp(out + '$', 'u'); } catch (_) { re = null; }
    }
    segCache.set(seg, re);
    return re;
  }

  function matchSegs(p, s, i, j) {
    for (; i < p.length; i++, j++) {
      if (p[i] === '**') {
        for (let k = j; k <= s.length; k++) if (matchSegs(p, s, i + 1, k)) return true;
        return false;
      }
      if (j >= s.length) return false;
      const re = segRegExp(p[i]);
      if (!re || !re.test(s[j])) return false;
    }
    return true;
  }

  // The directory a pattern is rooted in: its leading segments without
  // wildcards, as internal/glob's LiteralDir has it. Every name the pattern
  // matches is that directory or below it.
  function literalDir(pattern) {
    const segs = String(pattern).split('/');
    let n = 0;
    while (n < segs.length && !/[*?[\\]/.test(segs[n])) n++;
    return segs.slice(0, n).join('/');
  }

  // The paths in sorted that are dir or below it ('' is everything).
  function under(sorted, dir) {
    if (!dir) return sorted;
    const from = (x) => {
      let lo = 0;
      let hi = sorted.length;
      while (lo < hi) {
        const mid = (lo + hi) >>> 1;
        if (sorted[mid] < x) lo = mid + 1; else hi = mid;
      }
      return lo;
    };
    const out = [];
    const i = from(dir);
    if (sorted[i] === dir) out.push(dir);
    const below = dir + '/';
    for (let j = from(below); j < sorted.length && sorted[j].startsWith(below); j++) out.push(sorted[j]);
    return out;
  }

  // --- API -------------------------------------------------------------------

  class AuthError extends Error {}

  // A request that hangs must not hold the board's refresh slot for good.
  const FETCH_TIMEOUT_MS = 15000;

  // The timeout covers the body too: a response that stalls halfway is as
  // stuck as one that never starts. read turns the response into the result.
  async function request(url, headers, read) {
    const ctl = typeof AbortController === 'function' ? new AbortController() : null;
    const timer = ctl ? setTimeout(() => ctl.abort(), FETCH_TIMEOUT_MS) : 0;
    try {
      const r = await fetch(url, {
        credentials: 'same-origin', cache: 'no-store', signal: ctl ? ctl.signal : undefined,
        headers: Object.assign({ Accept: 'application/json' }, headers),
      });
      if (r.status === 401) throw new AuthError('not signed in');
      if (!r.ok && r.status !== 304) {
        let msg = 'HTTP ' + r.status;
        try { const b = await r.json(); if (b && typeof b.error === 'string') msg = b.error; } catch (_) { /* keep status */ }
        throw new Error(msg);
      }
      return await read(r);
    } finally {
      clearTimeout(timer);
    }
  }

  const getJSON = (url) => request(url, {}, (r) => r.json());

  // A repository's board, or null when the server says the one the page holds
  // (etag) is still current. The page sets If-None-Match itself, so the
  // browser hands the 304 over rather than a stored copy with an old "at".
  function getBoard(repo, etag) {
    return request('/v1/board?repo=' + encodeURIComponent(repo), etag ? { 'If-None-Match': etag } : {},
      async (r) => (r.status === 304 ? null : { view: await r.json(), etag: r.headers.get('ETag') || '' }));
  }

  // --- people ----------------------------------------------------------------

  function initials(name) {
    const parts = String(name || '?').split(/[._\-\s]+/).filter(Boolean);
    const first = (s) => Array.from(s)[0] || '';
    const s = parts.length >= 2 ? first(parts[0]) + first(parts[1]) : first(parts[0] || '?');
    return s.toUpperCase();
  }

  function hue(name) {
    let h = 2166136261;
    for (const ch of String(name)) {
      h ^= ch.codePointAt(0);
      h = Math.imul(h, 16777619);
    }
    return (h >>> 0) % 8;
  }

  function avatar(name, cls) {
    return el('span', { class: 'avatar av-' + hue(name) + (cls ? ' ' + cls : ''), 'aria-hidden': 'true' }, initials(name));
  }

  const agentName = (a) => AGENTS[a] || a || 'agent';

  // --- views: login and board ------------------------------------------------

  function show(id, on) { $(id).hidden = !on; }

  function showLogin(canCancel) {
    stopLive();
    show('boot', false);
    show('board', false);
    show('foot', false);
    show('repo-pick', false);
    show('conn', false);
    show('who', false);
    show('logout-form', false);
    show('signin-btn', false);
    show('login', true);
    show('login-error', !!S.loginFailed);
    $('token').setAttribute('aria-invalid', S.loginFailed ? 'true' : 'false');
    $('token').setAttribute('aria-describedby', S.loginFailed ? 'login-error token-help' : 'token-help');
    show('login-back', !!canCancel);
    document.title = 'Sign in · intagent';
    const t = $('token');
    t.value = '';
    t.focus();
  }

  function showBoard() {
    show('boot', false);
    show('login', false);
    show('board', true);
    show('conn', true);
    renderWho();
    renderFoot();
    startLive();
    if (S.repo) {
      connectStream();
      refreshBoard();
    }
    loadRepos();
  }

  function showUnreachable(msg) {
    const b = $('boot');
    b.hidden = false;
    b.textContent = 'Cannot reach the intagent server (' + msg + '). Retrying…';
  }

  function renderWho() {
    const who = $('who');
    who.replaceChildren();
    who.hidden = false;
    if (S.me) {
      who.append(avatar(S.me, 'avatar-sm'), el('span', { class: 'who-name', title: 'Signed in as ' + S.me }, S.me));
      show('logout-form', true);
      show('signin-btn', false);
    } else {
      who.append(S.demo
        ? el('span', { class: 'ro', title: 'Simulated agents on an in-memory board; nothing is saved' }, icon('play'), 'demo')
        : el('span', { class: 'ro', title: 'This server lets anyone who can reach it read the board' }, icon('eye'), 'read-only'));
      show('logout-form', false);
      show('signin-btn', !S.demo);
    }
  }

  function renderFoot() {
    show('foot', true);
    $('foot-text').replaceChildren(
      'intagent ', S.version ? code(S.version) : '', ' · ', location.host,
      ' · board refreshes a second or so after events, and every 15 s');
  }

  function toLogin() {
    S.loginFailed = false;
    showLogin(false);
  }

  // --- live updates ------------------------------------------------------------

  function startLive() {
    stopLive();
    S.timers.push(setInterval(() => { scheduleRefresh(); loadRepos(); }, REFRESH_MS));
    S.timers.push(setInterval(updateTimes, RELTIME_MS));
  }

  function stopLive() {
    for (const t of S.timers) clearInterval(t);
    S.timers = [];
    clearTimeout(S.reconnectTimer);
    clearTimeout(S.debounce);
    S.debounce = 0;
    clearTimeout(S.feedTimer);
    S.feedTimer = 0;
    if (S.es) { S.es.close(); S.es = null; }
  }

  const CONN_TEXT = {
    connecting: ['Connecting', 'Opening the live event stream'],
    live: ['Live', 'Receiving events as they happen'],
    reconnecting: ['Reconnecting', 'The live stream dropped; reconnecting and catching up on missed events'],
    offline: ['Offline', 'The live stream is closed; retrying'],
  };

  function setConn(state) {
    S.conn = state;
    const c = $('conn');
    c.dataset.state = state;
    const [text, title] = CONN_TEXT[state];
    $('conn-text').textContent = text;
    c.title = title;
  }

  function connectStream() {
    if (S.es) { S.es.close(); S.es = null; }
    clearTimeout(S.reconnectTimer);
    if (typeof EventSource === 'undefined') { setConn('offline'); return; }
    const es = new EventSource('/v1/stream' + (S.repo ? '?repo=' + encodeURIComponent(S.repo) : ''));
    S.es = es;
    setConn(S.everLive ? 'reconnecting' : 'connecting');
    es.onopen = () => {
      if (S.es !== es) return;
      const recovering = S.everLive && S.conn !== 'live';
      S.everLive = true;
      S.retry = 0;
      setConn('live');
      if (recovering) scheduleRefresh();
    };
    // The server says when agents' edits go ahead unchecked because it cannot
    // answer them in time, and when it can again.
    es.addEventListener('status', (ev) => {
      if (S.es !== es) return;
      let st;
      try { st = JSON.parse(ev.data); } catch (_) { return; }
      setServerStatus(st);
    });
    es.addEventListener('activity', (ev) => {
      if (S.es !== es) return;
      let a;
      try { a = JSON.parse(ev.data); } catch (_) { return; }
      if (!a || typeof a !== 'object') return;
      if (!S.repo) { loadRepos(); return; }
      if (a.repo !== S.repo) return;
      mergeFeed([a], true);
      scheduleRefresh();
    });
    es.onerror = () => {
      if (S.es !== es) return;
      if (es.readyState === EventSource.CLOSED) {
        setConn('offline');
        scheduleReconnect();
      } else {
        setConn('reconnecting');
      }
    };
  }

  // The browser gives up on a stream that answers with an error status; check
  // whether the session is still signed in, then open a new one.
  function scheduleReconnect() {
    clearTimeout(S.reconnectTimer);
    const delay = Math.min(30000, 2000 * Math.pow(2, S.retry++));
    S.reconnectTimer = setTimeout(async () => {
      try {
        await getJSON('/v1/whoami');
      } catch (e) {
        if (e instanceof AuthError) { toLogin(); return; }
        scheduleReconnect();
        return;
      }
      connectStream();
      refreshBoard();
    }, delay);
  }

  // Throttled, not debounced: under steady activity a debounce would never
  // fire, and the board would sit still while the feed moved. Reloads are a
  // second apart and up to half a second more, so tabs on one board drift
  // apart, and twice as far apart as the last one took, drawing included, so
  // neither a slow server nor a slow laptop is asked to go faster than it can.
  // They are never further apart than the poll's 15 s, though: a reload that
  // only seemed long, because the laptop slept through it, must not hold the
  // board still for as long, and the poll reloads within its period. The feed
  // itself is live.
  function scheduleRefresh() {
    if (S.debounce) return;
    const gap = Math.min(REFRESH_MS, Math.max(RELOAD_GAP_MS + Math.random() * RELOAD_JITTER_MS, 2 * S.lastFetchMs));
    const wait = Math.min(gap, Math.max(DEBOUNCE_MS, S.lastFetchAt + gap - tick()));
    S.debounce = setTimeout(() => { S.debounce = 0; refreshBoard(); }, wait);
  }

  // --- repositories ------------------------------------------------------------

  async function loadRepos() {
    let repos;
    try {
      repos = await getJSON('/v1/repos');
    } catch (e) {
      if (e instanceof AuthError) { toLogin(); return; }
      lostContact(e);
      return;
    }
    S.repos = Array.isArray(repos) ? repos.filter((r) => r && typeof r.repo === 'string') : [];
    // A new server process: the board's next answer starts the feed over.
    const epoch = (S.repos.find((r) => typeof r.epoch === 'string' && r.epoch) || {}).epoch;
    if (epoch && S.epoch && epoch !== S.epoch && S.repo) refreshBoard();
    if (!S.repo) {
      const saved = storeGet(STORE_KEY);
      const pick = S.repos.some((r) => r.repo === saved) ? saved : (S.repos[0] && S.repos[0].repo);
      renderRepoPicker();
      if (pick) selectRepo(pick, false);
      else if (!S.es) { connectStream(); renderAll(); }
      else renderAll();
      return;
    }
    renderRepoPicker();
  }

  function renderRepoPicker() {
    const list = S.repos.slice();
    if (S.repo && !list.some((r) => r.repo === S.repo)) list.push({ repo: S.repo, claims: 0, active_claims: 0 });
    const key = JSON.stringify(list.map((r) => [r.repo, r.active_claims, r.claims])) + '|' + S.repo;
    show('repo-pick', list.length > 0);
    if (key === S.reposKey) return;
    S.reposKey = key;
    const sel = $('repo');
    sel.replaceChildren(...list.map((r) => {
      const n = r.active_claims | 0;
      const o = el('option', { value: r.repo }, r.repo + (n ? '  ·  ' + n + ' active' : '  ·  idle'));
      if (r.repo === S.repo) o.selected = true;
      return o;
    }));
    sel.title = S.repo || '';
  }

  function selectRepo(repo, remember) {
    if (remember) storeSet(STORE_KEY, repo);
    if (repo === S.repo && S.view) return;
    S.repo = repo;
    S.view = null;
    S.viewKey = '';
    S.etag = '';
    S.feed = new Map();
    S.fresh.clear();
    S.expanded.clear();
    S.hotExpanded = false;
    S.boardError = '';
    renderRepoPicker();
    renderAll();
    connectStream();
    refreshBoard();
  }

  // --- the board ---------------------------------------------------------------

  async function refreshBoard() {
    if (!S.repo) return;
    if (S.inflight) { S.again = true; return; }
    S.inflight = true;
    const repo = S.repo;
    // Every event seen so far happened before this request, so the board it
    // gets back counts at least this far.
    const seenSeq = S.feed.size ? Math.max(...S.feed.keys()) : 0;
    const started = tick();
    S.lastFetchAt = started;
    try {
      const got = await getBoard(repo, S.view ? S.etag : '');
      if (repo !== S.repo) return;
      S.lastOk = Date.now();
      setBoardError('');
      if (!got) {
        // Unchanged since the board the page shows: only its age is new.
        asOf(S.lastOk + S.skew);
        updateTimes();
        return;
      }
      const v = got.view;
      S.etag = got.etag;
      setServerStatus(v && v.server);
      const at = tsOf(v.at);
      if (Number.isFinite(at)) S.skew = at - Date.now();
      S.view = normalise(v);
      if (restarted(S.view, seenSeq)) {
        S.feed = new Map();
        S.fresh.clear();
        connectStream();
      }
      mergeFeed(S.view.recent, false);
      const key = JSON.stringify([S.view.claims, S.view.stats, S.view.policy, S.view.live_sessions]);
      if (key !== S.viewKey) {
        S.viewKey = key;
        renderBoard();
      } else {
        asOf(at);
        updateTimes();
      }
    } catch (e) {
      if (e instanceof AuthError) { toLogin(); return; }
      lostContact(e);
    } finally {
      S.lastFetchMs = tick() - started;
      S.inflight = false;
      if (S.again) { S.again = false; scheduleRefresh(); }
    }
  }

  // A restarted server may number its events from 1 again: the feed and the
  // stream then start over rather than skip the new events. The server names
  // its process with an epoch; an older server has its numbers checked instead.
  function restarted(v, seenSeq) {
    if (v.epoch) {
      const was = S.epoch;
      S.epoch = v.epoch;
      return was !== '' && was !== v.epoch;
    }
    const reused = v.recent.some((a) => a && S.feed.has(a.seq) &&
      (S.feed.get(a.seq).at !== a.at || S.feed.get(a.seq).kind !== a.kind));
    return v.last_seq < seenSeq || reused;
  }

  // Moves the "as of" time of a board that did not change otherwise.
  function asOf(t) {
    if (!S.view || !Number.isFinite(t)) return;
    S.view.at = new Date(t).toISOString();
    const n = $('as-of');
    if (!n) return;
    n.dataset.ts = String(t);
    n.setAttribute('datetime', S.view.at);
    n.title = 'Board read at ' + absTime(t);
  }

  const arr = (x) => (Array.isArray(x) ? x : []);

  function normalise(v) {
    v = v && typeof v === 'object' ? v : {};
    const claims = arr(v.claims).filter((c) => c && typeof c === 'object').map((c) => ({
      id: String(c.id || ''),
      member: String(c.member || '?'),
      host: String(c.host || ''),
      worktree: String(c.worktree || ''),
      branch: c.branch ? String(c.branch) : '',
      task: c.task ? String(c.task) : '',
      active: !!c.active,
      intents: arr(c.intents).filter((i) => i && typeof i.pattern === 'string'),
      files: arr(c.files).filter((f) => f && typeof f.path === 'string'),
      fileCount: Number(c.file_count) || arr(c.files).length,
      truncated: !!c.truncated,
      sessions: arr(c.sessions).filter((s) => s && typeof s === 'object'),
      pending: Number(c.pending_inbox) || 0,
      created_at: c.created_at,
      updated_at: c.updated_at,
    }));
    return {
      repo: String(v.repo || ''),
      at: v.at,
      claims,
      recent: arr(v.recent),
      stats: v.stats && typeof v.stats === 'object' ? v.stats : {},
      policy: v.policy && typeof v.policy === 'object' ? v.policy : {},
      members: arr(v.members),
      live_sessions: Number(v.live_sessions) || 0,
      last_seq: Number(v.last_seq) || 0,
      epoch: typeof v.epoch === 'string' ? v.epoch : '',
    };
  }

  function lostContact(e) {
    const as = S.lastOk ? ' Showing the board as of ' + new Date(S.lastOk).toLocaleTimeString() + '.' : '';
    setBoardError('Cannot reach the intagent server (' + e.message + ').' + as + ' Retrying every 15 s.');
  }

  function setServerStatus(st) {
    let msg = '';
    if (st && typeof st === 'object' && st.degraded === true) {
      msg = 'The intagent server is not answering agents in time, so their edits go ahead without a check' +
        (n(st.pre_edits_60s) ? ': ' + fmtNum(st.unchecked_60s) + ' of ' + fmtNum(st.pre_edits_60s) + ' in the last minute.' : '.');
    }
    if (S.serverNote === msg) return;
    S.serverNote = msg;
    const b = $('server-banner');
    b.hidden = !msg;
    b.replaceChildren(msg ? icon('warn') : '', msg);
  }

  function setBoardError(msg) {
    if (S.boardError === msg) return;
    S.boardError = msg;
    const b = $('board-error');
    b.hidden = !msg;
    b.replaceChildren(msg ? icon('warn') : '', msg);
  }

  // Keep keyboard focus on the same control across a re-render.
  function keepFocus(fn) {
    const a = document.activeElement;
    const key = a && a.dataset ? a.dataset.focus : null;
    fn();
    if (key) {
      for (const n of document.querySelectorAll('[data-focus]')) {
        if (n.dataset.focus === key) { n.focus({ preventScroll: true }); break; }
      }
    }
  }

  function renderAll() {
    renderBoard();
    renderFeed();
  }

  function renderBoard() {
    const v = S.view;
    const ax = v ? analyse(v.claims) : null;
    keepFocus(() => {
      renderStats(v);
      renderClaims(v, ax);
      renderHot(v, ax);
    });
    document.title = S.repo ? shortRepo(S.repo) + ' · intagent' : 'intagent';
  }

  function shortRepo(r) {
    const parts = String(r).split('/');
    return parts.slice(-2).join('/');
  }

  // --- cross-claim analysis --------------------------------------------------------

  function analyse(claims) {
    const byPath = new Map();
    const byArea = new Map();
    for (const c of claims) {
      for (const f of c.files) {
        if (!byPath.has(f.path)) byPath.set(f.path, []);
        byPath.get(f.path).push(c);
        if (f.area) {
          if (!byArea.has(f.area)) byArea.set(f.area, new Map());
          const m = byArea.get(f.area);
          if (!m.has(c.id)) m.set(c.id, { claim: c, files: 0 });
          m.get(c.id).files++;
        }
      }
    }
    // Files where an agent was refused or asked are contested too, though
    // only one claim changed them.
    const byId = new Map(claims.map((c) => [c.id, c]));
    // path -> claim -> what stopped it there, the most serious kept.
    const stoppedAt = new Map();
    const rank = { bumped: 1, asked: 2, refused: 3 };
    for (const a of S.feed.values()) {
      if (!a || a.kind !== 'conflict' || (a.decision !== 'deny' && a.decision !== 'ask') || !byId.has(a.claim_id)) continue;
      const how = conflictOutcome(a).key;
      for (const p of arr(a.paths)) {
        if (!stoppedAt.has(p)) stoppedAt.set(p, new Map());
        const m = stoppedAt.get(p);
        if ((rank[how] || 0) > (rank[m.get(a.claim_id)] || 0)) m.set(a.claim_id, how);
        if (!byPath.has(p)) byPath.set(p, []);
      }
    }
    // Which intents cover which of these paths. Each pattern is matched once
    // against the paths under its directory, not every path against every
    // pattern; the lists keep the claims' order, then each claim's intents'.
    const sorted = Array.from(byPath.keys()).sort();
    const segs = new Map(sorted.map((p) => [p, p.split('/')]));
    const covered = new Map();
    for (const c of claims) {
      for (const it of c.intents) {
        const pat = String(it.pattern).split('/');
        for (const p of under(sorted, literalDir(it.pattern))) {
          if (!matchSegs(pat, segs.get(p), 0, 0)) continue;
          if (!covered.has(p)) covered.set(p, []);
          covered.get(p).push([c, it]);
        }
      }
    }
    const files = new Map();
    for (const [path, touchers] of byPath) {
      const involved = new Map();
      const role = (c) => {
        if (!involved.has(c.id)) involved.set(c.id, { claim: c, changed: false, stopped: '', intents: [] });
        return involved.get(c.id);
      };
      for (const c of touchers) role(c).changed = true;
      for (const [id, how] of stoppedAt.get(path) || []) role(byId.get(id)).stopped = how;
      for (const [c, it] of covered.get(path) || []) role(c).intents.push(it);
      if (involved.size < 2) continue;
      files.set(path, { path, involved, severity: spotSeverity(involved) });
    }
    const areas = [];
    for (const [area, m] of byArea) if (m.size >= 2) areas.push({ area, involved: m });
    areas.sort((a, b) => b.involved.size - a.involved.size || (a.area < b.area ? -1 : 1));
    return { files, areas };
  }

  const reserves = (r) => r.claim.active && r.intents.some((i) => i.mode === 'exclusive');

  // How an agent was stopped at a file, as the hot spots and files say it.
  const STOPPED = { refused: 'was refused', bumped: 'was bumped', asked: 'had to ask' };

  // block: one claim reserves the file while another changed it.
  function spotSeverity(involved) {
    for (const r of involved.values()) {
      if (!reserves(r)) continue;
      for (const o of involved.values()) if (o !== r && (o.changed || o.stopped)) return 'block';
    }
    return 'overlap';
  }

  function roleText(r) {
    const out = [];
    if (r.changed) out.push('changed');
    if (r.stopped) out.push(STOPPED[r.stopped]);
    const intents = r.intents || [];
    if (intents.some((i) => i.mode === 'exclusive')) out.push(r.claim.active ? 'reserves' : 'reserved (not running)');
    else if (intents.length) out.push('plans');
    return out.join(', ');
  }

  function roleTitle(r) {
    const c = r.claim;
    const bits = [c.member + (c.branch ? ' on ' + c.branch : ''), c.host + ':' + c.worktree, c.active ? 'running' : 'not running'];
    for (const i of r.intents || []) bits.push(i.mode + ' intent ' + i.pattern);
    return bits.join(' · ');
  }

  // --- stats ------------------------------------------------------------------------

  function n(x) { return Number(x) || 0; }

  function fmtNum(x) {
    return n(x).toLocaleString();
  }

  function renderStats(v) {
    const st = v ? v.stats : {};
    // Loading shows dashes; a server with no repositories has honestly counted nothing.
    const known = !!v || !S.repo;
    // Under a radar policy nothing is stopped: the headline counts what was
    // spotted and warned about instead of a permanent zero.
    const policy = (v && v.policy) || S.policy || {};
    const radar = policy.block === 'warn' || policy.block === 'off';
    const caught = radar ? n(st.warned) + n(st.refused) + n(st.bumped) + n(st.asked) : n(st.refused) + n(st.bumped);
    const checks = n(st.checks);
    const share = checks ? Math.min(1, caught / checks) : 0;
    const since = tsOf(st.since);

    const meter = el('div', { class: 'meter', role: 'img', 'aria-label': checks ? caught + ' of ' + checks + ' checked edits were stopped' : 'No edits checked yet' },
      el('span', { class: 'meter-fill', style: 'width:' + (share * 100).toFixed(1) + '%' }));

    const hero = el('div', { class: 'hero' },
      el('p', { class: 'hero-label' }, radar ? 'Would-be collisions spotted' : 'Collisions caught before they happened'),
      el('div', { class: 'hero-row' },
        el('p', { class: 'hero-value', title: radar ? 'warned, under a policy that stops nothing' : 'refused + bumped' }, known ? fmtNum(caught) : '–'),
        radar
          ? el('dl', { class: 'hero-split' },
            el('div', { title: 'Agents told before the edit; the team policy lets them go ahead (block → ' + policy.block + ')' },
              el('dt', null, 'Warned'), el('dd', null, known ? fmtNum(st.warned) : '–')))
          : el('dl', { class: 'hero-split' },
            el('div', { title: 'Edits refused outright: an active claim holds the file exclusively' },
              el('dt', null, 'Refused'), el('dd', null, known ? fmtNum(st.refused) : '–')),
            el('div', { title: 'First attempts stopped with an explanation; a retry of the same path goes through' },
              el('dt', null, 'Bumped'), el('dd', null, known ? fmtNum(st.bumped) : '–')))),
      meter,
      el('p', { class: 'hero-foot' }, checks
        ? [fmtNum(caught), ' of ', fmtNum(checks), ' checked edits (', (share * 100).toFixed(share && share < 0.1 ? 1 : 0), '%)']
        : 'No edits checked yet',
        n(st.unheard) ? el('span', { class: 'hero-unheard', title: 'Edits whose agent went ahead before the server could answer' },
          ' · ' + fmtNum(st.unheard) + ' went ahead unchecked') : null));

    const tile = (label, value, hint) => el('div', { class: 'tile' },
      el('dt', null, label), el('dd', null, known ? fmtNum(value) : '–'), el('dd', { class: 'tile-hint' }, hint));

    const tiles = el('dl', { class: 'tiles' },
      tile('Edits checked', st.checks, 'against every other claim, before the write'),
      tile('Asked', st.asked, 'handed to the person to decide'),
      tile('Warned', st.warned, 'allowed, with a heads-up'),
      tile('Overlap alerts', st.alerts, 'claims told someone changed their files'),
      tile('Notes', st.notes, 'delivered between claims'),
      tile('Intents', st.intents, 'declared before multi-file changes'));

    const p = (v && v.policy) || S.policy || {};
    const pol = el('p', { class: 'policy', title: 'What the server does for each severity of conflict' },
      el('span', { class: 'policy-k' }, 'Policy'),
      polItem('block', p.block), polItem('overlap', p.overlap), polItem('nearby', p.nearby));

    const meta = el('div', { class: 'stats-meta' },
      el('p', { class: 'since' }, Number.isFinite(since)
        ? ['Counting since ', el('time', { datetime: new Date(since).toISOString(), title: absTime(since) },
          new Date(since).toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: 'numeric' }))]
        : 'Nothing counted yet'),
      pol);

    $('stats-body').replaceChildren(hero, tiles, meta);
  }

  function polItem(sev, action) {
    return el('span', { class: 'pol sev-' + sev },
      el('span', { class: 'pol-sev' }, sev), ' → ', el('strong', null, action || '?'));
  }

  // --- claims -----------------------------------------------------------------------

  function renderClaims(v, ax) {
    const body = $('claims-body');
    const meta = $('claims-meta');
    if (!S.repo) {
      meta.replaceChildren();
      body.replaceChildren(connectHelp(true));
      return;
    }
    if (!v) {
      meta.replaceChildren();
      body.replaceChildren(el('p', { class: 'loading' }, 'Loading ', code(S.repo), '…'));
      return;
    }
    const active = v.claims.filter((c) => c.active).length;
    const idle = v.claims.length - active;
    meta.replaceChildren(
      el('span', { class: 'meta-strong' }, active + ' active'),
      idle ? ' · ' + idle + ' not running' : '',
      ' · ' + v.live_sessions + ' live session' + (v.live_sessions === 1 ? '' : 's'),
      ' · as of ', asOfNode(v.at));
    if (!v.claims.length) {
      body.replaceChildren(connectHelp(false));
      return;
    }
    const list = el('ol', { class: 'claim-list' });
    for (const c of v.claims) list.appendChild(el('li', null, claimCard(c, ax)));
    body.replaceChildren(list);
  }

  function asOfNode(at) {
    const n = timeNode(at, 'ago', 'Board read at');
    if (!n) return 'now';
    n.id = 'as-of';
    return n;
  }

  function connectHelp(noRepos) {
    const origin = location.origin;
    return el('div', { class: 'empty' },
      el('h3', null, noRepos ? 'No agents have reported to this server yet' : 'Nobody is working in this repository right now'),
      el('p', null, noRepos
        ? 'Each agent session that starts in an enrolled repository opens a claim here within a second: who, which branch, which files, and what it is about to do.'
        : ['Claims appear when an agent starts in a worktree of ', code(S.repo),
          ', and go away once their branch is merged and their sessions end.']),
      el('ol', { class: 'steps' },
        el('li', null, 'Save your token for this server: ', code('intagent login --url ' + origin)),
        el('li', null, 'In the repository, run ', code('intagent init'), ' once to enrol it for Claude Code, Codex, Cursor, Copilot CLI and Gemini CLI.'),
        el('li', null, 'Start Claude Code there. Its session appears on this board as soon as it starts.')),
      el('p', { class: 'muted' }, 'Run ', code('intagent doctor'), ' in the repository if nothing shows up.'));
  }

  const SEV_ORDER = { block: 2, overlap: 1 };

  function claimCard(c, ax) {
    const cid = 'cl-' + c.id.replace(/[^A-Za-z0-9_-]/g, '_');
    const card = el('article', { class: 'claim ' + (c.active ? 'is-active' : 'is-idle'), 'aria-labelledby': cid + '-t' });

    const badges = el('div', { class: 'claim-badges' },
      c.active
        ? el('span', { class: 'badge badge-run', title: 'At least one agent session is working or waiting' }, icon('dot'), 'Running')
        : el('span', { class: 'badge badge-idle', title: 'No live session. Its changed files still warn others; its exclusive intents no longer block.' }, icon('ring'), 'Not running'),
      c.pending > 0
        ? el('span', { class: 'badge badge-inbox', title: c.pending + ' note' + (c.pending === 1 ? '' : 's') + ' or alert' + (c.pending === 1 ? '' : 's') + ' waiting for this claim’s next agent hook' },
          icon('mail'), c.pending + ' in inbox')
        : null);

    const head = el('header', { class: 'claim-head' },
      avatar(c.member),
      el('div', { class: 'claim-id' },
        el('h3', { id: cid + '-t' },
          el('span', { class: 'member' }, c.member),
          el('span', { class: 'branch' + (c.branch ? '' : ' muted') }, icon('branch'), c.branch || 'no branch')),
        el('p', { class: 'where', title: c.host + ':' + c.worktree }, c.host, el('span', { class: 'sep' }, ':'), c.worktree)),
      badges);

    const task = c.task
      ? el('p', { class: 'task' }, el('q', null, c.task))
      : el('p', { class: 'task task-none' }, 'No task summary yet');

    const body = el('div', { class: 'claim-body' },
      el('div', { class: 'claim-col' }, sessionsBlock(c), intentsBlock(c)),
      el('div', { class: 'claim-col' }, filesBlock(c, ax)));

    const foot = el('p', { class: 'claim-foot' },
      'opened ', timeNode(c.created_at, 'ago', 'Opened') || '?',
      ' · updated ', timeNode(c.updated_at, 'ago', 'Updated') || '?',
      ' · ', el('span', { class: 'mono' }, c.id));

    card.append(head, task, body, foot);
    return card;
  }

  function blockHead(label, count) {
    return el('h4', { class: 'block-h' }, label, count != null ? el('span', { class: 'count' }, String(count)) : null);
  }

  function sessionsBlock(c) {
    const box = el('div', { class: 'block' }, blockHead('Agents', c.sessions.length));
    if (!c.sessions.length) {
      box.appendChild(el('p', { class: 'none' }, 'No sessions attached'));
      return box;
    }
    const ul = el('ul', { class: 'sessions' });
    for (const s of c.sessions) ul.appendChild(el('li', null, sessionPill(s)));
    box.appendChild(ul);
    return box;
  }

  function sessionPill(s) {
    const state = STATES[s.state] ? s.state : 'ended';
    const meta = STATES[state];
    const started = tsOf(s.started_at);
    return el('div', {
      class: 'sess st-' + state,
      title: 'Session ' + String(s.id || '') + (Number.isFinite(started) ? ' · started ' + absTime(started) : '') + ' · ' + meta.hint,
    },
    el('span', { class: 'sess-state' }, icon(meta.icon), String(s.state || state)),
    el('span', { class: 'sess-agent' }, agentName(s.agent)),
    s.tool
      ? el('span', { class: 'sess-tool' }, el('span', { class: 'sr-only' }, 'running '), code(String(s.tool)),
        s.tool_since ? [' ', timeNode(s.tool_since, 'dur', 'In this tool since')] : null)
      : null,
    el('span', { class: 'sess-seen' }, timeNode(s.last_seen, 'seen', 'Last event at')),
    stopsLine(s));
  }

  // What stopped this session, from the feed: a team lead sees who is blocked
  // and by whose reservation without reading the whole feed.
  function stopsLine(s) {
    const mine = [...S.feed.values()].filter((a) => a && a.kind === 'conflict' && a.session && a.session === s.id)
      .sort((x, y) => (y.seq || 0) - (x.seq || 0));
    const refused = mine.filter((a) => a.decision === 'deny' || a.decision === 'ask');
    const last = refused[0] || mine.find((a) => a.breach); // warnings do not stop anyone
    if (!last) return null;
    const holder = (/→ (\S+) \(/.exec(String(last.text || '')) || [])[1] || '';
    const path = arr(last.paths)[0] || '';
    const what = refused.length
      ? conflictOutcome(last).key + (refused.length > 1 ? ' ' + refused.length + '×' : '')
      : 'changed a reserved file';
    return el('span', { class: 'sess-stops' + (refused.length ? '' : ' unchecked'), title: String(last.text || '') },
      icon(refused.length ? 'deny' : 'warn'), what, path ? [' · ', pathNode(path)] : '',
      holder ? ' (' + holder + "'s)" : '', ' · ', timeNode(last.at, 'ago', 'At'));
  }

  function intentsBlock(c) {
    const box = el('div', { class: 'block' }, blockHead('Intents', c.intents.length));
    if (!c.intents.length) {
      box.appendChild(el('p', { class: 'none' }, 'None declared'));
      return box;
    }
    const ul = el('ul', { class: 'intents' });
    const summaries = new Set();
    for (const it of c.intents) {
      const excl = it.mode === 'exclusive';
      const paused = excl && !c.active;
      const title = (excl
        ? (paused ? 'Exclusive, but not enforced while no agent of this claim is running' : 'Exclusive: other agents are refused while this claim is active')
        : 'Shared: other agents are told, not stopped') + (it.summary ? ' · ' + it.summary : '');
      const at = tsOf(it.declared_at);
      ul.appendChild(el('li', {
        class: 'intent ' + (excl ? 'is-excl' : 'is-shared') + (paused ? ' is-paused' : ''),
        title: title + (Number.isFinite(at) ? ' · declared ' + absTime(at) : ''),
      },
      icon(excl ? 'lock' : 'shared'),
      code(it.pattern, 'intent-pat'),
      el('span', { class: 'intent-mode' }, excl ? (paused ? 'exclusive · paused' : 'exclusive') : 'shared')));
      if (it.summary && it.summary !== c.task) summaries.add(it.summary);
    }
    box.appendChild(ul);
    for (const s of summaries) box.appendChild(el('p', { class: 'intent-sum' }, el('q', null, s)));
    return box;
  }

  function filesBlock(c, ax) {
    const box = el('div', { class: 'block' }, blockHead('Changed files', c.fileCount + (c.truncated ? '+' : '')));
    if (!c.files.length) {
      box.appendChild(el('p', { class: 'none' }, 'No changed files yet'));
      return box;
    }
    const rows = c.files.map((f, i) => {
      const spot = ax.files.get(f.path);
      const others = spot ? Array.from(spot.involved.values()).filter((r) => r.claim.id !== c.id) : [];
      const sev = others.length ? (others.some(reserves) ? 'block' : 'overlap') : '';
      return { f, i, others, sev };
    });
    rows.sort((a, b) => (SEV_ORDER[b.sev] || 0) - (SEV_ORDER[a.sev] || 0) || a.i - b.i);
    const hot = rows.filter((r) => r.sev).length;
    if (hot) {
      box.firstChild.appendChild(el('span', { class: 'count count-hot', title: 'Files another claim also changed or holds an intent on' },
        icon('overlap'), hot + ' overlapping'));
    }
    const open = S.expanded.has(c.id);
    const shown = open ? rows : rows.slice(0, FILES_SHOWN);
    const listId = 'files-' + c.id.replace(/[^A-Za-z0-9_-]/g, '_');
    const ul = el('ul', { class: 'files', id: listId });
    for (const r of shown) ul.appendChild(fileRow(r));
    box.appendChild(ul);
    if (rows.length > FILES_SHOWN) {
      box.appendChild(el('button', {
        type: 'button', class: 'btn btn-link more', 'aria-expanded': open ? 'true' : 'false', 'aria-controls': listId,
        data: { focus: 'files:' + c.id },
        on: { click: () => { if (S.expanded.has(c.id)) S.expanded.delete(c.id); else S.expanded.add(c.id); renderBoard(); } },
      }, icon('chevron', open ? 'flip' : ''), open ? 'Show fewer' : 'Show all ' + rows.length + ' files'));
    }
    if (c.fileCount > c.files.length) {
      box.appendChild(el('p', { class: 'none' }, 'Listing ' + c.files.length + ' of ' + c.fileCount +
        ' files: the most recent, and any a teammate also changed.'));
    }
    if (c.truncated) {
      box.appendChild(el('p', { class: 'none' }, 'Git reported more changed files than intagent keeps per claim.'));
    }
    return box;
  }

  function fileRow(r) {
    const f = r.f;
    const li = el('li', { class: 'file' + (r.sev ? ' is-hot sev-' + r.sev : ''), title: f.area ? 'area ' + f.area : null },
      el('span', { class: 'file-mark' }, r.sev ? icon(r.sev === 'block' ? 'deny' : 'overlap') : null),
      el('span', { class: 'file-main' },
        pathNode(f.path),
        r.others.length
          ? el('span', { class: 'also' }, el('span', { class: 'also-k' }, r.sev === 'block' ? 'Reserved · ' : 'Also · '),
            joinNodes(r.others.map((o) => el('span', { class: 'also-who', title: roleTitle(o) }, alsoNodes(o, f.path))), ' · '))
          : null),
      el('span', { class: 'file-meta' },
        f.from_git ? el('span', { class: 'tag', title: 'Found by reconciling with git, not reported by a hook' }, 'git') : null,
        timeNode(f.at, 'ago', 'Changed')));
    return li;
  }

  // "carol changed it, plans libs/money/**": what another claim has to do with a file.
  function alsoNodes(o, path) {
    const parts = [];
    if (o.changed) parts.push('changed it');
    if (o.stopped) parts.push(STOPPED[o.stopped] + (o.stopped === 'asked' ? ' about it' : ' at it'));
    for (const i of o.intents) {
      const verb = i.mode === 'exclusive' ? (o.claim.active ? 'reserves ' : 'reserved, not running, ') : 'plans ';
      parts.push(i.pattern === path ? verb + 'it' : [verb, code(i.pattern, 'path')]);
    }
    return [el('strong', null, o.claim.member), ' ', joinNodes(parts, ', ')];
  }

  function joinNodes(nodes, sep) {
    const out = [];
    nodes.forEach((x, i) => { if (i) out.push(sep); out.push(x); });
    return out;
  }

  // --- hot spots -----------------------------------------------------------------

  function renderHot(v, ax) {
    const body = $('hot-body');
    const meta = $('hot-meta');
    if (!v) {
      meta.replaceChildren();
      body.replaceChildren(el('p', { class: 'empty-mini' }, S.repo ? 'Loading…' : 'Files and areas that more than one claim touches appear here.'));
      return;
    }
    const files = Array.from(ax.files.values()).sort((a, b) =>
      (SEV_ORDER[b.severity] || 0) - (SEV_ORDER[a.severity] || 0) || b.involved.size - a.involved.size || (a.path < b.path ? -1 : 1));
    const areas = ax.areas;
    meta.replaceChildren(files.length + ' file' + (files.length === 1 ? '' : 's') + ' · ' + areas.length + ' area' + (areas.length === 1 ? '' : 's'));
    if (!files.length && !areas.length) {
      body.replaceChildren(el('p', { class: 'empty-mini' },
        'No overlaps. When two claims change the same file, or one changes a file another has declared, it shows up here before it shows up in a merge conflict.'));
      return;
    }
    const out = [];
    if (files.length) {
      const shown = S.hotExpanded ? files : files.slice(0, HOT_SHOWN);
      const ul = el('ul', { class: 'spots', id: 'hot-files' });
      for (const s of shown) {
        ul.appendChild(el('li', { class: 'spot sev-' + s.severity },
          el('span', { class: 'spot-mark', title: s.severity === 'block' ? 'Reserved by an active exclusive intent and changed by another claim' : 'Changed or declared by more than one claim' },
            icon(s.severity === 'block' ? 'deny' : 'overlap'),
            el('span', { class: 'sr-only' }, s.severity === 'block' ? 'reserved' : 'overlap')),
          el('div', { class: 'spot-main' }, pathNode(s.path), whoList(s.involved, roleText))));
      }
      out.push(el('h3', { class: 'sub-h' }, 'Files'), ul);
      if (files.length > HOT_SHOWN) {
        out.push(el('button', {
          type: 'button', class: 'btn btn-link more', 'aria-expanded': S.hotExpanded ? 'true' : 'false', 'aria-controls': 'hot-files',
          data: { focus: 'hot' },
          on: { click: () => { S.hotExpanded = !S.hotExpanded; renderBoard(); } },
        }, icon('chevron', S.hotExpanded ? 'flip' : ''), S.hotExpanded ? 'Show fewer' : 'Show all ' + files.length));
      }
    }
    if (areas.length) {
      const ul = el('ul', { class: 'spots' });
      for (const a of areas) {
        ul.appendChild(el('li', { class: 'spot sev-nearby' },
          el('span', { class: 'spot-mark', title: 'More than one claim works in this area' }, icon('nearby'),
            el('span', { class: 'sr-only' }, 'same area')),
          el('div', { class: 'spot-main' }, code(a.area, 'path area'),
            whoList(a.involved, (r) => r.files + ' file' + (r.files === 1 ? '' : 's')))));
      }
      out.push(el('h3', { class: 'sub-h' }, 'Areas'), ul);
    }
    body.replaceChildren(...out);
  }

  function whoList(involved, describe) {
    const ul = el('ul', { class: 'who-list' });
    const seen = new Map();
    for (const r of involved.values()) seen.set(r.claim.member, (seen.get(r.claim.member) || 0) + 1);
    for (const r of involved.values()) {
      const c = r.claim;
      // The branch only tells apart two claims of the same member; the title has the rest.
      ul.appendChild(el('li', { class: 'who-chip' + (c.active ? '' : ' is-idle'), title: roleTitle(r) },
        avatar(c.member, 'avatar-xs'),
        el('span', { class: 'who-m' }, c.member),
        c.branch && seen.get(c.member) > 1 ? el('span', { class: 'who-b' }, c.branch) : null,
        el('span', { class: 'who-r' }, describe(r))));
    }
    return ul;
  }

  // --- activity feed ------------------------------------------------------------------

  function mergeFeed(acts, fresh) {
    let added = false;
    for (const a of acts) {
      if (!a || typeof a !== 'object' || !Number.isFinite(a.seq)) continue;
      if (a.repo && a.repo !== S.repo) continue;
      if (S.feed.has(a.seq)) continue;
      S.feed.set(a.seq, a);
      if (fresh) S.fresh.add(a.seq);
      added = true;
    }
    if (S.feed.size > FEED_CAP) {
      const seqs = Array.from(S.feed.keys()).sort((x, y) => x - y);
      for (const s of seqs.slice(0, S.feed.size - FEED_CAP)) S.feed.delete(s);
    }
    if (added) scheduleFeed();
  }

  // A busy repository has tens of events a second, and each redraw rebuilds
  // the whole list: events arriving close together are drawn together.
  function scheduleFeed() {
    if (S.feedTimer) return;
    const wait = Math.min(FEED_REDRAW_MS, Math.max(0, S.feedAt + FEED_REDRAW_MS - tick()));
    S.feedTimer = setTimeout(() => { S.feedTimer = 0; renderFeed(); }, wait);
  }

  function renderFeed() {
    S.feedAt = tick();
    const list = $('feed-body');
    const empty = $('feed-empty');
    const items = Array.from(S.feed.values())
      .filter((a) => S.filter === 'all' || a.kind === 'conflict')
      .sort((x, y) => y.seq - x.seq);
    list.replaceChildren(...items.map(feedItem));
    list.hidden = !items.length;
    S.fresh.clear();
    if (items.length) {
      empty.hidden = true;
      return;
    }
    empty.hidden = false;
    empty.className = 'empty-mini';
    empty.replaceChildren(!S.repo
      ? 'Activity appears here as it happens: sessions starting and stopping, files changing, intents, notes and every conflict intagent catches.'
      : S.filter === 'conflicts'
        ? 'No conflicts yet. Every edit intagent refuses, bumps, asks about or warns about appears here.'
        : 'Waiting for activity. Sessions starting and stopping, files changing, intents, notes and conflicts appear here as they happen.');
  }

  const KIND_ICON = {
    'claim.opened': 'flag', 'claim.released': 'check', 'claim.forgotten': 'clock',
    'session.started': 'play', 'session.ended': 'stop', 'session.stalled': 'warn', 'session.gone': 'xcircle',
    'session.recovered': 'recover', 'file.changed': 'pencil', 'footprint.reconciled': 'sync',
    'intent.declared': 'lock', 'intent.released': 'unlock', 'note.sent': 'note',
  };

  function feedItem(a) {
    const kind = String(a.kind || '');
    const sev = kind === 'conflict' ? String(a.severity || 'none') : '';
    let ic = KIND_ICON[kind] || 'dot';
    let tag = null;
    if (kind === 'conflict') {
      const o = conflictOutcome(a);
      ic = o.icon;
      tag = el('span', { class: 'act-tag tag-' + o.key }, o.label);
    } else if (kind === 'intent.declared' && /^shared:/.test(String(a.text || ''))) {
      ic = 'shared';
    }
    const cls = ['act', 'k-' + kind.replace(/[^a-z.]/g, '').replace(/\./g, '-')];
    if (sev) cls.push('sev-' + sev.replace(/[^a-z]/g, ''));
    if (S.fresh.has(a.seq)) cls.push('is-fresh');
    return el('li', { class: cls.join(' ') },
      el('span', { class: 'act-ic' }, icon(ic)),
      el('p', { class: 'act-text' }, tag, sentence(a)),
      el('span', { class: 'act-time' }, timeNode(a.at, 'ago')));
  }

  const strong = (s) => el('strong', null, s);

  function actor(a) {
    return [strong(String(a.member || 'someone')), '’s ', a.agent ? agentName(String(a.agent)) + ' agent' : 'agent'];
  }

  function pathList(paths, max) {
    const ps = arr(paths).map(String);
    if (!ps.length) return 'files';
    const lim = max || 2;
    const out = joinNodes(ps.slice(0, lim).map(pathNode), ', ');
    if (ps.length > lim) out.push(' and ' + (ps.length - lim) + ' more');
    return out;
  }

  function onBranch(a) {
    return a.text ? [' on ', code(String(a.text), 'branch-c')] : '';
  }

  function sentence(a) {
    const m = strong(String(a.member || 'someone'));
    const text = a.text ? String(a.text) : '';
    switch (a.kind) {
      case 'claim.opened': return [m, ' opened a claim', onBranch(a)];
      case 'claim.released': return [m, '’s claim', onBranch(a), ' was released: nothing left to track'];
      case 'claim.forgotten': return [m, '’s claim', onBranch(a), ' was forgotten after a long time without activity'];
      case 'session.started': return [actor(a), ' started a session'];
      case 'session.ended': return [actor(a), ' ended its session'];
      case 'session.stalled': return [actor(a), ' has stalled', text ? ': ' + text : ''];
      case 'session.gone': return [actor(a), ' is gone', text ? ': ' + text : '', '. Its files still count; its exclusive intents stop blocking.'];
      case 'session.recovered': return [actor(a), ' is responding again'];
      case 'file.changed': return [actor(a), ' changed ', pathList(a.paths)];
      case 'footprint.reconciled': return [m, '’s changes were reconciled with git: ', text];
      case 'intent.declared': {
        const mm = /^(exclusive|shared): ?(.*)$/.exec(text);
        const mode = mm ? mm[1] : '';
        const why = mm ? mm[2] : text;
        return [m, mode === 'exclusive' ? ' reserved ' : ' plans to change ', pathList(a.paths, 3),
          mode ? el('span', { class: 'mode-' + mode }, ' (' + mode + ')') : '',
          why ? [': ', el('q', null, why)] : ''];
      }
      case 'intent.released': return [m, ' released ', pathList(a.paths, 3)];
      case 'note.sent': {
        // The server names the path a note went to; activities saved before
        // it did are told apart by a slash.
        const path = arr(a.paths)[0];
        const prefix = path ? 'to whoever works on ' + path + ': ' : '';
        if (path && text.startsWith(prefix)) {
          return [m, ' sent a note to whoever works on ', pathNode(String(path)), ': ', el('q', null, text.slice(prefix.length))];
        }
        const mm = /^to ([^:]+): ([\s\S]*)$/.exec(text);
        if (!mm) return [m, ' sent a note: ', el('q', null, text)];
        return mm[1].includes('/')
          ? [m, ' sent a note to whoever works on ', pathNode(mm[1]), ': ', el('q', null, mm[2])]
          : [m, ' sent ', strong(mm[1]), ' a note: ', el('q', null, mm[2])];
      }
      case 'conflict': return conflictSentence(a);
      default: return [m, ' ', code(String(a.kind || 'event')), text ? ': ' + text : ''];
    }
  }

  function policyAction(sev) {
    const p = (S.view && S.view.policy) || S.policy || {};
    return p[sev] || { block: 'deny', overlap: 'bump', nearby: 'warn' }[sev] || '';
  }

  // What the agent was told: the activity's decision, read with the policy's
  // action for its severity (deny is both a refusal and a bump).
  function conflictOutcome(a) {
    if (a.breach) return { key: 'breach', label: 'Unchecked', icon: 'deny', verb: ' changed, without a check, ' };
    const action = policyAction(String(a.severity || ''));
    switch (a.decision) {
      case 'deny':
        return action === 'deny'
          ? { key: 'refused', label: 'Refused', icon: 'deny', verb: ' was refused ' }
          : { key: 'bumped', label: 'Bumped', icon: 'overlap', verb: ' was bumped at ' };
      case 'ask':
        return { key: 'asked', label: 'Asked', icon: 'ask', verb: ' had to ask its person before editing ' };
      default:
        return { key: 'warned', label: 'Warned', icon: 'nearby', verb: ' was warned about ' };
    }
  }

  // The server writes "path → member (why)".
  function parseConflictText(t) {
    const i = t.indexOf(' → ');
    if (i < 0) return null;
    const path = t.slice(0, i);
    const rest = t.slice(i + 3);
    const j = rest.indexOf(' (');
    if (j < 0 || !rest.endsWith(')')) return { path, member: rest, why: '' };
    return { path, member: rest.slice(0, j), why: rest.slice(j + 2, -1) };
  }

  function reasonNodes(member, why, path) {
    const who = strong(member);
    let m = /^declared (exclusive|shared) intent (\S+)(?:: ([\s\S]*))?$/.exec(why);
    if (m) {
      const what = m[2] === path ? 'it' : pathNode(m[2]);
      return m[1] === 'exclusive'
        ? [who, ' holds ', what, ' exclusively']
        : [who, ' plans to change ', what];
    }
    if (why === 'has unmerged changes to this file') return [who, ' has unmerged changes to it'];
    m = /^had unmerged changes to this file \(claim dormant for (.+)\)$/.exec(why);
    if (m) return [who, ' changed it before going quiet ', m[1], ' ago'];
    m = /^is working in the same area (.+)$/.exec(why);
    if (m) return [who, ' is working in ', code(m[1], 'path')];
    if (why === 'another live session in this same worktree changed this file') return ['another live session in ', who, '’s same worktree changed it'];
    return [who, why ? ' ' + why : ''];
  }

  function conflictSentence(a) {
    const o = conflictOutcome(a);
    const parsed = parseConflictText(String(a.text || ''));
    const paths = arr(a.paths).map(String);
    const path = parsed ? parsed.path : paths[0] || 'a file';
    const out = [actor(a), o.verb, pathNode(path)];
    if (paths.length > 1) out.push(' and ' + (paths.length - 1) + ' more');
    if (parsed) out.push(' (', reasonNodes(parsed.member, parsed.why, parsed.path), ')');
    else if (a.text) out.push(': ' + String(a.text));
    for (const x of arr(a.also)) out.push('; also ', String(x));
    if (o.key === 'bumped') out.push('. A retry goes through.');
    return out;
  }

  // --- wiring -----------------------------------------------------------------------

  function wire() {
    $('repo').addEventListener('change', (e) => selectRepo(e.target.value, true));
    $('signin-btn').addEventListener('click', () => { S.loginFailed = false; showLogin(true); });
    $('login-cancel').addEventListener('click', () => { S.loginFailed = false; showBoard(); });
    const saved = storeGet(FILTER_KEY);
    if (saved === 'conflicts') S.filter = 'conflicts';
    for (const b of document.querySelectorAll('.seg-btn')) {
      b.setAttribute('aria-pressed', b.dataset.filter === S.filter ? 'true' : 'false');
      b.addEventListener('click', () => {
        S.filter = b.dataset.filter === 'conflicts' ? 'conflicts' : 'all';
        storeSet(FILTER_KEY, S.filter);
        for (const o of document.querySelectorAll('.seg-btn')) o.setAttribute('aria-pressed', o === b ? 'true' : 'false');
        renderFeed();
      });
    }
    // A hidden tab gives its stream back: browsers allow six connections to a
    // server over HTTP/1.1, and every open dashboard tab would hold one.
    document.addEventListener('visibilitychange', () => {
      if ($('board').hidden) return;
      if (document.hidden) {
        clearTimeout(S.reconnectTimer);
        if (S.es) { S.es.close(); S.es = null; }
        return;
      }
      if (!S.es) connectStream();
      if (S.repo) { refreshBoard(); updateTimes(); }
    });
  }

  async function boot(attempt) {
    let me;
    try {
      me = await getJSON('/v1/whoami');
    } catch (e) {
      if (e instanceof AuthError) { showLogin(false); return; }
      showUnreachable(e.message);
      setTimeout(() => boot((attempt || 0) + 1), Math.min(30000, 2000 * Math.pow(2, attempt || 0)));
      return;
    }
    S.me = me && typeof me.member === 'string' ? me.member : '';
    S.demo = !!(me && me.demo);
    S.version = me && typeof me.version === 'string' ? me.version : '';
    S.policy = me && me.policy && typeof me.policy === 'object' ? me.policy : null;
    if (S.loginFailed) { showLogin(true); return; }
    showBoard();
  }

  function start() {
    const q = new URLSearchParams(location.search);
    if (q.get('login') === 'failed') {
      S.loginFailed = true;
      try { history.replaceState(null, '', location.pathname); } catch (_) { /* cosmetic */ }
    }
    wire();
    boot(0);
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', start);
  else start();
})();
