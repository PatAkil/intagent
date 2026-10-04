// Drives the dashboard's board reloads and feed redraws without a browser,
// on fake clocks and timers: node pacing.js app.js. It prints what went wrong
// and exits 1 when a reload or a redraw waits past its bound.
//
// The page reads two clocks: Date.now(), the wall clock, which a time sync
// can step either way, and performance.now(), which only moves forward. A
// laptop that sleeps moves both, or only the wall clock; that depends on the
// system, so each case below says which one it moves.
'use strict';

const fs = require('fs');
const vm = require('vm');

const REFRESH_MS = 15000; // app.js's poll, the longest a reload may wait
const RELOAD_MAX_MS = 1500; // RELOAD_GAP_MS + RELOAD_JITTER_MS
const FEED_REDRAW_MS = 250;
const HOUR = 3600000;

const failures = [];
const fail = (msg) => failures.push(msg);

const source = fs.readFileSync(process.argv[2], 'utf8');
const anchor = "  if (document.readyState === 'loading')";
if (!source.includes(anchor)) {
  console.error('pacing.js: app.js no longer has the line it hooks in before: ' + anchor.trim());
  process.exit(2);
}
const hooked = source.replace(anchor,
  '  globalThis.__app = { S, refreshBoard, startLive, connectStream };\n' + anchor);

// A stand-in for any element: it takes whatever the page sets and counts the
// redraws of each.
class Node {
  constructor() {
    this.hidden = false;
    this.dataset = {};
    this.style = {};
    this.classList = { add() {}, remove() {}, toggle() {}, contains: () => false };
    this.redraws = 0;
  }
  replaceChildren() { this.redraws++; }
  append() {}
  appendChild() {}
  setAttribute() {}
  removeAttribute() {}
  addEventListener() {}
  querySelectorAll() { return []; }
}

// page loads app.js into a fresh realm with its own clocks (p.mono for
// performance.now(), p.wall for Date.now()) and timers. Every board request
// is answered 304 Not Modified, once p.boardDelay settles if it is set.
function page() {
  const p = { mono: 1000, wall: 1.7e12, boards: [], timers: [], nextId: 1, nodes: new Map(), boardDelay: null };
  const sched = (fn, ms, every) => {
    const id = p.nextId++;
    p.timers.push({ id, due: p.mono + Math.max(0, Number(ms) || 0), fn, every });
    return id;
  };
  const cancel = (id) => { p.timers = p.timers.filter((t) => t.id !== id); };
  const RealDate = Date;
  const ctx = {
    console,
    URLSearchParams,
    Date: class extends RealDate {
      constructor(...a) { if (a.length) super(...a); else super(p.wall); }
      static now() { return p.wall; }
    },
    performance: { now: () => p.mono },
    setTimeout: (fn, ms) => sched(fn, ms, 0),
    setInterval: (fn, ms) => sched(fn, ms, ms),
    clearTimeout: cancel,
    clearInterval: cancel,
    location: { host: 'test', origin: 'http://test', search: '', pathname: '/' },
    window: {},
    EventSource: class {
      constructor() { this.handlers = {}; p.stream = this; }
      addEventListener(kind, fn) { this.handlers[kind] = fn; }
      close() {}
    },
    document: {
      readyState: 'complete',
      hidden: false,
      getElementById: (id) => {
        if (!p.nodes.has(id)) p.nodes.set(id, new Node());
        return p.nodes.get(id);
      },
      createElement: () => new Node(),
      createElementNS: () => new Node(),
      createTextNode: () => new Node(),
      querySelectorAll: () => [],
      addEventListener() {},
    },
    fetch: async (url) => {
      if (String(url).startsWith('/v1/board')) {
        p.boards.push(p.mono);
        if (p.boardDelay) await p.boardDelay;
        return { status: 304, ok: false, headers: { get: () => '' }, json: async () => ({}) };
      }
      return new Promise(() => {}); // every other request stays open: only the board matters here
    },
  };
  ctx.EventSource.CLOSED = 2;
  vm.createContext(ctx);
  // The page boots by asking who is signed in; that request never answers,
  // so the page waits there and the hooks below drive it instead.
  vm.runInContext(hooked, ctx);
  p.app = ctx.__app;
  p.app.S.repo = 'r';
  p.event = (seq) => p.stream.handlers.activity({
    data: JSON.stringify({ seq, repo: 'r', kind: 'note.sent', member: 'bo', text: 'hi', at: new RealDate(p.wall).toISOString() }),
  });
  // run moves both clocks forward by ms, firing the timers that fall due on
  // the way, in order, and letting the page's promises settle after each.
  p.run = async (ms) => {
    const end = p.mono + ms;
    for (;;) {
      await new Promise(setImmediate);
      const due = p.timers.filter((t) => t.due <= end).sort((a, b) => a.due - b.due || a.id - b.id)[0];
      if (!due) break;
      const step = Math.max(0, due.due - p.mono); // overdue timers fire now
      p.wall += step;
      p.mono += step;
      if (due.every) due.due = p.mono + due.every;
      else cancel(due.id);
      due.fn();
    }
    p.wall += end - p.mono;
    p.mono = end;
    await new Promise(setImmediate);
  };
  p.boardsSince = (t) => p.boards.filter((b) => b > t).length;
  return p;
}

