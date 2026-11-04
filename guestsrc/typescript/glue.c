// The plg guest ABI for TypeScript plugins, on QuickJS-ng (built by `make guests`, see the Makefile).
//
// The host bundles a plugin into one script with esbuild and sends it in the init payload (internal/scriptabi):
// its file name, the script, the number of interface methods and the wire.Schema of the interface's methods
// followed by the Host's. plg_init evaluates the prelude (console, fetch, Headers, Response), creates the global
// `host` object, evaluates the script and calls its exported Create; plg_call decodes the wire-encoded
// arguments straight into JS values, calls the method with `this` bound to Create's result, runs pending jobs
// until a returned Promise settles, and encodes the result. QuickJS's std and os modules are not linked: plugins
// get the ECMAScript built-ins, console, host and fetch only.
//
// Result buffers start with a status byte (internal/wire): OK + results, or Error (the plugin threw), Load (the
// script or Create is unusable), Convert (a result does not fit its Go type), Panic (out of memory: the host
// discards the instance) or ABI (a malformed request) + a message. A step that ran deep on the stack also sets
// the status's deep-stack flag (see probes below).

#include <math.h>
#include <malloc.h>
#include <stdarg.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include "quickjs.h"

#define EXPORT(name) __attribute__((export_name(name)))
#define IMPORT(name) __attribute__((import_module("plg"), import_name(name)))

IMPORT("host_call") uint32_t plg_host_call(uint32_t method, uint32_t ptr, uint32_t len);
IMPORT("http_roundtrip") uint32_t plg_http_roundtrip(uint32_t ptr, uint32_t len);
IMPORT("take_result") void plg_take_result(uint32_t ptr, uint32_t len);
IMPORT("http_start") uint32_t plg_http_start(uint32_t call, uint32_t ptr, uint32_t len);

// The prelude: a function expression taking the natives object (prelude.js).
static const char prelude[] = {
#embed "prelude.js"
    , 0};

enum status { ST_OK = 1, ST_ERROR, ST_PANIC, ST_LOAD, ST_ABI, ST_CONVERT };
#define ST_DEEP_STACK 0x80 // engine.FlagDeepStack

enum kind {
  K_BOOL = 1, K_INT, K_UINT, K_FLOAT, K_STRING, K_BYTES, K_TIME, K_ERROR, K_SLICE, K_MAP, K_POINTER, K_STRUCT,
};

#define MAX_DEPTH 10000            // wire.MaxDepth
#define MAX_MESSAGE (64 << 10)     // wire.MaxMessage
#define NIL_LENGTH 0xFFFFFFFFu     // the length prefix of a nil slice or map
#define MAX_SAFE 9007199254740992.0 // 2^53, wire.MaxSafeInt
#define ZERO_TIME_SECONDS (-62135596800LL) // time.Time{}.Unix()
// The C stack QuickJS may use for JS frames, about 3000 nested JS calls: deeper recursion throws an ordinary
// RangeError. The linker gives the module a 2 MiB stack (TS_STACK in the Makefile, against wasm-ld's default
// 64 KiB), leaving the glue's own recursion over deeply nested values room to spare. A larger limit is not
// better: each JS frame also takes several KiB of wazero's native stack, which lives on the host's Go heap, and
// past about 6 MiB here that stack reaches wazero's ceiling first and the call traps.
#define JS_STACK_LIMIT (1 << 20)

// ---- stack probes ----

// wazero runs each JS frame on several KiB of its own native stack (about 13 KiB: 40 times the frame's share of
// the wasm stack), a Go allocation that grows with the deepest call and is kept with the export until the
// function is replaced: one deep recursion would leave the instance holding tens of MiB outside its memory
// limit. So probe words sit DEEP_STACK down the wasm stack (which starts at __stack_high and grows down), and a
// step that overwrote one ran deeper than about 200 JS frames: its result's status gets ST_DEEP_STACK, on which
// the host replaces the export's function, letting the grown stack go.
#define DEEP_STACK (64 << 10)
#define PROBES 16        // words, 64 bytes apart: a frame crossing the band writes some of them
#define PROBE_STRIDE 8   // in words
#define PROBE 0x706c67737461636bULL // "plgstack"

extern unsigned char __stack_high[];

static uint64_t *probes(void) {
  return (uint64_t *)(__stack_high - DEEP_STACK) - PROBES * PROBE_STRIDE;
}

// paint_probes runs first when the module starts (_initialize runs constructors), then after each deep step.
__attribute__((constructor)) static void paint_probes(void) {
  uint64_t *p = probes();
  for (int i = 0; i < PROBES; i++) p[i * PROBE_STRIDE] = PROBE;
}

// ran_deep reports whether the step overwrote a probe, repainting them.
static bool ran_deep(void) {
  uint64_t *p = probes();
  for (int i = 0; i < PROBES; i++) {
    if (p[i * PROBE_STRIDE] != PROBE) {
      paint_probes();
      return true;
    }
  }
  return false;
}

// ---- buffers ----

typedef struct {
  uint8_t *p;
  size_t len, cap;
  bool failed; // an allocation failed: the contents are incomplete
} buf_t;

static bool grow(buf_t *b, size_t n) {
  if (b->failed) return false;
  if (b->len + n <= b->cap) return true;
  size_t c = b->cap ? b->cap : 256;
  while (c < b->len + n) c *= 2;
  uint8_t *p = realloc(b->p, c);
  if (!p) {
    b->failed = true;
    return false;
  }
  b->p = p;
  b->cap = c;
  return true;
}

static void put(buf_t *b, const void *s, size_t n) {
  if (grow(b, n)) {
    if (n) memcpy(b->p + b->len, s, n);
    b->len += n;
  }
}

static void put_u8(buf_t *b, uint8_t v) { put(b, &v, 1); }
static void put_u32(buf_t *b, uint32_t v) { put(b, &v, 4); }
static void put_u64(buf_t *b, uint64_t v) { put(b, &v, 8); }

static void put_str(buf_t *b, const char *s, size_t n) {
  put_u32(b, (uint32_t)n);
  put(b, s, n);
}

typedef struct {
  const uint8_t *p;
  size_t len, off;
} rd_t;

static bool rd_need(rd_t *r, size_t n) { return r->len - r->off >= n; }

static bool rd_u8(rd_t *r, uint8_t *v) {
  if (!rd_need(r, 1)) return false;
  *v = r->p[r->off++];
  return true;
}

static bool rd_u32(rd_t *r, uint32_t *v) {
  if (!rd_need(r, 4)) return false;
  memcpy(v, r->p + r->off, 4);
  r->off += 4;
  return true;
}

static bool rd_u64(rd_t *r, uint64_t *v) {
  if (!rd_need(r, 8)) return false;
  memcpy(v, r->p + r->off, 8);
  r->off += 8;
  return true;
}

static bool rd_str(rd_t *r, const char **s, uint32_t *n) {
  if (!rd_u32(r, n) || !rd_need(r, *n)) return false;
  *s = (const char *)r->p + r->off;
  r->off += *n;
  return true;
}

// ---- state ----

typedef struct {
  JSAtom atom;
  char *name;
  uint32_t type;
} field_t;

typedef struct {
  uint8_t kind, bits;
  uint32_t elem;
  uint32_t nfields;
  field_t *fields;
  size_t min; // the smallest encoding of a value of the type, to bound decoded counts by the input
} type_t;

typedef struct {
  JSAtom atom;
  char *name;
  uint32_t nparams, nresults;
  uint32_t *params, *results;
} method_t;

static JSRuntime *rt;
static JSContext *ctx;
static JSValue inst = JS_UNDEFINED; // Create's result
static bool closed;

static type_t *types;
static uint32_t ntypes;
static method_t *methods;
static uint32_t nmethods, nplugin; // methods [0, nplugin) are the interface's, the rest the Host's

static buf_t in;     // the input buffer plg_alloc hands out
static buf_t out;    // the result buffer
static buf_t req;    // host_call and http_roundtrip requests
static buf_t answer; // their answers

static JSAtom a_seconds, a_nanos, a_message, a_getTime, a_close, a_create;

// oom is set when an allocation fails; a call that fails afterwards ran out of memory.
static bool oom;

// ---- allocation ----

static void *js_calloc_(void *opaque, size_t count, size_t size) {
  (void)opaque;
  void *p = calloc(count, size);
  if (!p && count && size) oom = true;
  return p;
}

static void *js_malloc_(void *opaque, size_t size) {
  (void)opaque;
  void *p = malloc(size);
  if (!p && size) oom = true;
  return p;
}

static void js_free_(void *opaque, void *ptr) {
  (void)opaque;
  free(ptr);
}

static void *js_realloc_(void *opaque, void *ptr, size_t size) {
  (void)opaque;
  void *p = realloc(ptr, size);
  if (!p && size) oom = true;
  return p;
}

