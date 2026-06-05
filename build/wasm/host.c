/*
 * WaxSeal QuickJS host ABI (compiled to a WASI "reactor" wasm module).
 *
 * This is the only C we ship. It wraps quickjs-ng's public API (quickjs.h)
 * and exposes a small, length-prefixed contract that the pure-Go wazero
 * backend (internal/jsruntime/quickjs) drives. It deliberately does not use
 * quickjs-libc (no std/os/worker/module-loader surface): the only host imports
 * are WASI preview1 (random_get -> getentropy, clock_time_get, fd_write).
 *
 * Design notes (see i-am-looking-to-zany-dragon.md, "The QuickJS-on-wazero runtime"):
 *  - One wasm instance == one JSRuntime == one linear memory (isolation). The
 *    Go side compiles once and instantiates many; C statics live in per-instance
 *    linear memory, so `g_rt`/`g_ctx`/`g_deadline_ns` are independent per instance.
 *  - Untrusted obfuscated JS is bounded three ways: JS_SetMemoryLimit,
 *    JS_SetMaxStackSize, and a deadline-checked JS_SetInterruptHandler. wazero's
 *    WithCloseOnContextDone is the outer (Go-owned) watchdog on top of these.
 *  - crypto.getRandomValues / Math.random are wired to getentropy()
 *    (WASI random_get), which wazero backs with crypto/rand.
 *  - Go owns the real deadline; the event loop here drains all microtasks first
 *    and only advances a virtual clock (a JS-side timer queue in the shim) when
 *    otherwise idle, so a synthetic setTimeout can never beat real VM progress.
 *
 * Result ABI: wx_eval / wx_call return a packed u64 == ((u64)ptr << 32) | len.
 * The buffer at ptr is [1 status byte][UTF-8 payload]:
 *   status 0 -> payload is the JSON encoding of the JS value ("null" for undefined)
 *   status 1 -> payload is a redacted-elsewhere error string "Name: message\n<stack>"
 * The host reads len bytes and then calls wx_free(ptr).
 */

#include "quickjs.h"

#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <stdio.h>
#include <time.h>
#include <sys/random.h> /* getentropy */

/* Per-instance state lives in this instance's linear memory. */

static JSRuntime *g_rt = NULL;
static JSContext *g_ctx = NULL;

/* Interrupt deadline in monotonic nanoseconds; 0 == disabled. */
static int64_t g_deadline_ns = 0;

/* Backstop against a pathological self-rescheduling virtual timer that never
 * yields to real time; the interrupt handler is the primary guard. */
#define WX_MAX_PUMP_ITERS 500000

static int64_t now_ns(void) {
    struct timespec ts;
    /* CLOCK_MONOTONIC -> WASI clock_time_get (wazero-backed). */
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (int64_t)ts.tv_sec * 1000000000LL + (int64_t)ts.tv_nsec;
}

static int interrupt_handler(JSRuntime *rt, void *opaque) {
    (void)rt; (void)opaque;
    if (g_deadline_ns == 0)
        return 0;
    return now_ns() > g_deadline_ns ? 1 : 0; /* 1 -> throw InternalError, unwind */
}

/* Host-backed JS builtins, registered as C functions rather than wasm imports. */

static void fill_csprng(uint8_t *p, size_t n) {
    /* getentropy caps at 256 bytes/call; loop. WASI random_get is wired by
     * wazero to crypto/rand via ModuleConfig.WithRandSource. */
    while (n > 0) {
        size_t chunk = n > 256 ? 256 : n;
        if (getentropy(p, chunk) != 0) {
            /* Should not happen with wazero; abort rather than serve weak RNG. */
            abort();
        }
        p += chunk;
        n -= chunk;
    }
}

/* __wx_random_fill(typedArray): fill an integer TypedArray with CSPRNG bytes. */
static JSValue js_wx_random_fill(JSContext *ctx, JSValueConst this_val,
                                 int argc, JSValueConst *argv) {
    (void)this_val;
    if (argc < 1)
        return JS_ThrowTypeError(ctx, "__wx_random_fill: missing array");
    size_t byte_off = 0, byte_len = 0, bytes_per = 0;
    JSValue ab = JS_GetTypedArrayBuffer(ctx, argv[0], &byte_off, &byte_len, &bytes_per);
    if (JS_IsException(ab))
        return ab;
    size_t ab_size = 0;
    uint8_t *base = JS_GetArrayBuffer(ctx, &ab_size, ab);
    if (base && byte_off + byte_len <= ab_size)
        fill_csprng(base + byte_off, byte_len);
    JS_FreeValue(ctx, ab);
    return JS_UNDEFINED;
}