const cases = {
  // A laptop sleeps for an hour in the middle of a board request, and both
  // clocks say the request took that hour. On waking, the page reloads the
  // board within the poll's 15 s, as it did before reloads were throttled.
  async 'a request that spans a sleep does not stop reloads'(p) {
    p.app.startLive();
    let wake;
    p.boardDelay = new Promise((r) => { wake = r; });
    p.app.refreshBoard();
    await p.run(0);
    p.mono += HOUR;
    p.wall += HOUR;
    const woke = p.mono;
    p.boardDelay = null;
    wake();
    await p.run(0);
    p.app.connectStream();
    p.event(1);
    await p.run(REFRESH_MS + 1000);
    if (!p.boardsSince(woke)) fail('no board reload in the ' + (REFRESH_MS + 1000) + ' ms after waking');
  },

  // The wall clock steps an hour forward while a request is out: the request
  // did not take an hour, and the next reload is as soon as ever.
  async 'a wall clock step forward does not stretch the gap'(p) {
    p.app.connectStream();
    let answer;
    p.boardDelay = new Promise((r) => { answer = r; });
    p.app.refreshBoard();
    await p.run(0);
    p.wall += HOUR;
    p.boardDelay = null;
    answer();
    await p.run(100);
    const from = p.mono;
    p.event(1);
    await p.run(RELOAD_MAX_MS + 100);
    if (!p.boardsSince(from)) fail('no board reload ' + (RELOAD_MAX_MS + 100) + ' ms after an event');
  },

  // The wall clock steps an hour back after a reload: neither the board nor
  // the feed waits for it to catch up.
  async 'a wall clock step back holds back neither the board nor the feed'(p) {
    p.app.connectStream();
    p.event(1);
    await p.run(RELOAD_MAX_MS + 100);
    const feed = p.nodes.get('feed-body');
    const drawn = feed ? feed.redraws : 0;
    if (!drawn) fail('the feed was not drawn after its first event');
    p.wall -= HOUR;
    const from = p.mono;
    p.event(2);
    await p.run(FEED_REDRAW_MS + 50);
    if (p.nodes.get('feed-body').redraws === drawn) fail('the feed was not redrawn ' + (FEED_REDRAW_MS + 50) + ' ms after an event');
    await p.run(RELOAD_MAX_MS);
    if (!p.boardsSince(from)) fail('no board reload ' + (RELOAD_MAX_MS + FEED_REDRAW_MS + 50) + ' ms after an event');
  },
};

(async () => {
  for (const [name, run] of Object.entries(cases)) {
    const before = failures.length;
    try {
      await run(page());
    } catch (e) {
      fail(e.stack || String(e));
    }
    for (const f of failures.slice(before)) console.log(name + ': ' + f);
  }
  process.exit(failures.length ? 1 : 0);
})();