static size_t js_usable_size_(const void *ptr) { return malloc_usable_size((void *)ptr); }

static const JSMallocFunctions malloc_functions = {
    js_calloc_, js_malloc_, js_free_, js_realloc_, js_usable_size_,
};

// ---- results ----

// A static answer for when the result buffer cannot grow: out of memory, which discards the instance.
static uint8_t oom_result[] = {ST_PANIC, 13, 0, 0, 0, 'o', 'u', 't', ' ', 'o', 'f', ' ', 'm', 'e', 'm', 'o', 'r', 'y'};

// packed ends a step: its result buffer, packed ptr<<32 | len, with the deep-stack flag on its status when the
// step ran deep (an instance out of memory is discarded, so its static answer needs none).
static uint64_t packed(void) {
  if (out.failed) return ((uint64_t)(uintptr_t)oom_result << 32) | sizeof oom_result;
  if (ran_deep()) out.p[0] |= ST_DEEP_STACK;
  return ((uint64_t)(uintptr_t)out.p << 32) | out.len;
}

static void begin(uint8_t status) {
  out.len = 0;
  out.failed = false;
  put_u8(&out, status);
}

// message ends the result with a status and a message, cut to MAX_MESSAGE bytes at a UTF-8 boundary.
static uint64_t message(uint8_t status, const char *msg, size_t n) {
  begin(status);
  if (n > MAX_MESSAGE) {
    n = MAX_MESSAGE;
    while (n > 0 && ((uint8_t)msg[n] & 0xC0) == 0x80) n--;
  }
  put_str(&out, msg, n);
  return packed();
}

static uint64_t messagef(uint8_t status, const char *fmt, const char *arg) {
  char text[512];
  int n = snprintf(text, sizeof text, fmt, arg);
  return message(status, text, n < 0 ? 0 : (size_t)n < sizeof text ? (size_t)n : sizeof text - 1);
}

// ---- errors ----

static void write_all(int fd, const char *s, size_t n) {
  while (n > 0) {
    ssize_t w = write(fd, s, n);
    if (w <= 0) return;
    s += w;
    n -= (size_t)w;
  }
}

// text returns the C string of v (a TypeError-safe ToString), or NULL; free it with JS_FreeCString.
static const char *text(JSValueConst v, size_t *n) {
  const char *s = JS_ToCStringLen(ctx, n, v);
  if (!s) JS_FreeValue(ctx, JS_GetException(ctx));
  return s;
}

// describe returns the message of a thrown value: an Error's message, otherwise String(v).
static const char *describe(JSValueConst e, size_t *n) {
  if (JS_IsError(e)) {
    JSValue m = JS_GetProperty(ctx, e, a_message);
    if (JS_IsException(m)) JS_FreeValue(ctx, JS_GetException(ctx));
    if (JS_IsString(m)) {
      const char *s = text(m, n);
      JS_FreeValue(ctx, m);
      return s;
    }
    JS_FreeValue(ctx, m);
  }
  return text(e, n);
}

// trace writes a thrown value and its stack to stderr: the guest traceback of a PanicError.
static void trace(JSValueConst e) {
  size_t n;
  const char *s = text(e, &n);
  if (s) {
    write_all(2, s, n);
    write_all(2, "\n", 1);
    JS_FreeCString(ctx, s);
  }
  if (JS_IsError(e)) {
    JSValue stack = JS_GetPropertyStr(ctx, e, "stack");
    if (JS_IsString(stack) && (s = text(stack, &n))) {
      write_all(2, s, n);
      JS_FreeCString(ctx, s);
    }
    JS_FreeValue(ctx, stack);
  }
}

// failure ends a step whose JS code threw: out of memory is a panic (the instance is discarded), anything else
// the given status with the thrown value's message.
static uint64_t failure(uint8_t status, const char *prefix) {
  JSValue e = JS_GetException(ctx);
  if (oom) {
    JS_FreeValue(ctx, e);
    write_all(2, "plg: the plugin ran out of memory\n", 34);
    return message(ST_PANIC, "out of memory", 13);
  }
  size_t n = 0;
  const char *s = describe(e, &n);
  uint64_t res;
  if (prefix) {
    char head[256];
    int h = snprintf(head, sizeof head, "%s", prefix);
    buf_t msg = {0};
    put(&msg, head, (size_t)h);
    put(&msg, s ? s : "exception", s ? n : 9);
    res = msg.failed ? message(ST_PANIC, "out of memory", 13) : message(status, (char *)msg.p, msg.len);
    free(msg.p);
  } else {
    res = message(status, s ? s : "exception", s ? n : 9);
  }
  if (s) JS_FreeCString(ctx, s);
  JS_FreeValue(ctx, e);
  return res;
}

// ---- jobs and promises ----

// drain runs pending jobs until none is left or, with p, until p settles. A job that throws is reported on
// stderr: nothing else could see it.
static void drain(JSValueConst p) {
  for (;;) {
    if (!JS_IsUndefined(p) && JS_PromiseState(ctx, p) != JS_PROMISE_PENDING) return;
    JSContext *jctx;
    int r = JS_ExecutePendingJob(rt, &jctx);
    if (r == 0) return;
    if (r < 0) {
      JSValue e = JS_GetException(jctx);
      write_all(2, "plg: uncaught exception in a pending job: ", 42);
      trace(e);
      JS_FreeValue(ctx, e);
    }
  }
}

// settle turns v into its settled value: a fulfilled Promise's value, a rejected one's reason thrown, or v itself.
// It takes v over.
static JSValue settle(JSValue v) {
  if (JS_IsException(v)) {
    drain(JS_UNDEFINED);
    return v;
  }
  if (!JS_IsPromise(v)) {
    drain(JS_UNDEFINED);
    return v;
  }
  drain(v);
  JSValue res;
  switch (JS_PromiseState(ctx, v)) {
  case JS_PROMISE_FULFILLED:
    res = JS_PromiseResult(ctx, v);
    break;
  case JS_PROMISE_REJECTED:
    JS_PromiseMarkAsHandled(ctx, v);
    res = JS_Throw(ctx, JS_PromiseResult(ctx, v));
    break;
  default:
    res = JS_ThrowInternalError(ctx, "the returned promise never settles: no pending job is left to run");
  }
  JS_FreeValue(ctx, v);
  drain(JS_UNDEFINED);
  return res;
}

// ---- schema ----

static bool valid_bits(uint8_t kind, uint8_t bits) {
  if (kind == K_FLOAT) return bits == 32 || bits == 64;
  return bits == 8 || bits == 16 || bits == 32 || bits == 64;
}

static char *dup_name(const char *s, uint32_t n) {
  char *d = malloc((size_t)n + 1);
  if (d) {
    memcpy(d, s, n);
    d[n] = 0;
  }
  return d;
}

static bool read_indexes(rd_t *r, uint32_t *count, uint32_t **idx) {
  if (!rd_u32(r, count) || !rd_need(r, (size_t)*count * 4)) return false;
  *idx = calloc(*count ? *count : 1, 4);
  if (!*idx) return false;
  for (uint32_t i = 0; i < *count; i++) {
    rd_u32(r, &(*idx)[i]);
    if ((*idx)[i] >= ntypes) return false;
  }
  return true;
}

// min_size computes each type's smallest encoding. Structs reach other structs only through pointers, slices
// and maps (Go has no recursive struct values), so the fixed point is reached in ntypes rounds.
static void min_sizes(void) {
  for (uint32_t round = 0; round <= ntypes; round++) {
    for (uint32_t i = 0; i < ntypes; i++) {
      type_t *t = &types[i];
      switch (t->kind) {
      case K_BOOL: case K_ERROR: case K_POINTER: t->min = 1; break;
      case K_INT: case K_UINT: case K_FLOAT: t->min = 8; break;
      case K_STRING: case K_BYTES: case K_SLICE: case K_MAP: t->min = 4; break;
      case K_TIME: t->min = 16; break;
      case K_STRUCT: {
        size_t m = 0;
        for (uint32_t f = 0; f < t->nfields; f++) m += types[t->fields[f].type].min;
        t->min = m;
        break;
      }
      }
    }
  }
}