/* __wx_random_double(): a uniform double in [0,1) with 53 bits of CSPRNG. */
static JSValue js_wx_random_double(JSContext *ctx, JSValueConst this_val,
                                   int argc, JSValueConst *argv) {
    (void)ctx; (void)this_val; (void)argc; (void)argv;
    uint8_t buf[8];
    fill_csprng(buf, 8);
    uint64_t x = 0;
    for (int i = 0; i < 8; i++)
        x = (x << 8) | buf[i];
    /* top 53 bits -> [0,1) */
    return JS_NewFloat64(ctx, (double)(x >> 11) * (1.0 / 9007199254740992.0));
}

/* __wx_console(level, message): route shim console.* to fd_write (wazero stderr).
 * level: 0 log,1 info,2 warn,3 error,4 debug. */
static JSValue js_wx_console(JSContext *ctx, JSValueConst this_val,
                             int argc, JSValueConst *argv) {
    (void)this_val;
    int level = 0;
    if (argc >= 1)
        JS_ToInt32(ctx, &level, argv[0]);
    const char *msg = argc >= 2 ? JS_ToCString(ctx, argv[1]) : NULL;
    static const char *tags[] = {"log", "info", "warn", "error", "debug"};
    const char *tag = (level >= 0 && level <= 4) ? tags[level] : "log";
    fprintf(stderr, "[js:%s] %s\n", tag, msg ? msg : "");
    if (msg)
        JS_FreeCString(ctx, msg);
    return JS_UNDEFINED;
}

static void register_host_builtins(JSContext *ctx) {
    JSValue g = JS_GetGlobalObject(ctx);
    JS_SetPropertyStr(ctx, g, "__wx_random_fill",
                      JS_NewCFunction(ctx, js_wx_random_fill, "__wx_random_fill", 1));
    JS_SetPropertyStr(ctx, g, "__wx_random_double",
                      JS_NewCFunction(ctx, js_wx_random_double, "__wx_random_double", 0));
    JS_SetPropertyStr(ctx, g, "__wx_console",
                      JS_NewCFunction(ctx, js_wx_console, "__wx_console", 2));
    JS_FreeValue(ctx, g);
}

/* Result marshaling. */

/* Pack a status byte + payload into a fresh malloc'd buffer; return
 * ((u64)ptr<<32)|len. Host frees ptr via wx_free. */
static uint64_t pack_result(uint8_t status, const char *payload, size_t len) {
    size_t total = 1 + len;
    uint8_t *buf = (uint8_t *)malloc(total ? total : 1);
    if (!buf)
        return 0; /* host treats 0 as fatal */
    buf[0] = status;
    if (len)
        memcpy(buf + 1, payload, len);
    return ((uint64_t)(uintptr_t)buf << 32) | (uint32_t)total;
}

/* Format the pending exception (or a thrown value) into a status-1 result. */
static uint64_t pack_exception(JSContext *ctx) {
    JSValue exc = JS_GetException(ctx);
    const char *msg = JS_ToCString(ctx, exc);
    char *full = NULL;
    size_t full_len = 0;

    /* Append .stack when present; the Go layer redacts before live logging. */
    JSValue stack = JS_GetPropertyStr(ctx, exc, "stack");
    const char *stk = JS_IsUndefined(stack) ? NULL : JS_ToCString(ctx, stack);

    const char *m = msg ? msg : "exception";
    if (stk && *stk) {
        full_len = strlen(m) + 1 + strlen(stk);
        full = (char *)malloc(full_len + 1);
        if (full) {
            strcpy(full, m);
            strcat(full, "\n");
            strcat(full, stk);
        }
    }
    uint64_t r;
    if (full)
        r = pack_result(1, full, full_len);
    else
        r = pack_result(1, m, strlen(m));

    free(full);
    if (stk) JS_FreeCString(ctx, stk);
    JS_FreeValue(ctx, stack);
    if (msg) JS_FreeCString(ctx, msg);
    JS_FreeValue(ctx, exc);
    return r;
}

