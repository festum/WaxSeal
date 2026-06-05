/*
 * WaxSeal browser shim (hand-written ES2020).
 *
 * QuickJS-on-wazero gives us a bare JS engine: quickjs-ng core already provides
 * atob/btoa/performance/queueMicrotask/WeakRef/BigInt, but not TextEncoder/
 * TextDecoder, timers, a browser identity, or CSPRNG-backed crypto. This shim
 * renders exactly that surface, all derived from the active BrowserProfile so
 * the identity BotGuard fingerprints is coherent (navigator/screen/timezone/UA
 * all agree), and wraps window/navigator/document in a Proxy "discovery trap"
 * that logs unshimmed property probes back to Go.
 *
 * The DOM fidelity layer (real prototype chains, native Function.toString,
 * canvas/WebGL/SVG/media interfaces, Date timezone coherence) lives in the
 * separately-tested ./dom.js; this file wires it to the profile. See dom.js for
 * the rationale behind behaviorally coherent stubs.
 *
 * Host bridges (registered by build/wasm/host.c, not wasm imports):
 *   __wx_random_fill(typedArray)  CSPRNG bytes (WASI random_get -> crypto/rand)
 *   __wx_random_double()          uniform double [0,1) with 53 CSPRNG bits
 *   __wx_console(level, message)  -> wazero stderr
 *
 * Networking is deliberately absent: WaxSeal does all HTTP in Go. The VM only
 * runs the BotGuard snapshot + minter.
 */
import {
  markNative, asNative, markClassNative,
  createDocument, installWindow, installDateTimezone
} from './dom.js';