static bool read_schema(rd_t *r) {
  if (!rd_u32(r, &ntypes) || !rd_need(r, ntypes)) return false;
  types = calloc(ntypes ? ntypes : 1, sizeof *types);
  if (!types) return false;
  for (uint32_t i = 0; i < ntypes; i++) {
    type_t *t = &types[i];
    if (!rd_u8(r, &t->kind)) return false;
    switch (t->kind) {
    case K_INT: case K_UINT: case K_FLOAT:
      if (!rd_u8(r, &t->bits) || !valid_bits(t->kind, t->bits)) return false;
      break;
    case K_SLICE: case K_MAP: case K_POINTER:
      if (!rd_u32(r, &t->elem)) return false;
      break;
    case K_STRUCT:
      if (!rd_u32(r, &t->nfields) || !rd_need(r, (size_t)t->nfields * 8)) return false;
      t->fields = calloc(t->nfields ? t->nfields : 1, sizeof *t->fields);
      if (!t->fields) return false;
      for (uint32_t f = 0; f < t->nfields; f++) {
        const char *s;
        uint32_t n;
        if (!rd_str(r, &s, &n) || !rd_u32(r, &t->fields[f].type)) return false;
        t->fields[f].name = dup_name(s, n);
        t->fields[f].atom = JS_NewAtomLen(ctx, s, n);
        if (!t->fields[f].name || t->fields[f].atom == JS_ATOM_NULL) return false;
      }
      break;
    case K_BOOL: case K_STRING: case K_BYTES: case K_TIME: case K_ERROR:
      break;
    default:
      return false;
    }
  }
  for (uint32_t i = 0; i < ntypes; i++) {
    type_t *t = &types[i];
    if ((t->kind == K_SLICE || t->kind == K_MAP || t->kind == K_POINTER) && t->elem >= ntypes) return false;
    for (uint32_t f = 0; f < t->nfields; f++)
      if (t->fields[f].type >= ntypes) return false;
  }
  min_sizes();
  if (!rd_u32(r, &nmethods) || nmethods < nplugin || !rd_need(r, nmethods)) return false;
  methods = calloc(nmethods ? nmethods : 1, sizeof *methods);
  if (!methods) return false;
  for (uint32_t i = 0; i < nmethods; i++) {
    method_t *m = &methods[i];
    const char *s;
    uint32_t n;
    if (!rd_str(r, &s, &n)) return false;
    m->name = dup_name(s, n);
    m->atom = JS_NewAtomLen(ctx, s, n);
    if (!m->name || m->atom == JS_ATOM_NULL) return false;
    if (!read_indexes(r, &m->nparams, &m->params) || !read_indexes(r, &m->nresults, &m->results)) return false;
  }
  return r->off == r->len;
}

// ---- wire -> JS ----

// decode_err names why decoding failed; when it is NULL, a JS exception is pending instead (out of memory).
static const char *decode_err;

static JSValue bad(const char *why) {
  decode_err = why;
  return JS_EXCEPTION;
}

static JSValue decode(rd_t *r, uint32_t ti, int depth);

static JSValue decode_int(rd_t *r, const type_t *t) {
  uint64_t u;
  if (!rd_u64(r, &u)) return bad("truncated integer");
  if (t->kind == K_INT) {
    int64_t v = (int64_t)u;
    if (v > (int64_t)MAX_SAFE || v < -(int64_t)MAX_SAFE) return bad("integer beyond ±2^53");
    if (t->bits < 64 && (v >= (1LL << (t->bits - 1)) || v < -(1LL << (t->bits - 1)))) return bad("integer out of range");
    return JS_NewInt64(ctx, v);
  }
  if (u > (uint64_t)MAX_SAFE) return bad("integer beyond ±2^53");
  if (t->bits < 64 && u >= (1ULL << t->bits)) return bad("integer out of range");
  return JS_NewInt64(ctx, (int64_t)u);
}

static JSValue decode_time(rd_t *r) {
  uint64_t s, ns;
  if (!rd_u64(r, &s) || !rd_u64(r, &ns)) return bad("truncated time");
  int64_t sec = (int64_t)s, nsec = (int64_t)ns;
  if (sec > (int64_t)MAX_SAFE || sec < -(int64_t)MAX_SAFE) return bad("time beyond ±2^53 seconds");
  if (nsec < 0 || nsec > 999999999) return bad("invalid nanoseconds");
  JSValue o = JS_NewObject(ctx);
  if (JS_IsException(o)) return o;
  if (JS_DefinePropertyValue(ctx, o, a_seconds, JS_NewInt64(ctx, sec), JS_PROP_C_W_E) < 0 ||
      JS_DefinePropertyValue(ctx, o, a_nanos, JS_NewInt64(ctx, nsec), JS_PROP_C_W_E) < 0) {
    JS_FreeValue(ctx, o);
    return JS_EXCEPTION;
  }
  return o;
}

static JSValue decode_list(rd_t *r, const type_t *t, int depth) {
  uint32_t n;
  if (!rd_u32(r, &n)) return bad("truncated length");
  if (n == NIL_LENGTH) return JS_NULL;
  const type_t *e = &types[t->elem];
  if (e->min > 0 && (r->len - r->off) / e->min < n) return bad("count exceeds the input");
  JSValue a = JS_NewArray(ctx);
  if (JS_IsException(a)) return a;
  for (uint32_t i = 0; i < n; i++) {
    JSValue v = decode(r, t->elem, depth + 1);
    if (JS_IsException(v) || JS_DefinePropertyValueUint32(ctx, a, i, v, JS_PROP_C_W_E) < 0) {
      JS_FreeValue(ctx, a);
      return JS_EXCEPTION;
    }
  }
  return a;
}

static JSValue decode_map(rd_t *r, const type_t *t, int depth) {
  uint32_t n;
  if (!rd_u32(r, &n)) return bad("truncated length");
  if (n == NIL_LENGTH) return JS_NULL;
  if ((r->len - r->off) / (4 + types[t->elem].min) < n) return bad("count exceeds the input");
  JSValue o = JS_NewObject(ctx);
  if (JS_IsException(o)) return o;
  for (uint32_t i = 0; i < n; i++) {
    const char *k;
    uint32_t kn;
    if (!rd_str(r, &k, &kn)) {
      JS_FreeValue(ctx, o);
      return bad("truncated map key");
    }
    JSAtom key = JS_NewAtomLen(ctx, k, kn);
    if (key == JS_ATOM_NULL) {
      JS_FreeValue(ctx, o);
      return JS_EXCEPTION;
    }
    int dup = JS_GetOwnProperty(ctx, NULL, o, key);
    JSValue v = dup == 0 ? decode(r, t->elem, depth + 1) : dup > 0 ? bad("duplicate map key") : JS_EXCEPTION;
    int ok = !JS_IsException(v) && JS_DefinePropertyValue(ctx, o, key, v, JS_PROP_C_W_E) >= 0;
    JS_FreeAtom(ctx, key);
    if (!ok) {
      JS_FreeValue(ctx, o);
      return JS_EXCEPTION;
    }
  }
  return o;
}

static JSValue decode_struct(rd_t *r, const type_t *t, int depth) {
  JSValue o = JS_NewObject(ctx);
  if (JS_IsException(o)) return o;
  for (uint32_t f = 0; f < t->nfields; f++) {
    JSValue v = decode(r, t->fields[f].type, depth + 1);
    if (JS_IsException(v) || JS_DefinePropertyValue(ctx, o, t->fields[f].atom, v, JS_PROP_C_W_E) < 0) {
      JS_FreeValue(ctx, o);
      return JS_EXCEPTION;
    }
  }
  return o;
}

static JSValue decode(rd_t *r, uint32_t ti, int depth) {
  const type_t *t = &types[ti];
  if (depth > MAX_DEPTH) return bad("value nested too deeply");
  uint8_t b;
  uint32_t n;
  const char *s;
  uint64_t u;
  switch (t->kind) {
  case K_BOOL:
    if (!rd_u8(r, &b) || b > 1) return bad("invalid bool");
    return JS_NewBool(ctx, b);
  case K_INT:
  case K_UINT:
    return decode_int(r, t);
  case K_FLOAT: {
    double d;
    if (!rd_u64(r, &u)) return bad("truncated float");
    memcpy(&d, &u, 8);
    return JS_NewFloat64(ctx, d);
  }
  case K_STRING:
    if (!rd_str(r, &s, &n)) return bad("truncated string");
    return JS_NewStringLen(ctx, s, n);
  case K_BYTES:
    if (!rd_u32(r, &n)) return bad("truncated length");
    if (n == NIL_LENGTH) return JS_NULL;
    if (!rd_need(r, n)) return bad("truncated bytes");
    r->off += n;
    return JS_NewUint8ArrayCopy(ctx, r->p + r->off - n, n);
  case K_TIME:
    return decode_time(r);
  case K_ERROR:
    if (!rd_u8(r, &b) || b > 1) return bad("invalid error presence");
    if (b == 0) return JS_NULL;
    if (!rd_str(r, &s, &n)) return bad("truncated error");
    return JS_NewStringLen(ctx, s, n);
  case K_SLICE:
    return decode_list(r, t, depth);
  case K_MAP:
    return decode_map(r, t, depth);
  case K_POINTER:
    if (!rd_u8(r, &b) || b > 1) return bad("invalid pointer presence");
    return b ? decode(r, t->elem, depth + 1) : JS_NULL;
  case K_STRUCT:
    return decode_struct(r, t, depth);
  }
  return bad("invalid kind");
}

// ---- JS -> wire ----