/* JSON-encode a (non-promise, non-exception) value into a status-0 result. */
static uint64_t pack_value(JSContext *ctx, JSValueConst val) {
    JSValue json = JS_JSONStringify(ctx, val, JS_UNDEFINED, JS_UNDEFINED);
    if (JS_IsException(json)) {
        JS_FreeValue(ctx, json);
        return pack_exception(ctx);
    }
    uint64_t r;
    if (JS_IsUndefined(json)) {
        /* value was undefined / function / symbol -> JSON null */
        r = pack_result(0, "null", 4);
    } else {
        size_t len = 0;
        const char *s = JS_ToCStringLen(ctx, &len, json);
        r = pack_result(0, s ? s : "null", s ? len : 4);
        if (s) JS_FreeCString(ctx, s);
    }
    JS_FreeValue(ctx, json);
    return r;
}

/* Fire the earliest-due virtual timer (shim __wx_runTimers); 1 if one fired. */
static int run_one_virtual_timer(JSContext *ctx) {
    JSValue g = JS_GetGlobalObject(ctx);
    JSValue fn = JS_GetPropertyStr(ctx, g, "__wx_runTimers");
    int fired = 0;
    if (JS_IsFunction(ctx, fn)) {
        JSValue r = JS_Call(ctx, fn, g, 0, NULL);
        if (!JS_IsException(r))
            fired = JS_ToBool(ctx, r);
        JS_FreeValue(ctx, r);
    }
    JS_FreeValue(ctx, fn);
    JS_FreeValue(ctx, g);
    return fired;
}

/* Take ownership of `val` (a JS_Call/JS_Eval result) and produce a result:
 *  - exception sentinel  -> status 1
 *  - promise             -> pump (microtasks first, then minimal virtual time)
 *                           until settled, then encode value or rejection
 *  - plain value         -> JSON-encode */
static uint64_t finish(JSContext *ctx, JSValue val) {
    if (JS_IsException(val)) {
        JS_FreeValue(ctx, val);
        return pack_exception(ctx);
    }
    if (!JS_IsPromise(val)) {
        uint64_t r = pack_value(ctx, val);
        JS_FreeValue(ctx, val);
        return r;
    }

    for (int iter = 0; iter < WX_MAX_PUMP_ITERS; iter++) {
        /* Drain all ready Promise jobs (microtasks) first. */
        JSContext *jc;
        int r;
        while ((r = JS_ExecutePendingJob(g_rt, &jc)) > 0) { /* spin */ }
        if (r < 0) {
            /* A job threw; its exception is pending on jc's context. */
            JS_FreeValue(ctx, val);
            return pack_exception(jc ? jc : ctx);
        }

        JSPromiseStateEnum st = JS_PromiseState(ctx, val);
        if (st == JS_PROMISE_FULFILLED) {
            JSValue res = JS_PromiseResult(ctx, val);
            JS_FreeValue(ctx, val);
            /* result could itself be a thenable in theory; encode directly. */
            uint64_t out = pack_value(ctx, res);
            JS_FreeValue(ctx, res);
            return out;
        }
        if (st == JS_PROMISE_REJECTED) {
            JSValue res = JS_PromiseResult(ctx, val);
            JS_FreeValue(ctx, val);
            JS_Throw(ctx, res); /* re-raise so pack_exception formats it */
            return pack_exception(ctx);
        }

        /* Still pending: only now advance virtual time, minimally. */
        if (!run_one_virtual_timer(ctx)) {
            JS_FreeValue(ctx, val);
            JS_ThrowInternalError(ctx, "promise pending with no jobs or timers (deadlock)");
            return pack_exception(ctx);
        }
    }
    JS_FreeValue(ctx, val);
    JS_ThrowInternalError(ctx, "pump exceeded max iterations");
    return pack_exception(ctx);
}

/* Exported ABI. */

__attribute__((export_name("wx_alloc")))
void *wx_alloc(uint32_t size) {
    return malloc(size ? size : 1);
}

__attribute__((export_name("wx_free")))
void wx_free(void *ptr) {
    free(ptr);
}

__attribute__((export_name("wx_init")))
int wx_init(uint32_t memory_limit, uint32_t max_stack) {
    if (g_rt)
        return -1; /* already initialized */
    g_rt = JS_NewRuntime();
    if (!g_rt)
        return -1;
    if (memory_limit)
        JS_SetMemoryLimit(g_rt, (size_t)memory_limit);
    if (max_stack)
        JS_SetMaxStackSize(g_rt, (size_t)max_stack);
    JS_SetInterruptHandler(g_rt, interrupt_handler, NULL);

    g_ctx = JS_NewContext(g_rt);
    if (!g_ctx) {
        JS_FreeRuntime(g_rt);
        g_rt = NULL;
        return -1;
    }
    register_host_builtins(g_ctx);
    return 0;
}