(function () {
  'use strict';

  const G = globalThis;

  // Define a native-looking global function in one step.
  const defFn = (name, fn) => {
    asNative(fn, name);
    Object.defineProperty(G, name, { value: fn, configurable: true, writable: true });
    return fn;
  };

  // Console.
  const mklog = (level) => asNative(function () {
    let s = '';
    for (let i = 0; i < arguments.length; i++) {
      if (i) s += ' ';
      const a = arguments[i];
      try {
        s += typeof a === 'string' ? a : JSON.stringify(a);
      } catch (_) {
        s += String(a);
      }
    }
    __wx_console(level, s);
  }, '');
  G.console = {
    log: mklog(0), info: mklog(1), warn: mklog(2),
    error: mklog(3), debug: mklog(4), trace: mklog(4),
    dir: mklog(0), group: mklog(0), groupEnd: () => {}, assert: () => {}
  };

  // CSPRNG.
  // Keep all random sources backed by the host CSPRNG.
  Math.random = asNative(function random() {
    return __wx_random_double();
  }, 'random');
  // A real Crypto instance keeps `crypto instanceof Crypto` true. `subtle` is a
  // SubtleCrypto instance for the same reason.
  const subtleObj = Object.create(G.SubtleCrypto.prototype);
  Object.assign(subtleObj, {
    digest: asNative(function digest() { return Promise.resolve(new ArrayBuffer(32)); }, 'digest'),
    generateKey: asNative(function generateKey() { return Promise.resolve(Object.create(G.CryptoKey.prototype)); }, 'generateKey'),
    importKey: asNative(function importKey() { return Promise.resolve(Object.create(G.CryptoKey.prototype)); }, 'importKey'),
    exportKey: asNative(function exportKey() { return Promise.resolve(new ArrayBuffer(0)); }, 'exportKey'),
    encrypt: asNative(function encrypt() { return Promise.resolve(new ArrayBuffer(0)); }, 'encrypt'),
    decrypt: asNative(function decrypt() { return Promise.resolve(new ArrayBuffer(0)); }, 'decrypt'),
    sign: asNative(function sign() { return Promise.resolve(new ArrayBuffer(0)); }, 'sign'),
    verify: asNative(function verify() { return Promise.resolve(true); }, 'verify')
  });
  const cryptoObj = Object.create(G.Crypto.prototype);
  Object.assign(cryptoObj, {
    getRandomValues: asNative(function getRandomValues(arr) {
      if (arr == null || arr.buffer === undefined)
        throw new TypeError("getRandomValues expects an integer TypedArray");
      __wx_random_fill(arr);
      return arr;
    }, 'getRandomValues'),
    randomUUID: asNative(function randomUUID() {
      const b = new Uint8Array(16);
      __wx_random_fill(b);
      b[6] = (b[6] & 0x0f) | 0x40;
      b[8] = (b[8] & 0x3f) | 0x80;
      const h = [];
      for (let i = 0; i < 256; i++) h.push((i + 0x100).toString(16).slice(1));
      return (
        h[b[0]] + h[b[1]] + h[b[2]] + h[b[3]] + '-' +
        h[b[4]] + h[b[5]] + '-' + h[b[6]] + h[b[7]] + '-' +
        h[b[8]] + h[b[9]] + '-' +
        h[b[10]] + h[b[11]] + h[b[12]] + h[b[13]] + h[b[14]] + h[b[15]]
      );
    }, 'randomUUID'),
    subtle: subtleObj
  });
  // QuickJS has no `crypto`; some hosts (Node) ship a non-configurable one.
  // Prefer ours; tolerate a locked host global so the shim still loads there.
  try {
    Object.defineProperty(G, 'crypto', { value: cryptoObj, configurable: true, writable: false });
  } catch (_) { /* host crypto is locked (non-QuickJS); leave it */ }

  // TextEncoder / TextDecoder (UTF-8).
  if (typeof G.TextEncoder === 'undefined') {
    G.TextEncoder = class TextEncoder {
      get encoding() { return 'utf-8'; }
      encode(str) {
        str = String(str === undefined ? '' : str);
        const out = [];
        for (let i = 0; i < str.length; i++) {
          let c = str.charCodeAt(i);
          if (c >= 0xd800 && c <= 0xdbff && i + 1 < str.length) {
            const c2 = str.charCodeAt(i + 1);
            if (c2 >= 0xdc00 && c2 <= 0xdfff) {
              c = 0x10000 + ((c - 0xd800) << 10) + (c2 - 0xdc00);
              i++;
            }
          }
          if (c < 0x80) out.push(c);
          else if (c < 0x800) out.push(0xc0 | (c >> 6), 0x80 | (c & 0x3f));
          else if (c < 0x10000) out.push(0xe0 | (c >> 12), 0x80 | ((c >> 6) & 0x3f), 0x80 | (c & 0x3f));
          else out.push(0xf0 | (c >> 18), 0x80 | ((c >> 12) & 0x3f), 0x80 | ((c >> 6) & 0x3f), 0x80 | (c & 0x3f));
        }
        return new Uint8Array(out);
      }
      encodeInto(str, dest) {
        const enc = this.encode(str);
        const n = Math.min(enc.length, dest.length);
        dest.set(enc.subarray(0, n));
        return { read: str.length, written: n };
      }
    };
  }
  if (typeof G.TextDecoder === 'undefined') {
    G.TextDecoder = class TextDecoder {
      constructor(label) { this._enc = (label || 'utf-8').toLowerCase(); }
      get encoding() { return 'utf-8'; }
      decode(input) {
        if (input == null) return '';
        const bytes = input instanceof Uint8Array
          ? input
          : new Uint8Array(input.buffer || input);
        let out = '';
        let i = 0;
        while (i < bytes.length) {
          let c = bytes[i++];
          if (c < 0x80) { /* 1 byte */ }
          else if (c < 0xe0) c = ((c & 0x1f) << 6) | (bytes[i++] & 0x3f);
          else if (c < 0xf0) c = ((c & 0x0f) << 12) | ((bytes[i++] & 0x3f) << 6) | (bytes[i++] & 0x3f);
          else {
            c = ((c & 0x07) << 18) | ((bytes[i++] & 0x3f) << 12) | ((bytes[i++] & 0x3f) << 6) | (bytes[i++] & 0x3f);
          }
          if (c > 0xffff) {
            c -= 0x10000;
            out += String.fromCharCode(0xd800 + (c >> 10), 0xdc00 + (c & 0x3ff));
          } else {
            out += String.fromCharCode(c);
          }
        }
        return out;
      }
    };
  }
  markClassNative(G.TextEncoder);
  markClassNative(G.TextDecoder);

  // Virtual timer queue.
  // Go owns the real deadline. setTimeout enqueues against a virtual clock the
  // C pump only advances when otherwise idle (microtasks drained first), so a
  // synthetic Promise.race(snapshot, setTimeout(reject)) can never beat real
  // VM progress. Driven from C via __wx_runTimers().
  let timers = [];
  let timerSeq = 1;
  let vnow = 0;
  defFn('setTimeout', function setTimeout(fn, delay) {
    const id = timerSeq++;
    const args = Array.prototype.slice.call(arguments, 2);
    timers.push({ id, at: vnow + (Number(delay) || 0), fn, args });
    return id;
  });
  defFn('setInterval', function setInterval(fn, delay) {
    // Spike: model interval as a one-shot (BotGuard snapshot does not depend on
    // repeated intervals); avoids unbounded virtual-time loops.
    return G.setTimeout.apply(undefined, arguments);
  });
  defFn('clearTimeout', function clearTimeout(id) { timers = timers.filter((t) => t.id !== id); });
  defFn('clearInterval', function clearInterval(id) { timers = timers.filter((t) => t.id !== id); });
  defFn('setImmediate', function setImmediate(fn) { return G.setTimeout(fn, 0); });
  defFn('clearImmediate', function clearImmediate(id) { return G.clearTimeout(id); });
  G.queueMicrotask = G.queueMicrotask || asNative(function queueMicrotask(fn) { Promise.resolve().then(fn); }, 'queueMicrotask');
  // Returns true if a timer fired (and advanced the virtual clock).
  G.__wx_runTimers = function __wx_runTimers() {
    if (timers.length === 0) return false;
    let idx = 0;
    for (let i = 1; i < timers.length; i++)
      if (timers[i].at < timers[idx].at) idx = i;
    const t = timers.splice(idx, 1)[0];
    vnow = t.at;
    try { t.fn.apply(undefined, t.args); } catch (e) { console.error('timer threw: ' + e); }
    return true;
  };

  // Browser identity rendered from a BrowserProfile.
  const DEFAULT_PROFILE = {
    // Chrome-on-Windows, close to WaxTap's WEB profile. America/Phoenix stays at
    // UTC-7 year-round, which matches the shim's static Date offset. Mirrors
    // waxseal.DefaultProfile() in profile.go.
    userAgent: 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36',
    platform: 'Win32',
    language: 'en-US',
    languages: ['en-US', 'en'],
    vendor: 'Google Inc.',
    timezone: 'America/Phoenix',
    utcOffsetMinutes: -420,
    screen: [1920, 1080],
    userAgentData: {
      brands: [
        { brand: 'Google Chrome', version: '131' },
        { brand: 'Chromium', version: '131' },
        { brand: 'Not_A Brand', version: '24' }
      ],
      mobile: false,
      platform: 'Windows'
    }
  };

  // Discovery trap: log probes of unshimmed properties so we extend the shim
  // from the drift log. Two modes:
  //   __wxDiscovery=true  -> log the probe
  //   __wxAutoStub=true   -> (dev only) return a universal stub so BotGuard runs
  //                          to completion and reveals its probe set in one
  //                          pass, instead of stopping at the first miss.
  // Production sets both false: unknown reads return undefined / fail closed.
  G.__wxDiscovery = true;
  G.__wxAutoStub = false;
  const seenProbes = new Set();
  const ALLOW = new Set(['then', 'toJSON', 'constructor', 'valueOf', 'toString',
    Symbol.toPrimitive, Symbol.iterator, Symbol.toStringTag]);

  function logProbe(path) {
    if (G.__wxDiscovery && !seenProbes.has(path)) {
      seenProbes.add(path);
      console.warn('API-DRIFT probe: ' + path);
    }
  }

  // A universal stub: callable, constructable, any property yields another stub.
  // Coerces to 0/'' and is non-iterable / non-thenable so it doesn't derail
  // spreads or awaits. Discovery aid only; never production.
  function universalStub(path) {
    const target = function () {};
    return new Proxy(target, {
      get(t, prop) {
        if (prop === 'then' || prop === Symbol.iterator || prop === Symbol.asyncIterator)
          return undefined;
        if (prop === Symbol.toPrimitive) return () => 0;
        if (prop === Symbol.toStringTag) return undefined;
        if (prop === 'toString' || prop === 'valueOf')
          return () => (prop === 'toString' ? '[object Object]' : 0);
        if (prop === 'constructor') return target;
        if (typeof prop === 'symbol') return undefined;
        logProbe(path + '.' + String(prop));
        return universalStub(path + '.' + String(prop));
      },
      apply() { return universalStub(path + '()'); },
      construct() { return universalStub('new ' + path); },
      has() { return true; }
    });
  }

  function discoveryProxy(target, label) {
    return new Proxy(target, {
      get(t, prop, recv) {
        if (prop in t || typeof prop === 'symbol' || ALLOW.has(prop))
          return Reflect.get(t, prop, recv);
        logProbe(label + '.' + String(prop));
        return G.__wxAutoStub ? universalStub(label + '.' + String(prop)) : undefined;
      },
      has(t, prop) {
        if (G.__wxAutoStub && typeof prop === 'string') return true;
        return Reflect.has(t, prop);
      },
      set(t, prop, val, recv) { return Reflect.set(t, prop, val, recv); }
    });
  }

  // Define on globalThis with defineProperty: some hosts (e.g. Node) ship
  // getter-only navigator; QuickJS has none.
  const def = (name, value) =>
    Object.defineProperty(G, name, { value, configurable: true, writable: true });

  // Intl. This QuickJS build ships without Intl.
  // Browsers expose Intl, and BotGuard reads DateTimeFormat().resolvedOptions().
  // timeZone. When a real Intl exists (Node), wrap it to pin the resolved
  // timeZone; otherwise install a minimal, profile-coherent one.
  function makeMinimalIntl(prof) {
    const locale = prof.language || 'en-US';
    const TZ = prof.timezone || 'UTC';
    const resolved = (extra) => Object.assign({ locale, calendar: 'gregory', numberingSystem: 'latn' }, extra);
    const fmtDate = (d) => { const p = (n) => (n < 10 ? '0' : '') + n; return p(d.getMonth() + 1) + '/' + p(d.getDate()) + '/' + d.getFullYear(); };

    function DateTimeFormat(_locales, options) {
      const opts = options || {};
      const self = this instanceof DateTimeFormat ? this : Object.create(DateTimeFormat.prototype);
      self.resolvedOptions = asNative(function resolvedOptions() { return resolved({ timeZone: opts.timeZone || TZ, year: 'numeric', month: '2-digit', day: '2-digit' }); }, 'resolvedOptions');
      self.format = asNative(function format(d) { return fmtDate(d == null ? new Date() : new Date(d)); }, 'format');
      self.formatToParts = asNative(function formatToParts(d) { return [{ type: 'literal', value: fmtDate(d == null ? new Date() : new Date(d)) }]; }, 'formatToParts');
      return self;
    }
    function NumberFormat(_locales, _options) {
      const self = this instanceof NumberFormat ? this : Object.create(NumberFormat.prototype);
      self.resolvedOptions = asNative(function resolvedOptions() { return resolved({ style: 'decimal', notation: 'standard', minimumIntegerDigits: 1, useGrouping: 'auto' }); }, 'resolvedOptions');
      self.format = asNative(function format(n) { return String(n); }, 'format');
      self.formatToParts = asNative(function formatToParts(n) { return [{ type: 'integer', value: String(n) }]; }, 'formatToParts');
      return self;
    }
    function Collator() {
      const self = this instanceof Collator ? this : Object.create(Collator.prototype);
      self.compare = asNative(function compare(a, b) { return String(a) < String(b) ? -1 : String(a) > String(b) ? 1 : 0; }, 'compare');
      self.resolvedOptions = asNative(function resolvedOptions() { return resolved({ usage: 'sort', sensitivity: 'variant' }); }, 'resolvedOptions');
      return self;
    }
    function RelativeTimeFormat() {
      const self = this instanceof RelativeTimeFormat ? this : Object.create(RelativeTimeFormat.prototype);
      self.format = asNative(function format(n, unit) { return n + ' ' + unit; }, 'format');
      self.resolvedOptions = asNative(function resolvedOptions() { return resolved({ style: 'long', numeric: 'always' }); }, 'resolvedOptions');
      return self;
    }
    function PluralRules() {
      const self = this instanceof PluralRules ? this : Object.create(PluralRules.prototype);
      self.select = asNative(function select(n) { return n === 1 ? 'one' : 'other'; }, 'select');
      self.resolvedOptions = asNative(function resolvedOptions() { return resolved({ type: 'cardinal' }); }, 'resolvedOptions');
      return self;
    }
    function ListFormat() {
      const self = this instanceof ListFormat ? this : Object.create(ListFormat.prototype);
      self.format = asNative(function format(list) { return (list || []).join(', '); }, 'format');
      return self;
    }
    function Locale(tag) { const self = this instanceof Locale ? this : Object.create(Locale.prototype); self.baseName = String(tag || locale); self.language = self.baseName.split('-')[0]; self.region = self.baseName.split('-')[1] || ''; self.toString = asNative(function toString() { return self.baseName; }, 'toString'); return self; }
    const supportedLocalesOf = asNative(function supportedLocalesOf(l) { return Array.isArray(l) ? l.slice() : l ? [l] : []; }, 'supportedLocalesOf');
    [DateTimeFormat, NumberFormat, Collator, RelativeTimeFormat, PluralRules, ListFormat].forEach((C) => { asNative(C, C.name); C.supportedLocalesOf = supportedLocalesOf; });
    asNative(Locale, 'Locale');
    return {
      DateTimeFormat, NumberFormat, Collator, RelativeTimeFormat, PluralRules, ListFormat, Locale,
      getCanonicalLocales: asNative(function getCanonicalLocales(l) { return Array.isArray(l) ? l.slice() : [String(l)]; }, 'getCanonicalLocales'),
      supportedValuesOf: asNative(function supportedValuesOf(key) { return key === 'timeZone' ? [TZ] : key === 'calendar' ? ['gregory'] : []; }, 'supportedValuesOf')
    };
  }
  function installIntl(prof) {
    let hasReal = false;
    try { hasReal = typeof Intl !== 'undefined' && !!Intl.DateTimeFormat && !!new Intl.DateTimeFormat().resolvedOptions; } catch (_) { hasReal = false; }
    if (hasReal) {
      const orig = Intl.DateTimeFormat;
      const ResolvedTZ = prof.timezone;
      const wrapped = function (...a) { const inst = new orig(...a); const ro = inst.resolvedOptions.bind(inst); inst.resolvedOptions = () => Object.assign(ro(), { timeZone: ResolvedTZ }); return inst; };
      wrapped.prototype = orig.prototype; wrapped.supportedLocalesOf = orig.supportedLocalesOf;
      try { Intl.DateTimeFormat = wrapped; } catch (_) {}
      return;
    }
    def('Intl', makeMinimalIntl(prof));
  }

  // Best-effort `window instanceof Window` (structural; once, before profiles).
  installWindow(G);

  // performance: a real Performance instance (so `performance instanceof
  // Performance` holds), backed by the engine's high-res now() if present, else a
  // monotonic Date.now() fallback. Replaces any bare builtin so the singleton and
  // interface remain coherent.
  (function installPerformance() {
    let baseNow;
    try { baseNow = (typeof performance !== 'undefined' && typeof performance.now === 'function') ? performance.now.bind(performance) : null; } catch (_) { baseNow = null; }
    const t0 = Date.now();
    if (!baseNow) baseNow = () => Date.now() - t0;
    const perf = Object.create(G.Performance.prototype);
    perf.timeOrigin = t0;
    perf.now = asNative(function now() { return baseNow(); }, 'now');
    perf.mark = asNative(function mark() { return null; }, 'mark');
    perf.measure = asNative(function measure() { return null; }, 'measure');
    perf.clearMarks = asNative(function clearMarks() {}, 'clearMarks');
    perf.clearMeasures = asNative(function clearMeasures() {}, 'clearMeasures');
    perf.getEntries = asNative(function getEntries() { return []; }, 'getEntries');
    perf.getEntriesByName = asNative(function getEntriesByName() { return []; }, 'getEntriesByName');
    perf.getEntriesByType = asNative(function getEntriesByType() { return []; }, 'getEntriesByType');
    perf.toJSON = asNative(function toJSON() { return { timeOrigin: this.timeOrigin }; }, 'toJSON');
    def('performance', perf);
  })();

  let currentProfile = null;
  G.__wxApplyProfile = function __wxApplyProfile(p) {
    const prof = Object.assign({}, DEFAULT_PROFILE, p || {});
    currentProfile = prof;

    // navigator: a real Navigator instance (so `navigator instanceof Navigator`).
    const navBase = Object.create(G.Navigator.prototype);
    Object.assign(navBase, {
      userAgent: prof.userAgent,
      appVersion: prof.userAgent.replace(/^Mozilla\//, ''),
      appName: 'Netscape',
      appCodeName: 'Mozilla',
      platform: prof.platform,
      product: 'Gecko',
      productSub: '20030107',
      vendor: prof.vendor,
      vendorSub: '',
      language: prof.language,
      languages: Object.freeze(prof.languages.slice()),
      onLine: true,
      cookieEnabled: true,
      hardwareConcurrency: 8,
      deviceMemory: 8,
      maxTouchPoints: 0,
      webdriver: false,
      doNotTrack: null,
      pdfViewerEnabled: true,
      userAgentData: prof.userAgentData ? Object.assign(Object.create(G.NavigatorUAData.prototype), {
        brands: prof.userAgentData.brands.map((b) => Object.assign({}, b)),
        mobile: prof.userAgentData.mobile,
        platform: prof.userAgentData.platform,
        getHighEntropyValues: asNative(function getHighEntropyValues(hints) {
          const full = {
            brands: this.brands, mobile: this.mobile, platform: this.platform,
            platformVersion: '10.0.0', architecture: 'x86', bitness: '64',
            model: '', uaFullVersion: prof.userAgentData.brands[0].version + '.0.0.0',
            fullVersionList: this.brands
          };
          const out = { brands: this.brands, mobile: this.mobile, platform: this.platform };
          (hints || []).forEach((h) => { if (h in full) out[h] = full[h]; });
          return Promise.resolve(out);
        }, 'getHighEntropyValues'),
        toJSON() { return { brands: this.brands, mobile: this.mobile, platform: this.platform }; }
      }) : undefined,
      javaEnabled: asNative(function javaEnabled() { return false; }, 'javaEnabled'),
      // PluginArray/MimeTypeArray expose `length` as a getter-only accessor; the
      // prototype already returns 0, so create-without-assign (assigning length
      // would throw "no setter for property" in strict mode).
      plugins: Object.create(G.PluginArray.prototype),
      mimeTypes: Object.create(G.MimeTypeArray.prototype),
      sendBeacon: asNative(function sendBeacon() { return true; }, 'sendBeacon'),
      clearAppBadge: asNative(function clearAppBadge() { return Promise.resolve(); }, 'clearAppBadge')
    });
    def('navigator', discoveryProxy(navBase, 'navigator'));

    // screen: a real Screen instance.
    const screenBase = Object.assign(Object.create(G.Screen.prototype), {
      width: prof.screen[0], height: prof.screen[1],
      availWidth: prof.screen[0], availHeight: prof.screen[1] - 40,
      colorDepth: 24, pixelDepth: 24, availLeft: 0, availTop: 0,
      orientation: { type: 'landscape-primary', angle: 0, onchange: null }
    });
    def('screen', screenBase);
    def('innerWidth', prof.screen[0]);
    def('innerHeight', prof.screen[1] - 120);
    def('outerWidth', prof.screen[0]);
    def('outerHeight', prof.screen[1]);
    // Window position values are always numbers in browsers. screenLeft and
    // screenTop are legacy aliases for screenX/screenY; keep all four at 0.
    def('screenX', 0);
    def('screenY', 0);
    def('screenLeft', 0);
    def('screenTop', 0);
    def('devicePixelRatio', 1);

    // location: a real Location instance.
    const loc = Object.assign(Object.create(G.Location.prototype), {
      href: 'https://www.youtube.com/', origin: 'https://www.youtube.com',
      protocol: 'https:', host: 'www.youtube.com', hostname: 'www.youtube.com',
      port: '', pathname: '/', search: '', hash: '',
      replace() {}, assign() {}, reload() {}, toString() { return this.href; }
    });
    def('location', loc);
    def('origin', loc.origin);

    // document: the behaviorally coherent Document from dom.js (createElement returns
    // correctly-typed, instanceof-coherent elements; wrapped for discovery).
    const doc = createDocument();
    doc._title = '';
    def('document', discoveryProxy(doc, 'document'));

    // window === globalThis, wrapped so unshimmed probes are logged.
    const win = discoveryProxy(G, 'window');
    def('window', win); def('self', win); def('top', win);
    def('parent', win); def('frames', win);

    // Top-level window scalars BotGuard reads (frame count, name, focus state).
    def('length', 0);
    def('name', '');
    def('closed', false);
    def('frameElement', null);
    def('status', '');
    def('isSecureContext', true);
    def('crossOriginIsolated', false);
    def('history', Object.create(G.History.prototype));
    def('localStorage', Object.create(G.Storage.prototype));
    def('sessionStorage', Object.create(G.Storage.prototype));

    // Coherent timezone: Date and Intl must agree with the profile. dom.js fixes
    // Date.prototype.getTimezoneOffset plus the local getters; installIntl pins
    // Intl's resolved timeZone and provides a minimal Intl when QuickJS lacks one.
    installDateTimezone(prof.utcOffsetMinutes);
    installIntl(prof);

    // No-op rendering/animation hooks some probes expect.
    defFn('requestAnimationFrame', function requestAnimationFrame(cb) { return G.setTimeout(() => cb(G.performance ? G.performance.now() : 0), 16); });
    defFn('cancelAnimationFrame', function cancelAnimationFrame(id) { return G.clearTimeout(id); });
    defFn('requestIdleCallback', function requestIdleCallback(cb) { return G.setTimeout(() => cb({ didTimeout: false, timeRemaining: () => 50 }), 1); });
    defFn('cancelIdleCallback', function cancelIdleCallback(id) { return G.clearTimeout(id); });
    defFn('matchMedia', function matchMedia(q) { return { matches: false, media: String(q), onchange: null, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent() { return true; } }; });
    defFn('getComputedStyle', function getComputedStyle() { const s = Object.create(G.CSSStyleDeclaration.prototype); s.getPropertyValue = asNative(function getPropertyValue() { return ''; }, 'getPropertyValue'); return s; });
    defFn('addEventListener', function addEventListener() {});
    defFn('removeEventListener', function removeEventListener() {});
    defFn('dispatchEvent', function dispatchEvent() { return true; });
    defFn('postMessage', function postMessage() {});
    defFn('focus', function focus() {});
    defFn('blur', function blur() {});
    defFn('scroll', function scroll() {});
    defFn('scrollTo', function scrollTo() {});
    defFn('open', function open() { return null; });
    defFn('close', function close() {});
    defFn('alert', function alert() {});
    return currentProfile;
  };

  // Apply the default profile at load; Go overrides via __wxApplyProfile.
  G.__wxApplyProfile(null);
})();