// why is why encoding failed, set together with a -1 return; a -1 with an empty why means a JS exception is
// pending (a throwing getter, out of memory). The path to the failing value is collected only then, as the
// failure unwinds: trail receives each enclosing segment (Go codec style: .qty, [1], ["a"]), innermost first and
// each reversed, so that reversing trail once gives the path. conv is the finished message.
static char why[512];
static buf_t trail;
static char conv[1024];
#define PATH_SHOWN 400 // at most this much of the path is shown, so the reason always fits conv

static int fail(const char *fmt, ...) __attribute__((format(printf, 1, 2)));

static int fail(const char *fmt, ...) {
  va_list ap;
  va_start(ap, fmt);
  vsnprintf(why, sizeof why, fmt, ap);
  va_end(ap);
  return -1;
}

// unwind adds a segment of the path to the value that failed to encode, on the way out; it passes r through.
static int unwind(int r, const char *s, size_t n) {
  if (r < 0 && why[0])
    while (n > 0) put_u8(&trail, (uint8_t)s[--n]);
  return r;
}

// finish writes conv: "<label> <k>: <path>: <why>", the path without its leading dot; a path deeper than a
// message can show (a cyclic value) keeps its start and its end.
static void finish(const char *label, unsigned k) {
  size_t n = trail.failed ? 0 : trail.len;
  for (size_t i = 0; i < n / 2; i++) {
    uint8_t c = trail.p[i];
    trail.p[i] = trail.p[n - 1 - i];
    trail.p[n - 1 - i] = c;
  }
  const char *p = n ? (const char *)trail.p : "";
  if (n && p[0] == '.') {
    p++;
    n--;
  }
  if (n > PATH_SHOWN)
    snprintf(conv, sizeof conv, "%s %u: %.*s…%.*s: %s", label, k, PATH_SHOWN / 2, p, PATH_SHOWN / 2,
             p + n - PATH_SHOWN / 2, why);
  else if (n)
    snprintf(conv, sizeof conv, "%s %u: %.*s: %s", label, k, (int)n, p, why);
  else
    snprintf(conv, sizeof conv, "%s %u: %s", label, k, why);
}

static const char *type_name(JSValueConst v) {
  if (JS_IsUndefined(v)) return "undefined";
  if (JS_IsNull(v)) return "null";
  if (JS_IsBool(v)) return "boolean";
  if (JS_IsNumber(v)) return "number";
  if (JS_IsBigInt(v)) return "bigint";
  if (JS_IsString(v)) return "string";
  if (JS_IsSymbol(v)) return "symbol";
  if (JS_IsFunction(ctx, v)) return "function";
  if (JS_IsArray(v)) return "array";
  return "object";
}

static const char *go_name(const type_t *t) {
  static char name[16];
  if (t->kind == K_FLOAT) {
    snprintf(name, sizeof name, "float%u", t->bits);
  } else {
    snprintf(name, sizeof name, "%s%u", t->kind == K_INT ? "int" : "uint", t->bits);
  }
  return name;
}

static int encode(buf_t *b, JSValueConst v, uint32_t ti, int depth);

static void encode_zero(buf_t *b, uint32_t ti) {
  const type_t *t = &types[ti];
  switch (t->kind) {
  case K_BOOL: case K_ERROR: case K_POINTER: put_u8(b, 0); break;
  case K_INT: case K_UINT: case K_FLOAT: put_u64(b, 0); break;
  case K_STRING: put_u32(b, 0); break;
  case K_BYTES: case K_SLICE: case K_MAP: put_u32(b, NIL_LENGTH); break;
  case K_TIME: put_u64(b, (uint64_t)ZERO_TIME_SECONDS); put_u64(b, 0); break;
  case K_STRUCT:
    for (uint32_t f = 0; f < t->nfields; f++) encode_zero(b, t->fields[f].type);
    break;
  }
}

static int number(JSValueConst v, double *d, const char *want) {
  if (!JS_IsNumber(v)) return fail("want %s, got %s", want, type_name(v));
  JS_ToFloat64(ctx, d, v);
  return 0;
}

static int encode_int(buf_t *b, JSValueConst v, const type_t *t) {
  double d;
  if (number(v, &d, "an integer number") < 0) return -1;
  if (!isfinite(d) || d != trunc(d)) return fail("wire: not an integer: %g", d);
  if (fabs(d) > MAX_SAFE) return fail("wire: integer beyond ±2^53: %.0f", d);
  if (t->kind == K_UINT) {
    if (d < 0 || (t->bits < 64 && d >= ldexp(1, t->bits)))
      return fail("wire: value out of range: %.0f does not fit %s", d, go_name(t));
    put_u64(b, (uint64_t)d);
  } else {
    if (t->bits < 64 && (d >= ldexp(1, t->bits - 1) || d < -ldexp(1, t->bits - 1)))
      return fail("wire: value out of range: %.0f does not fit %s", d, go_name(t));
    put_u64(b, (uint64_t)(int64_t)d);
  }
  return 0;
}

static int encode_time(buf_t *b, JSValueConst v) {
  double sec, nsec = 0;
  if (JS_IsDate(v)) {
    double ms;
    JSValue r = JS_Invoke(ctx, v, a_getTime, 0, NULL);
    if (JS_IsException(r)) return -1;
    JS_ToFloat64(ctx, &ms, r);
    JS_FreeValue(ctx, r);
    if (!isfinite(ms)) return fail("wire: invalid Date");
    sec = floor(ms / 1000);
    nsec = (ms - sec * 1000) * 1e6;
  } else if (JS_IsObject(v) && !JS_IsArray(v) && !JS_IsFunction(ctx, v)) {
    JSValue s = JS_GetProperty(ctx, v, a_seconds);
    JSValue n = JS_GetProperty(ctx, v, a_nanos);
    int bad_s = !JS_IsNumber(s), bad_n = !JS_IsUndefined(n) && !JS_IsNumber(n);
    if (!bad_s) JS_ToFloat64(ctx, &sec, s);
    if (!bad_n && !JS_IsUndefined(n)) JS_ToFloat64(ctx, &nsec, n);
    int exc = JS_IsException(s) || JS_IsException(n);
    JS_FreeValue(ctx, s);
    JS_FreeValue(ctx, n);
    if (exc) return -1;
    if (bad_s || bad_n) return fail("want a Date or {seconds, nanos} with numbers");
  } else {
    return fail("want a Date or {seconds, nanos}, got %s", type_name(v));
  }
  if (!isfinite(sec) || sec != trunc(sec) || !isfinite(nsec) || nsec != trunc(nsec))
    return fail("wire: seconds and nanos must be integers");
  if (fabs(sec) > MAX_SAFE) return fail("wire: integer beyond ±2^53: %.0f", sec);
  if (nsec < 0 || nsec > 999999999) return fail("wire: invalid encoding: %.0f nanoseconds", nsec);
  put_u64(b, (uint64_t)(int64_t)sec);
  put_u64(b, (uint64_t)(int64_t)nsec);
  return 0;
}

static int encode_bytes(buf_t *b, JSValueConst v) {
  size_t n;
  uint8_t *p;
  if (JS_IsNull(v) || JS_IsUndefined(v)) {
    put_u32(b, NIL_LENGTH);
    return 0;
  }
  if (JS_GetTypedArrayType(v) == JS_TYPED_ARRAY_UINT8) {
    p = JS_GetUint8Array(ctx, &n, v);
  } else if (JS_IsArrayBuffer(v)) {
    p = JS_GetArrayBuffer(ctx, &n, v);
  } else {
    return fail("want a Uint8Array, got %s", type_name(v));
  }
  if (!p && n) return -1;
  if (n >= NIL_LENGTH) return fail("wire: value too long");
  put_u32(b, (uint32_t)n);
  put(b, p, n);
  return 0;
}

static int encode_list(buf_t *b, JSValueConst v, const type_t *t, int depth) {
  if (JS_IsNull(v) || JS_IsUndefined(v)) {
    put_u32(b, NIL_LENGTH);
    return 0;
  }
  if (!JS_IsArray(v)) return fail("want an array, got %s", type_name(v));
  int64_t n;
  if (JS_GetLength(ctx, v, &n) < 0) return -1;
  if (n >= NIL_LENGTH) return fail("wire: value too long");
  put_u32(b, (uint32_t)n);
  for (int64_t i = 0; i < n; i++) {
    JSValue e = JS_GetPropertyInt64(ctx, v, i);
    if (JS_IsException(e)) return -1;
    int r = encode(b, e, t->elem, depth + 1);
    JS_FreeValue(ctx, e);
    if (r < 0) {
      char seg[24];
      return unwind(r, seg, (size_t)snprintf(seg, sizeof seg, "[%lld]", (long long)i));
    }
  }
  return 0;
}