/* Set the interrupt deadline relative to now; ms<=0 disables it. */
__attribute__((export_name("wx_set_deadline_ms")))
void wx_set_deadline_ms(int32_t ms) {
    g_deadline_ns = ms <= 0 ? 0 : now_ns() + (int64_t)ms * 1000000LL;
}

/* Evaluate `src` as a global script and finish() the result. */
__attribute__((export_name("wx_eval")))
uint64_t wx_eval(const char *src, uint32_t src_len) {
    if (!g_ctx)
        return pack_result(1, "runtime not initialized", 23);
    JSValue v = JS_Eval(g_ctx, src, src_len, "<waxseal>", JS_EVAL_TYPE_GLOBAL);
    return finish(g_ctx, v);
}

/* Call globalThis[name](...JSON.parse(args_json)) and finish() the result.
 * args_json must encode a JS array (e.g. "[\"a\",\"b\"]"); "" or "[]" -> no args. */
__attribute__((export_name("wx_call")))
uint64_t wx_call(const char *name, uint32_t name_len,
                 const char *args_json, uint32_t args_len) {
    if (!g_ctx)
        return pack_result(1, "runtime not initialized", 23);

    JSValue g = JS_GetGlobalObject(g_ctx);

    /* JS_GetPropertyStr needs a nul-terminated C string; `name` may not be. */
    char *cname = (char *)malloc(name_len + 1);
    if (!cname) {
        JS_FreeValue(g_ctx, g);
        return pack_result(1, "oom", 3);
    }
    memcpy(cname, name, name_len);
    cname[name_len] = '\0';
    JSValue fn = JS_GetPropertyStr(g_ctx, g, cname);
    free(cname);

    if (!JS_IsFunction(g_ctx, fn)) {
        JS_FreeValue(g_ctx, fn);
        JS_FreeValue(g_ctx, g);
        return pack_result(1, "global function not found", 25);
    }

    /* Parse args JSON array. */
    JSValue argv_arr = JS_UNDEFINED;
    int argc = 0;
    JSValue *argv = NULL;
    if (args_len > 0) {
        argv_arr = JS_ParseJSON(g_ctx, args_json, args_len, "<args>");
        if (JS_IsException(argv_arr)) {
            JS_FreeValue(g_ctx, fn);
            JS_FreeValue(g_ctx, g);
            return pack_exception(g_ctx);
        }
        JSValue lenv = JS_GetPropertyStr(g_ctx, argv_arr, "length");
        JS_ToInt32(g_ctx, &argc, lenv);
        JS_FreeValue(g_ctx, lenv);
        if (argc > 0) {
            argv = (JSValue *)malloc(sizeof(JSValue) * argc);
            for (int i = 0; i < argc; i++)
                argv[i] = JS_GetPropertyUint32(g_ctx, argv_arr, i);
        }
    }

    JSValue ret = JS_Call(g_ctx, fn, g, argc, argv);

    if (argv) {
        for (int i = 0; i < argc; i++)
            JS_FreeValue(g_ctx, argv[i]);
        free(argv);
    }
    JS_FreeValue(g_ctx, argv_arr);
    JS_FreeValue(g_ctx, fn);
    JS_FreeValue(g_ctx, g);

    return finish(g_ctx, ret);
}

/* Explicitly pump the job/timer loop up to max_iters; returns jobs+timers run.
 * wx_eval/wx_call drive this internally; exposed for tests/diagnostics. */
__attribute__((export_name("wx_pump")))
int32_t wx_pump(int32_t max_iters) {
    if (!g_ctx)
        return 0;
    int processed = 0;
    for (int iter = 0; iter < max_iters; iter++) {
        JSContext *jc;
        int r, did = 0;
        while ((r = JS_ExecutePendingJob(g_rt, &jc)) > 0) {
            processed++;
            did = 1;
        }
        if (run_one_virtual_timer(g_ctx)) {
            processed++;
            did = 1;
        }
        if (!did)
            break;
    }
    return processed;
}

__attribute__((export_name("wx_destroy")))
void wx_destroy(void) {
    if (g_ctx) {
        JS_FreeContext(g_ctx);
        g_ctx = NULL;
    }
    if (g_rt) {
        JS_FreeRuntime(g_rt);
        g_rt = NULL;
    }
    g_deadline_ns = 0;
}