static int encode_map(buf_t *b, JSValueConst v, const type_t *t, int depth) {
  if (JS_IsNull(v) || JS_IsUndefined(v)) {
    put_u32(b, NIL_LENGTH);
    return 0;
  }
  if (!JS_IsObject(v) || JS_IsArray(v) || JS_IsFunction(ctx, v)) return fail("want an object, got %s", type_name(v));
  JSPropertyEnum *props;
  uint32_t n;
  if (JS_GetOwnPropertyNames(ctx, &props, &n, v, JS_GPN_STRING_MASK | JS_GPN_ENUM_ONLY) < 0) return -1;
  int r = 0;
  put_u32(b, n);
  for (uint32_t i = 0; i < n && r == 0; i++) {
    size_t kn;
    const char *k = JS_AtomToCStringLen(ctx, &kn, props[i].atom);
    if (!k) {
      r = -1;
      break;
    }
    put_str(b, k, kn);
    JSValue e = JS_GetProperty(ctx, v, props[i].atom);
    r = JS_IsException(e) ? -1 : encode(b, e, t->elem, depth + 1);
    JS_FreeValue(ctx, e);
    unwind(unwind(unwind(r, "\"]", 2), k, kn), "[\"", 2); // ["key"], added back to front
    JS_FreeCString(ctx, k);
  }
  JS_FreePropertyEnum(ctx, props, n);
  return r;
}

static int encode_struct(buf_t *b, JSValueConst v, const type_t *t, int depth) {
  if (!JS_IsObject(v) || JS_IsArray(v) || JS_IsFunction(ctx, v)) return fail("want an object, got %s", type_name(v));
  for (uint32_t f = 0; f < t->nfields; f++) {
    const field_t *fd = &t->fields[f];
    JSValue e = JS_GetProperty(ctx, v, fd->atom);
    if (JS_IsException(e)) return -1;
    if (JS_IsUndefined(e)) {
      encode_zero(b, fd->type); // an absent field is the zero value, as in Go
      continue;
    }
    int r = encode(b, e, fd->type, depth + 1);
    JS_FreeValue(ctx, e);
    if (r < 0) return unwind(unwind(r, fd->name, strlen(fd->name)), ".", 1);
  }
  return 0;
}

static int encode(buf_t *b, JSValueConst v, uint32_t ti, int depth) {
  const type_t *t = &types[ti];
  if (depth > MAX_DEPTH) return fail("wire: value nested too deeply");
  switch (t->kind) {
  case K_BOOL:
    if (!JS_IsBool(v)) return fail("want a boolean, got %s", type_name(v));
    put_u8(b, JS_ToBool(ctx, v));
    return 0;
  case K_INT:
  case K_UINT:
    return encode_int(b, v, t);
  case K_FLOAT: {
    double d;
    if (number(v, &d, "a number") < 0) return -1;
    if (t->bits == 32) d = (double)(float)d; // as Go's float32(x): rounded, ±Inf beyond the float32 range
    uint64_t u;
    memcpy(&u, &d, 8);
    put_u64(b, u);
    return 0;
  }
  case K_STRING: {
    if (!JS_IsString(v)) return fail("want a string, got %s", type_name(v));
    size_t n;
    const char *s = JS_ToCStringLen(ctx, &n, v);
    if (!s) return -1;
    put_str(b, s, n);
    JS_FreeCString(ctx, s);
    return 0;
  }
  case K_BYTES:
    return encode_bytes(b, v);
  case K_TIME:
    return encode_time(b, v);
  case K_ERROR: {
    if (JS_IsNull(v) || JS_IsUndefined(v)) {
      put_u8(b, 0);
      return 0;
    }
    size_t n;
    const char *s = describe(v, &n);
    if (!s) return -1;
    put_u8(b, 1);
    put_str(b, s, n);
    JS_FreeCString(ctx, s);
    return 0;
  }
  case K_SLICE:
    return encode_list(b, v, t, depth);
  case K_MAP:
    return encode_map(b, v, t, depth);
  case K_POINTER:
    if (JS_IsNull(v) || JS_IsUndefined(v)) {
      put_u8(b, 0);
      return 0;
    }
    put_u8(b, 1);
    return encode(b, v, t->elem, depth + 1);
  case K_STRUCT:
    return encode_struct(b, v, t, depth);
  }
  return fail("invalid kind");
}

// encode_values encodes count values of the given types: argv for a host call's parameters (label "parameter",
// numbered from 1 after the context, as the Go codec numbers them), or a method's result (label "result", from
// 0): one value, or an array of count values when there are several.
static int encode_values(buf_t *b, const uint32_t *idx, uint32_t count, JSValueConst *argv, int argc,
                         const char *label, int first) {
  for (uint32_t k = 0; k < count; k++) {
    JSValueConst v = (int)k < argc ? argv[k] : JS_UNDEFINED;
    trail.len = 0;
    trail.failed = false;
    why[0] = 0;
    conv[0] = 0;
    if (encode(b, v, idx[k], 0) < 0) {
      if (why[0]) finish(label, k + (unsigned)first);
      return -1;
    }
  }
  return 0;
}

// ---- host ----

static JSValue reject(JSValue reason) {
  JSValue funcs[2];
  JSValue p = JS_NewPromiseCapability(ctx, funcs);
  if (JS_IsException(p)) {
    JS_FreeValue(ctx, reason);
    return p;
  }
  JSValue r = JS_Call(ctx, funcs[1], JS_UNDEFINED, 1, (JSValueConst *)&reason);
  JS_FreeValue(ctx, r);
  JS_FreeValue(ctx, reason);
  JS_FreeValue(ctx, funcs[0]);
  JS_FreeValue(ctx, funcs[1]);
  return p;
}

static JSValue resolve(JSValue value) {
  JSValue funcs[2];
  JSValue p = JS_NewPromiseCapability(ctx, funcs);
  if (JS_IsException(p)) {
    JS_FreeValue(ctx, value);
    return p;
  }
  JSValue r = JS_Call(ctx, funcs[0], JS_UNDEFINED, 1, (JSValueConst *)&value);
  JS_FreeValue(ctx, r);
  JS_FreeValue(ctx, value);
  JS_FreeValue(ctx, funcs[0]);
  JS_FreeValue(ctx, funcs[1]);
  return p;
}

static JSValue error_value(const char *ctor, const char *s, size_t n) {
  JSValue msg = JS_NewStringLen(ctx, s, n);
  if (JS_IsException(msg)) return msg;
  JSValue g = JS_GetGlobalObject(ctx);
  JSValue c = JS_GetPropertyStr(ctx, g, ctor);
  JSValue e = JS_CallConstructor(ctx, c, 1, (JSValueConst *)&msg);
  JS_FreeValue(ctx, c);
  JS_FreeValue(ctx, g);
  JS_FreeValue(ctx, msg);
  return e;
}

// exchange hands req to a host import's answer: it takes the pending result of length n into answer.
static bool take(uint32_t n) {
  answer.len = 0;
  if (!grow(&answer, n)) return false;
  plg_take_result((uint32_t)(uintptr_t)answer.p, n);
  answer.len = n;
  return true;
}

// results turns an answer's encoded results into the JS value a call returns: undefined, the value, or an array.
static JSValue results(rd_t *r, const method_t *m) {
  decode_err = NULL;
  if (m->nresults == 1) return decode(r, m->results[0], 0);
  if (m->nresults == 0) return JS_UNDEFINED;
  JSValue a = JS_NewArray(ctx);
  for (uint32_t k = 0; k < m->nresults && !JS_IsException(a); k++) {
    JSValue v = decode(r, m->results[k], 0);
    if (JS_IsException(v) || JS_DefinePropertyValueUint32(ctx, a, k, v, JS_PROP_C_W_E) < 0) {
      JS_FreeValue(ctx, a);
      a = JS_EXCEPTION;
    }
  }
  return a;
}

// host_method calls the Host method magic: it returns a Promise of its results, rejected with an Error carrying
// the host's message when it fails (or a TypeError when the arguments do not fit its parameters).
static JSValue host_method(JSContext *c, JSValueConst this_val, int argc, JSValueConst *argv, int magic) {
  (void)c;
  (void)this_val;
  const method_t *m = &methods[nplugin + (uint32_t)magic];
  req.len = 0;
  req.failed = false;
  if (encode_values(&req, m->params, m->nparams, argv, argc, "parameter", 1) < 0) {
    if (!conv[0]) return JS_EXCEPTION;
    char msg[1200];
    int n = snprintf(msg, sizeof msg, "host.%s: %s", m->name, conv);
    return reject(error_value("TypeError", msg, (size_t)n));
  }
  if (req.failed) return JS_ThrowOutOfMemory(ctx);
  uint32_t n = plg_host_call((uint32_t)magic, (uint32_t)(uintptr_t)req.p, (uint32_t)req.len);
  if (!take(n)) return JS_ThrowOutOfMemory(ctx);
  rd_t r = {answer.p, answer.len, 0};
  uint8_t status;
  if (!rd_u8(&r, &status)) return JS_ThrowInternalError(ctx, "plg: empty host answer");
  if (status == ST_OK) {
    JSValue v = results(&r, m);
    if (JS_IsException(v)) {
      if (decode_err) return JS_ThrowInternalError(ctx, "plg: host.%s: malformed answer: %s", m->name, decode_err);
      return v;
    }
    return resolve(v);
  }
  const char *s;
  uint32_t sn;
  if (!rd_str(&r, &s, &sn)) return JS_ThrowInternalError(ctx, "plg: host.%s: malformed answer", m->name);
  return reject(error_value("Error", s, sn));
}

// ---- natives for the prelude ----

// write(fd, text) writes text to stdout (1) or stderr (2).
static JSValue native_write(JSContext *c, JSValueConst this_val, int argc, JSValueConst *argv) {
  (void)this_val;
  int32_t fd = 1;
  if (argc < 2 || JS_ToInt32(c, &fd, argv[0]) < 0) return JS_EXCEPTION;
  size_t n;
  const char *s = JS_ToCStringLen(c, &n, argv[1]);
  if (!s) return JS_EXCEPTION;
  write_all(fd == 2 ? 2 : 1, s, n);
  JS_FreeCString(c, s);
  return JS_UNDEFINED;
}

// ---- multiplexed steps: state ----

// In a multiplexed step (plg_start, plg_resume), fetch hands its request to the host with http_start and gets a
// Promise, settled by the plg_resume step that delivers the answer; meanwhile the instance serves other calls.
// A request belongs to the call whose step made it: the call started, or the one whose answer was delivered.
typedef struct pending_call {
  JSValue promise; // what the method returned
  uint32_t call, method;
} pending_call;

typedef struct pending_request {
  JSValue resolve, reject;
  uint32_t id, call;
} pending_request;

static bool stepping;          // a multiplexed step is running
static uint32_t current_call;  // the call the running step belongs to
static pending_call *calls;    // calls waiting for their Promise to settle
static uint32_t ncalls, capcalls;
static pending_request *requests; // requests waiting for their answer
static uint32_t nrequests, caprequests;

// http_answer turns an http_roundtrip answer into [status, [[name, value]...], body Uint8Array], or throws a
// TypeError with the host's reason for a refused or failed request.
static JSValue http_answer(JSContext *c, rd_t *r) {
  uint8_t status;
  const char *s;
  uint32_t sn;
  if (!rd_u8(r, &status)) return JS_ThrowInternalError(c, "plg: empty HTTP answer");
  if (status != ST_OK) {
    if (!rd_str(r, &s, &sn)) return JS_ThrowInternalError(c, "plg: malformed HTTP answer");
    return JS_Throw(c, error_value("TypeError", s, sn));
  }
  uint64_t code, count;
  if (!rd_u64(r, &code) || !rd_u64(r, &count) || count > (r->len - r->off) / 8)
    return JS_ThrowInternalError(c, "plg: malformed HTTP answer");
  JSValue headers = JS_NewArray(c);
  for (uint64_t i = 0; i < count && !JS_IsException(headers); i++) {
    const char *ns, *vs;
    uint32_t nn, vn;
    if (!rd_str(r, &ns, &nn) || !rd_str(r, &vs, &vn)) {
      JS_FreeValue(c, headers);
      return JS_ThrowInternalError(c, "plg: malformed HTTP answer");
    }
    JSValue pair[2] = {JS_NewStringLen(c, ns, nn), JS_NewStringLen(c, vs, vn)};
    JSValue p = JS_NewArrayFrom(c, 2, pair);
    if (JS_IsException(p) || JS_DefinePropertyValueUint32(c, headers, (uint32_t)i, p, JS_PROP_C_W_E) < 0) {
      JS_FreeValue(c, headers);
      headers = JS_EXCEPTION;
    }
  }
  if (JS_IsException(headers)) return headers;
  JSValue parts[3] = {
      JS_NewInt64(c, (int64_t)code),
      headers,
      JS_NewUint8ArrayCopy(c, r->p + r->off, r->len - r->off),
  };
  if (JS_IsException(parts[2])) {
    JS_FreeValue(c, headers);
    return JS_EXCEPTION;
  }
  return JS_NewArrayFrom(c, 3, parts);
}

// http_async starts the request in req with http_start and returns a Promise of its answer.
static JSValue http_async(JSContext *c) {
  if (nrequests == caprequests) {
    uint32_t cap = caprequests ? 2 * caprequests : 16;
    pending_request *grown = realloc(requests, cap * sizeof *grown);
    if (!grown) return JS_ThrowOutOfMemory(c);
    requests = grown;
    caprequests = cap;
  }
  JSValue funcs[2];
  JSValue p = JS_NewPromiseCapability(c, funcs);
  if (JS_IsException(p)) return p;
  uint32_t id = plg_http_start(current_call, (uint32_t)(uintptr_t)req.p, (uint32_t)req.len);
  requests[nrequests++] = (pending_request){.resolve = funcs[0], .reject = funcs[1], .id = id, .call = current_call};
  return p;
}

// http(method, url, [[name, value]...], body Uint8Array | null) makes a request through the host and returns
// [status, [[name, value]...], body Uint8Array], or a Promise of it in a multiplexed step; a refused or failed
// request throws (rejects with) a TypeError, as fetch does.
static JSValue native_http(JSContext *c, JSValueConst this_val, int argc, JSValueConst *argv) {
  (void)this_val;
  if (argc < 4 || !JS_IsArray(argv[2])) return JS_ThrowTypeError(c, "plg: http: bad arguments");
  req.len = 0;
  req.failed = false;
  for (int i = 0; i < 2; i++) {
    size_t n;
    const char *s = JS_ToCStringLen(c, &n, argv[i]);
    if (!s) return JS_EXCEPTION;
    put_str(&req, s, n);
    JS_FreeCString(c, s);
  }
  int64_t pairs;
  if (JS_GetLength(c, argv[2], &pairs) < 0) return JS_EXCEPTION;
  put_u64(&req, (uint64_t)pairs);
  for (int64_t i = 0; i < pairs; i++) {
    JSValue pair = JS_GetPropertyInt64(c, argv[2], i);
    for (uint32_t k = 0; k < 2; k++) {
      JSValue part = JS_GetPropertyUint32(c, pair, k);
      size_t n;
      const char *s = JS_IsException(part) ? NULL : JS_ToCStringLen(c, &n, part);
      JS_FreeValue(c, part);
      if (!s) {
        JS_FreeValue(c, pair);
        return JS_EXCEPTION;
      }
      put_str(&req, s, n);
      JS_FreeCString(c, s);
    }
    JS_FreeValue(c, pair);
  }
  if (!JS_IsNull(argv[3]) && !JS_IsUndefined(argv[3])) {
    size_t n;
    uint8_t *p = JS_GetUint8Array(c, &n, argv[3]);
    if (!p && n) return JS_EXCEPTION;
    put(&req, p, n);
  }
  if (req.failed) return JS_ThrowOutOfMemory(c);
  if (stepping) return http_async(c);
  uint32_t n = plg_http_roundtrip((uint32_t)(uintptr_t)req.p, (uint32_t)req.len);
  if (!take(n)) return JS_ThrowOutOfMemory(c);
  rd_t r = {answer.p, answer.len, 0};
  return http_answer(c, &r);
}

// utf8Decode(bytes) returns the text of UTF-8 bytes.
static JSValue native_decode(JSContext *c, JSValueConst this_val, int argc, JSValueConst *argv) {
  (void)this_val;
  size_t n = 0;
  uint8_t *p = argc ? JS_GetUint8Array(c, &n, argv[0]) : NULL;
  if (!p && n) return JS_EXCEPTION;
  if (!p) return JS_NewStringLen(c, "", 0);
  return JS_NewStringLen(c, (const char *)p, n);
}

// utf8Encode(text) returns the UTF-8 bytes of text.
static JSValue native_encode(JSContext *c, JSValueConst this_val, int argc, JSValueConst *argv) {
  (void)this_val;
  size_t n;
  const char *s = JS_ToCStringLen(c, &n, argc ? argv[0] : JS_UNDEFINED);
  if (!s) return JS_EXCEPTION;
  JSValue v = JS_NewUint8ArrayCopy(c, (const uint8_t *)s, n);
  JS_FreeCString(c, s);
  return v;
}

// ---- exports ----

EXPORT("plg_alloc") uint32_t plg_alloc(uint32_t size) {
  in.len = 0;
  in.failed = false;
  if (!grow(&in, size)) return 0;
  return (uint32_t)(uintptr_t)in.p;
}

static bool setup(void) {
  rt = JS_NewRuntime2(&malloc_functions, NULL);
  if (!rt) return false;
  JS_SetMaxStackSize(rt, JS_STACK_LIMIT);
  ctx = JS_NewContext(rt);
  if (!ctx) return false;
  a_seconds = JS_NewAtom(ctx, "seconds");
  a_nanos = JS_NewAtom(ctx, "nanos");
  a_message = JS_NewAtom(ctx, "message");
  a_getTime = JS_NewAtom(ctx, "getTime");
  a_close = JS_NewAtom(ctx, "Close");
  a_create = JS_NewAtom(ctx, "Create");
  return true;
}

// globals installs the host object and runs the prelude, which installs console, fetch, Headers and Response.
static bool globals(void) {
  JSValue g = JS_GetGlobalObject(ctx);
  JSValue host = JS_NewObject(ctx);
  bool ok = !JS_IsException(host);
  for (uint32_t i = nplugin; ok && i < nmethods; i++) {
    JSValue fn = JS_NewCFunctionMagic(ctx, host_method, methods[i].name, (int)methods[i].nparams,
                                      JS_CFUNC_generic_magic, (int)(i - nplugin));
    ok = !JS_IsException(fn) && JS_DefinePropertyValue(ctx, host, methods[i].atom, fn, JS_PROP_ENUMERABLE) >= 0;
  }
  ok = ok && JS_DefinePropertyValueStr(ctx, g, "host", host, JS_PROP_CONFIGURABLE | JS_PROP_WRITABLE) >= 0;
  JSValue natives = JS_NewObject(ctx);
  ok = ok && !JS_IsException(natives) &&
       JS_SetPropertyStr(ctx, natives, "write", JS_NewCFunction(ctx, native_write, "write", 2)) >= 0 &&
       JS_SetPropertyStr(ctx, natives, "http", JS_NewCFunction(ctx, native_http, "http", 4)) >= 0 &&
       JS_SetPropertyStr(ctx, natives, "utf8Decode", JS_NewCFunction(ctx, native_decode, "utf8Decode", 1)) >= 0 &&
       JS_SetPropertyStr(ctx, natives, "utf8Encode", JS_NewCFunction(ctx, native_encode, "utf8Encode", 1)) >= 0;
  JSValue fn = ok ? JS_Eval(ctx, prelude, sizeof prelude - 1, "plg:prelude", JS_EVAL_TYPE_GLOBAL) : JS_EXCEPTION;
  JSValue r = JS_IsException(fn) ? JS_EXCEPTION : JS_Call(ctx, fn, JS_UNDEFINED, 1, (JSValueConst *)&natives);
  ok = ok && !JS_IsException(r);
  JS_FreeValue(ctx, r);
  JS_FreeValue(ctx, fn);
  JS_FreeValue(ctx, natives);
  JS_FreeValue(ctx, g);
  return ok;
}

// plg_init loads the plugin: payload = name source methodCount schema (internal/scriptabi).
EXPORT("plg_init") uint64_t plg_init(uint32_t len) {
  if (rt) return message(ST_ABI, "plg_init called twice", 21);
  if (len > in.cap) return message(ST_ABI, "init payload beyond the input buffer", 36);
  oom = false;
  rd_t r = {in.p, len, 0};
  const char *name, *source;
  uint32_t name_len, source_len;
  if (!rd_str(&r, &name, &name_len) || !rd_str(&r, &source, &source_len) || !rd_u32(&r, &nplugin))
    return message(ST_ABI, "malformed init payload", 22);
  if (!setup()) return message(ST_PANIC, "out of memory", 13);
  if (!read_schema(&r)) return oom ? message(ST_PANIC, "out of memory", 13) : message(ST_ABI, "malformed schema", 16);
  if (!globals()) return failure(ST_ABI, "plg: prelude: ");

  // JS_Eval needs a NUL-terminated script and file name; the input buffer may be reused, so copy both.
  char *file = dup_name(name, name_len);
  char *script = malloc((size_t)source_len + 1);
  if (!file || !script) {
    free(file);
    free(script);
    return message(ST_PANIC, "out of memory", 13);
  }
  memcpy(script, source, source_len);
  script[source_len] = 0;
  JSValue v = JS_Eval(ctx, script, source_len, file, JS_EVAL_TYPE_GLOBAL);
  free(script);
  free(file);
  if (JS_IsException(v)) return failure(ST_LOAD, NULL);
  JS_FreeValue(ctx, v);
  drain(JS_UNDEFINED);

  JSValue g = JS_GetGlobalObject(ctx);
  JSValue exports = JS_GetPropertyStr(ctx, g, "__plg_plugin");
  JS_FreeValue(ctx, g);
  JSValue create = JS_IsObject(exports) ? JS_GetProperty(ctx, exports, a_create) : JS_UNDEFINED;
  JS_FreeValue(ctx, exports);
  if (JS_IsException(create)) return failure(ST_LOAD, NULL);
  if (!JS_IsFunction(ctx, create)) {
    JS_FreeValue(ctx, create);
    return message(ST_LOAD, "the plugin exports no function Create", 37);
  }
  v = settle(JS_Call(ctx, create, JS_UNDEFINED, 0, NULL));
  JS_FreeValue(ctx, create);
  if (JS_IsException(v)) return failure(ST_ERROR, NULL);
  if (!JS_IsObject(v)) {
    uint64_t res = messagef(ST_LOAD, "Create returned %s, not an object", type_name(v));
    JS_FreeValue(ctx, v);
    return res;
  }
  for (uint32_t i = 0; i < nplugin; i++) {
    JSValue fn = JS_GetProperty(ctx, v, methods[i].atom);
    bool callable = JS_IsFunction(ctx, fn);
    JS_FreeValue(ctx, fn);
    if (JS_IsException(fn)) {
      JS_FreeValue(ctx, v);
      return failure(ST_LOAD, NULL);
    }
    if (!callable) {
      JS_FreeValue(ctx, v);
      return messagef(ST_LOAD, "the object Create returned has no method %s", methods[i].name);
    }
  }
  inst = v;
  begin(ST_OK);
  return packed();
}

// encode_result encodes a method's settled result into out: OK + results, or Convert naming the result.
static uint64_t encode_result(const method_t *m, JSValue v) {
  begin(ST_OK);
  int r = 0;
  if (m->nresults == 1) {
    r = encode_values(&out, m->results, 1, (JSValueConst *)&v, 1, "result", 0);
  } else if (m->nresults > 1) {
    int64_t n = -1;
    if (!JS_IsArray(v) || JS_GetLength(ctx, v, &n) < 0 || n != m->nresults) {
      if (JS_HasException(ctx)) JS_FreeValue(ctx, JS_GetException(ctx));
      char msg[160];
      int k = snprintf(msg, sizeof msg, "results: want an array of %u values, got %s", m->nresults,
                       JS_IsArray(v) ? "an array of another length" : type_name(v));
      return message(ST_CONVERT, msg, (size_t)k);
    }
    JSValue *vals = calloc(m->nresults, sizeof *vals);
    if (!vals) return message(ST_PANIC, "out of memory", 13);
    for (uint32_t k = 0; k < m->nresults; k++) vals[k] = JS_GetPropertyUint32(ctx, v, k);
    begin(ST_OK);
    r = encode_values(&out, m->results, m->nresults, vals, (int)m->nresults, "result", 0);
    for (uint32_t k = 0; k < m->nresults; k++) JS_FreeValue(ctx, vals[k]);
    free(vals);
  }
  if (r < 0) {
    if (!conv[0]) return failure(ST_ERROR, NULL);
    return message(ST_CONVERT, conv, strlen(conv));
  }
  return packed();
}

// invoke decodes the arguments of the interface method with the given index from the input buffer and calls the
// method: it returns 0 with what the method returned (a value, a Promise or an exception) in *v, or the result of
// a request that breaks the ABI.
static uint64_t invoke(uint32_t method, uint32_t len, JSValue *v) {
  if (!rt || JS_IsUndefined(inst) || closed) return message(ST_ABI, "plugin not loaded", 17);
  if (method >= nplugin) return message(ST_ABI, "unknown method", 14);
  if (len > in.cap) return message(ST_ABI, "arguments beyond the input buffer", 33);
  oom = false;
  const method_t *m = &methods[method];
  rd_t r = {in.p, len, 0};
  JSValue *args = calloc(m->nparams ? m->nparams : 1, sizeof *args);
  if (!args) return message(ST_PANIC, "out of memory", 13);
  uint32_t got = 0;
  decode_err = NULL;
  for (; got < m->nparams; got++) {
    args[got] = decode(&r, m->params[got], 0);
    if (JS_IsException(args[got])) break;
  }
  if (got < m->nparams || r.off != r.len) {
    for (uint32_t k = 0; k < got; k++) JS_FreeValue(ctx, args[k]);
    free(args);
    if (got == m->nparams) return message(ST_ABI, "trailing argument bytes", 23);
    if (decode_err) return messagef(ST_ABI, "malformed arguments: %s", decode_err);
    return failure(ST_ABI, "plg: decoding the arguments: ");
  }
  JSValue fn = JS_GetProperty(ctx, inst, m->atom);
  if (JS_IsFunction(ctx, fn)) {
    *v = JS_Call(ctx, fn, inst, (int)m->nparams, args);
  } else if (JS_IsException(fn)) {
    *v = JS_EXCEPTION;
  } else {
    *v = JS_ThrowTypeError(ctx, "the plugin's %s is not a function", m->name);
  }
  JS_FreeValue(ctx, fn);
  for (uint32_t k = 0; k < m->nparams; k++) JS_FreeValue(ctx, args[k]);
  free(args);
  return 0;
}

EXPORT("plg_call") uint64_t plg_call(uint32_t method, uint32_t len) {
  JSValue v;
  uint64_t bad = invoke(method, len, &v);
  if (bad) return bad;
  v = settle(v);
  if (JS_IsException(v)) return failure(ST_ERROR, NULL);
  uint64_t res = encode_result(&methods[method], v);
  JS_FreeValue(ctx, v);
  if (out.failed) oom = true;
  return res;
}

// ---- multiplexed steps: plg_start, plg_resume and plg_drop ----

// A step runs the call it starts (or the code a request's answer resumes) and every pending job, then lists the
// calls whose Promise settled meanwhile, possibly several (calls may await one another's work).

static buf_t steps;       // the result buffer of steps
static uint32_t finished; // calls the step in progress finished
static bool step_deep;    // a finished call's result carried the deep-stack flag

static const char never_settles[] = "the returned promise never settles: no pending job or HTTP request is left";

static void begin_step(uint32_t call) {
  steps.len = 0;
  steps.failed = false;
  put_u8(&steps, ST_OK);
  put_u32(&steps, 0);
  finished = 0;
  step_deep = false;
  oom = false;
  stepping = true;
  current_call = call;
}

static uint64_t end_step(void) {
  stepping = false;
  if (steps.failed) return ((uint64_t)(uintptr_t)oom_result << 32) | sizeof oom_result;
  memcpy(steps.p + 1, &finished, sizeof finished);
  if (ran_deep() || step_deep) steps.p[0] |= ST_DEEP_STACK;
  return ((uint64_t)(uintptr_t)steps.p << 32) | steps.len;
}

// complete lists call as finished with the result buffer res (packed ptr<<32 | len); a deep-stack flag on the
// call's status moves to the step's.
static void complete(uint32_t call, uint64_t res) {
  uint32_t n = (uint32_t)res;
  put_u32(&steps, call);
  put_u32(&steps, n);
  size_t at = steps.len;
  put(&steps, (const void *)(uintptr_t)(res >> 32), n);
  if (!steps.failed && n > 0 && (steps.p[at] & ST_DEEP_STACK)) {
    steps.p[at] &= (uint8_t)~ST_DEEP_STACK;
    step_deep = true;
  }
  finished++;
}

// conclude finishes a call whose method returned v (taken over), unless v is a pending Promise: then the call
// waits in calls (which has room: plg_start made it).
static void conclude(uint32_t call, uint32_t method, JSValue v) {
  const method_t *m = &methods[method];
  if (JS_IsException(v)) {
    complete(call, failure(ST_ERROR, NULL));
    return;
  }
  if (JS_IsPromise(v)) {
    switch (JS_PromiseState(ctx, v)) {
    case JS_PROMISE_PENDING:
      calls[ncalls++] = (pending_call){.promise = v, .call = call, .method = method};
      return;
    case JS_PROMISE_REJECTED:
      JS_PromiseMarkAsHandled(ctx, v);
      JS_Throw(ctx, JS_PromiseResult(ctx, v));
      JS_FreeValue(ctx, v);
      complete(call, failure(ST_ERROR, NULL));
      return;
    default: {
      JSValue res = JS_PromiseResult(ctx, v);
      JS_FreeValue(ctx, v);
      v = res;
    }
    }
  }
  complete(call, encode_result(m, v));
  JS_FreeValue(ctx, v);
  if (out.failed) oom = true;
}

// harvest finishes the waiting calls whose Promise settled, reporting whether there were any.
static bool harvest(void) {
  bool any = false;
  for (uint32_t i = 0; i < ncalls;) {
    pending_call pc = calls[i];
    if (JS_PromiseState(ctx, pc.promise) == JS_PROMISE_PENDING) {
      i++;
      continue;
    }
    calls[i] = calls[--ncalls];
    current_call = pc.call;
    conclude(pc.call, pc.method, pc.promise);
    any = true;
  }
  return any;
}

// advance runs pending jobs and finishes the calls whose Promise settled, until nothing moves. With no request
// in flight, a call still waiting can never be answered: it fails, as plg_call's would.
static void advance(void) {
  do {
    drain(JS_UNDEFINED);
  } while (harvest());
  if (nrequests > 0) return;
  while (ncalls > 0) {
    pending_call pc = calls[--ncalls];
    JS_FreeValue(ctx, pc.promise);
    complete(pc.call, message(ST_ERROR, never_settles, sizeof never_settles - 1));
  }
}

// plg_start starts call number call of the interface method with the given index (arguments as for plg_call).
EXPORT("plg_start") uint64_t plg_start(uint32_t call, uint32_t method, uint32_t len) {
  for (uint32_t i = 0; i < ncalls; i++)
    if (calls[i].call == call) return message(ST_ABI, "call number already in flight", 29);
  if (ncalls == capcalls) {
    uint32_t cap = capcalls ? 2 * capcalls : 16;
    pending_call *grown = realloc(calls, cap * sizeof *grown);
    if (!grown) return message(ST_PANIC, "out of memory", 13);
    calls = grown;
    capcalls = cap;
  }
  begin_step(call);
  JSValue v;
  uint64_t bad = invoke(method, len, &v);
  if (bad) {
    stepping = false;
    return bad;
  }
  conclude(call, method, v);
  advance();
  return end_step();
}

// plg_resume delivers the answer to request number request (an http_roundtrip answer) to the fetch waiting for
// it.
EXPORT("plg_resume") uint64_t plg_resume(uint32_t request, uint32_t len) {
  if (!rt || JS_IsUndefined(inst) || closed) return message(ST_ABI, "plugin not loaded", 17);
  if (len > in.cap) return message(ST_ABI, "answer beyond the input buffer", 30);
  uint32_t i = 0;
  while (i < nrequests && requests[i].id != request) i++;
  if (i == nrequests) return message(ST_ABI, "no call waits for this request", 30);
  pending_request pr = requests[i];
  requests[i] = requests[--nrequests];
  begin_step(pr.call);
  rd_t r = {in.p, len, 0};
  JSValue v = http_answer(ctx, &r);
  JSValue fn = pr.resolve;
  if (JS_IsException(v)) {
    v = JS_GetException(ctx);
    fn = pr.reject;
  }
  JS_FreeValue(ctx, JS_Call(ctx, fn, JS_UNDEFINED, 1, (JSValueConst *)&v));
  JS_FreeValue(ctx, v);
  JS_FreeValue(ctx, pr.resolve);
  JS_FreeValue(ctx, pr.reject);
  advance();
  return end_step();
}

// plg_drop forgets call number call, which the host abandoned with none of its requests in flight: its Promise is
// released and the call is never listed as finished. Nothing runs meanwhile; code still awaiting what another call
// settles later runs then, as any pending job, its result reaching nobody.
EXPORT("plg_drop") uint64_t plg_drop(uint32_t call) {
  if (!rt || JS_IsUndefined(inst) || closed) return message(ST_ABI, "plugin not loaded", 17);
  for (uint32_t i = 0; i < ncalls; i++) {
    if (calls[i].call != call) continue;
    JSValue promise = calls[i].promise;
    calls[i] = calls[--ncalls];
    JS_FreeValue(ctx, promise);
    begin(ST_OK);
    return packed();
  }
  return message(ST_ABI, "no such call waits", 18);
}

// plg_close calls the plugin's optional Close method, awaiting it when it returns a Promise.
EXPORT("plg_close") uint64_t plg_close(void) {
  if (!rt || JS_IsUndefined(inst) || closed) {
    begin(ST_OK);
    return packed();
  }
  closed = true;
  oom = false;
  JSValue fn = JS_GetProperty(ctx, inst, a_close);
  if (JS_IsException(fn)) return failure(ST_ERROR, NULL);
  if (!JS_IsFunction(ctx, fn)) {
    JS_FreeValue(ctx, fn);
    begin(ST_OK);
    return packed();
  }
  JSValue v = settle(JS_Call(ctx, fn, inst, 0, NULL));
  JS_FreeValue(ctx, fn);
  if (JS_IsException(v)) return failure(ST_ERROR, NULL);
  JS_FreeValue(ctx, v);
  begin(ST_OK);
  return packed();
}
