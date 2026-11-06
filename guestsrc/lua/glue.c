/*
** The Lua guest's ABI (plg_alloc, plg_init, plg_call, plg_close), its sandboxed Lua state and `require`.
**
** plg_init receives the script init payload (internal/scriptabi): the entrypoint's path in the /plugin mount, an
** unused source, the interface's method count, whether the plugins validate and the wire schema of the
** interface's methods followed by Validate, when they validate, and the Host's. It runs the entrypoint, calls the
** global Create and keeps the table it returns; plg_call(i) calls inst:<method i>(args...), Validate included,
** which the table need not have: without it the call fails with PLG_LOAD. Every result buffer starts with a
** status byte (internal/wire).
*/
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "lauxlib.h"
#include "lua.h"
#include "lualib.h"
#include "plg.h"

#define EXPORT(name) __attribute__((export_name(name)))

/* The plugin's files are mounted read-only here; require resolves nothing else. */
#define PLUGIN_ROOT "/plugin/"

/* wire.MaxMessage: status messages are cut to 64 KiB. */
#define MAX_MESSAGE (64 << 10)

/* The longest module name require accepts. */
#define MAX_MODULE_NAME 200

plg_schema plg_the_schema;
static uint32_t iface_methods; /* the interface's: the schema's first methods */
uint32_t plg_host_base;

static lua_State *L;
static uint8_t *inbuf;
static uint32_t incap;
static plg_buf out; /* the result buffer the host reads after each step */
static int instref = LUA_NOREF;
static int initialized;

/* The step in progress, read by the protected functions below. */
static plg_reader args;
static uint32_t calling;
static const char *entrypoint;
static size_t entrypoint_len;

/* The status an error raised by the step in progress maps to (a memory error is always PLG_PANIC). */
static int fail_status;

/* What every result degrades to when the result buffer itself cannot grow. */
static uint8_t oom_result[] = {PLG_PANIC, 13, 0, 0, 0, 'o', 'u', 't', ' ', 'o', 'f', ' ', 'm', 'e', 'm', 'o', 'r', 'y'};

static uint64_t packed(const uint8_t *p, size_t n) {
  return (uint64_t)(uintptr_t)p << 32 | (uint32_t)n;
}

static uint64_t finish(void) {
  if (out.oom) return packed(oom_result, sizeof oom_result);
  return packed(out.data, out.len);
}

/* utf8_cut returns the longest prefix of s, at most max bytes, that ends on a UTF-8 boundary. */
static size_t utf8_cut(const char *s, size_t n, size_t max) {
  if (n <= max) return n;
  while (max > 0 && ((unsigned char)s[max] & 0xC0) == 0x80) max--;
  return max;
}

static uint64_t message(int status, const char *msg, size_t n) {
  out.len = 0;
  out.oom = 0;
  plg_buf_put(&out, &(uint8_t){(uint8_t)status}, 1);
  plg_buf_str(&out, msg, utf8_cut(msg, n, MAX_MESSAGE));
  return finish();
}

static uint64_t cmessage(int status, const char *msg) {
  return message(status, msg, strlen(msg));
}

/* The result of a step without results. */
static uint64_t ok_result(void) {
  static const uint8_t ok[] = {PLG_OK};
  return packed(ok, sizeof ok);
}

/*
** The allocator: wasm memory has a hard limit, so a failed malloc becomes a Lua memory error. starved records
** that an allocation failed during the current step. Lua raises the memory error after an emergency collection;
** a plugin may catch it, and its garbage then still fills the instance, so the step ends with a full collection
** (collect_if_starved).
*/
static int starved;

static void *l_alloc(void *ud, void *ptr, size_t osize, size_t nsize) {
  (void)ud;
  (void)osize;
  if (nsize == 0) {
    free(ptr);
    return NULL;
  }
  void *grown = realloc(ptr, nsize);
  if (grown == NULL) starved = 1;
  return grown;
}

static void collect_if_starved(void) {
  if (starved) {
    starved = 0;
    lua_gc(L, LUA_GCCOLLECT);
  }
}

EXPORT("plg_alloc") uint32_t plg_alloc(uint32_t size) {
  if (size > incap) {
    free(inbuf);
    inbuf = malloc(size);
    if (inbuf == NULL) __builtin_trap(); /* no memory for the request: the host discards the instance */
    incap = size;
  }
  return (uint32_t)(uintptr_t)inbuf;
}

/* ---- errors ---- */

static int tostring_protected(lua_State *L) {
  luaL_tolstring(L, 1, NULL);
  return 1;
}

/* failed turns the error of a failed lua_pcall (on top of the stack) into a result. */
static uint64_t failed(int code) {
  if (code == LUA_ERRMEM) {
    fprintf(stderr, "lua: not enough memory\n");
    lua_settop(L, 0);
    return cmessage(PLG_PANIC, "not enough memory");
  }
  size_t n;
  const char *msg;
  if (lua_type(L, -1) == LUA_TSTRING) {
    msg = lua_tolstring(L, -1, &n);
  } else {
    lua_pushcfunction(L, tostring_protected);
    lua_insert(L, -2);
    if (lua_pcall(L, 1, 1, 0) == LUA_OK && lua_type(L, -1) == LUA_TSTRING) {
      msg = lua_tolstring(L, -1, &n);
    } else {
      msg = "(error object is not a string)";
      n = strlen(msg);
    }
  }
  uint64_t res = message(fail_status, msg, n);
  lua_settop(L, 0);
  return res;
}

/* ---- loading files from /plugin ---- */

typedef struct file_reader {
  FILE *f;
  char buf[4096];
} file_reader;

static const char *read_file(lua_State *L, void *ud, size_t *size) {
  (void)L;
  file_reader *fr = ud;
  *size = fread(fr->buf, 1, sizeof fr->buf, fr->f);
  return *size > 0 ? fr->buf : NULL;
}

/*
** load_plugin_file loads the text chunk at path (below /plugin), named after rel, and pushes it. It returns 0
** when the file does not exist, otherwise 1 (raising the syntax error of a chunk that does not compile).
*/
static int load_plugin_file(lua_State *L, const char *path, const char *rel) {
  file_reader fr;
  fr.f = fopen(path, "r");
  if (fr.f == NULL) return 0;
  lua_pushfstring(L, "@%s", rel);
  int status = lua_load(L, read_file, &fr, lua_tostring(L, -1), "t");
  int ioerr = ferror(fr.f);
  fclose(fr.f);
  lua_remove(L, -2);
  if (status != LUA_OK) lua_error(L);
  if (ioerr) luaL_error(L, "cannot read %s", rel);
  return 1;
}

/* valid_module reports whether name is dot-separated components of letters, digits, '_' and '-'. */
static int valid_module(const char *name, size_t n) {
  if (n == 0 || n > MAX_MODULE_NAME || name[0] == '.' || name[n - 1] == '.') return 0;
  for (size_t i = 0; i < n; i++) {
    char c = name[i];
    int ok = (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' ||
             (c == '.' && name[i + 1] != '.');
    if (!ok) return 0;
  }
  return 1;
}

/* The value package.loaded holds for a module being loaded, to detect require cycles. */
static char loading;

/*
** require(name) loads the module <name with dots as slashes>.lua, else <...>/init.lua, from the plugin's
** directory (the root of the /plugin mount) and nothing else, once: later calls return the first result. The
** standard libraries and json are preloaded.
*/
static int plg_require(lua_State *L) {
  size_t n;
  const char *name = luaL_checklstring(L, 1, &n);
  lua_settop(L, 1);
  luaL_getsubtable(L, LUA_REGISTRYINDEX, LUA_LOADED_TABLE); /* 2 */
  lua_getfield(L, 2, name);
  if (lua_touserdata(L, -1) == &loading) return luaL_error(L, "module '%s' requires itself", name);
  if (lua_toboolean(L, -1)) return 1;
  lua_pop(L, 1);
  if (!valid_module(name, n)) return luaL_error(L, "invalid module name '%s'", name);

  char rel[MAX_MODULE_NAME + 16], path[sizeof PLUGIN_ROOT + sizeof rel];
  for (size_t i = 0; i <= n; i++) rel[i] = name[i] == '.' ? '/' : name[i];
  size_t base = n;
  memcpy(rel + base, ".lua", 5);
  snprintf(path, sizeof path, PLUGIN_ROOT "%s", rel);
  if (!load_plugin_file(L, path, rel)) {
    memcpy(rel + base, "/init.lua", 10);
    snprintf(path, sizeof path, PLUGIN_ROOT "%s", rel);
    if (!load_plugin_file(L, path, rel)) {
      rel[base] = 0;
      return luaL_error(L, "module '%s' not found in the plugin (no %s.lua or %s/init.lua)", name, rel, rel);
    }
  }
  lua_pushlightuserdata(L, &loading);
  lua_setfield(L, 2, name);
  lua_pushvalue(L, 1);
  lua_pushstring(L, rel);
  int status = lua_pcall(L, 2, 1, 0);
  if (status != LUA_OK) {
    lua_pushnil(L);
    lua_setfield(L, 2, name);
    return lua_error(L);
  }
  if (lua_isnil(L, -1)) {
    lua_pop(L, 1);
    lua_pushboolean(L, 1);
  }
  lua_pushvalue(L, -1);
  lua_setfield(L, 2, name);
  return 1;
}

/* ---- the sandboxed state ---- */

/* load(chunk [, chunkname [, mode [, env]]]) compiles text chunks given as strings only. */
static int safe_load(lua_State *L) {
  size_t n;
  const char *chunk = luaL_checklstring(L, 1, &n);
  const char *chunkname = luaL_optstring(L, 2, chunk);
  int env = !lua_isnone(L, 4);
  if (luaL_loadbufferx(L, chunk, n, chunkname, "t") != LUA_OK) {
    luaL_pushfail(L);
    lua_insert(L, -2);
    return 2;
  }
  if (env) {
    lua_pushvalue(L, 4);
    if (!lua_setupvalue(L, -2, 1)) lua_pop(L, 1);
  }
  return 1;
}

static const luaL_Reg libraries[] = {
    {LUA_GNAME, luaopen_base},       {LUA_COLIBNAME, luaopen_coroutine}, {LUA_TABLIBNAME, luaopen_table},
    {LUA_STRLIBNAME, luaopen_string}, {LUA_MATHLIBNAME, luaopen_math},    {LUA_UTF8LIBNAME, luaopen_utf8},
    {"json", luaopen_plg_json},       {NULL, NULL},
};

static void open_sandbox(lua_State *L) {
  for (const luaL_Reg *lib = libraries; lib->func != NULL; lib++) {
    luaL_requiref(L, lib->name, lib->func, 1);
    lua_pop(L, 1);
  }
  lua_pushnil(L);
  lua_setglobal(L, "dofile");
  lua_pushnil(L);
  lua_setglobal(L, "loadfile");
  lua_pushcfunction(L, safe_load);
  lua_setglobal(L, "load");
  lua_pushcfunction(L, plg_require);
  lua_setglobal(L, "require");
  lua_getglobal(L, LUA_STRLIBNAME);
  lua_pushnil(L);
  lua_setfield(L, -2, "dump"); /* bytecode never loads, so dumping it serves nothing */
  lua_pop(L, 1);
  plg_open_host(L);
  plg_open_http(L);
}

/* callable reports whether the value at idx can be called. */
static int callable(lua_State *L, int idx) {
  if (lua_isfunction(L, idx)) return 1;
  if (luaL_getmetafield(L, idx, "__call") == LUA_TNIL) return 0;
  lua_pop(L, 1);
  return 1;
}

/* is_failure reports whether a call's n results starting at base are Lua's `nil, err` failure convention. */
static int is_failure(lua_State *L, int base, int n) {
  return n >= 2 && lua_isnil(L, base) && !lua_isnil(L, base + 1);
}

static int init_protected(lua_State *L) {
  fail_status = PLG_LOAD;
  open_sandbox(L);

  char path[4096];
  if (entrypoint_len + sizeof PLUGIN_ROOT > sizeof path) return luaL_error(L, "entrypoint path too long");
  snprintf(path, sizeof path, PLUGIN_ROOT "%.*s", (int)entrypoint_len, entrypoint);
  if (!load_plugin_file(L, path, path + sizeof PLUGIN_ROOT - 1)) return luaL_error(L, "cannot open %s", path);
  lua_call(L, 0, 0);

  if (lua_getglobal(L, "Create") != LUA_TFUNCTION) return luaL_error(L, "the plugin defines no global function Create");
  fail_status = PLG_ERROR;
  int base = lua_gettop(L);
  lua_call(L, 0, LUA_MULTRET);
  int n = lua_gettop(L) - base + 1;
  if (is_failure(L, base, n)) {
    lua_pushvalue(L, base + 1);
    return lua_error(L);
  }
  fail_status = PLG_LOAD;
  if (n == 0 || !lua_istable(L, base))
    return luaL_error(L, "Create returned %s, want a table", n == 0 ? "nothing" : luaL_typename(L, base));
  lua_settop(L, base);
  for (uint32_t i = 0; i < iface_methods; i++) {
    lua_getfield(L, base, plg_the_schema.methods[i].name);
    if (!callable(L, -1)) return luaL_error(L, "the table Create returned has no method %s", plg_the_schema.methods[i].name);
    lua_pop(L, 1);
  }
  instref = luaL_ref(L, LUA_REGISTRYINDEX);
  return 0;
}

/* parse_init reads the init payload; it returns NULL or what is wrong with it. */
static const char *parse_init(plg_reader *r) {
  const char *source;
  size_t source_len;
  uint8_t validates;
  if (!plg_read_str(r, &entrypoint, &entrypoint_len) || !plg_read_str(r, &source, &source_len) ||
      !plg_read_u32(r, &iface_methods) || !plg_read_u8(r, &validates) || validates > 1)
    return "malformed init payload";
  if (entrypoint_len == 0 || memchr(entrypoint, 0, entrypoint_len) != NULL) return "malformed init payload: entrypoint";
  const char *err = plg_schema_parse(r, &plg_the_schema);
  if (err != NULL) return err;
  if (plg_the_schema.nmethods < validates || iface_methods > plg_the_schema.nmethods - validates)
    return "malformed init payload: method count";
  plg_host_base = iface_methods + validates;
  return NULL;
}

EXPORT("plg_init") uint64_t plg_init(uint32_t len) {
  if (initialized || L != NULL) return cmessage(PLG_ABI, "plg_init called twice");
  if (len > incap) return cmessage(PLG_ABI, "init payload beyond the input buffer");
  plg_reader r = {inbuf, inbuf + len};
  const char *err = parse_init(&r);
  if (err != NULL) return cmessage(PLG_ABI, err);
  L = lua_newstate(l_alloc, NULL);
  if (L == NULL) return cmessage(PLG_PANIC, "not enough memory");
  lua_pushcfunction(L, init_protected);
  int code = lua_pcall(L, 0, 0, 0);
  if (code != LUA_OK) return failed(code);
  initialized = 1;
  lua_settop(L, 0);
  return ok_result();
}

/* ---- calls ---- */

/* The conversion failure being reported; encoding runs no Lua code, so one is enough. */
static plg_conv conv;

/*
** encode_results encodes the n values a method returned, from base, into the result buffer: one result is the
** first value (nil when there is none), several are the elements of an array table.
*/
static int encode_results(lua_State *L, const plg_method *m, int base, int n) {
  out.len = 0;
  out.oom = 0;
  plg_buf_put(&out, &(uint8_t){PLG_OK}, 1);
  fail_status = PLG_CONVERT;
  if (m->nresults > 1 && (n == 0 || !lua_istable(L, base)))
    return luaL_error(L, "results: want an array of %d values, got %s", (int)m->nresults,
                      n == 0 ? "nothing" : luaL_typename(L, base));
  if (m->nresults == 1 && n == 0) lua_pushnil(L);
  for (uint32_t k = 0; k < m->nresults; k++) {
    int idx = base;
    if (m->nresults > 1) {
      lua_rawgeti(L, base, (lua_Integer)k + 1);
      idx = lua_gettop(L);
    }
    int err = plg_encode_value(L, &plg_the_schema, m->results[k], idx, &out, &conv, 0);
    if (m->nresults > 1) lua_pop(L, 1);
    if (out.oom) {
      fail_status = PLG_PANIC;
      return luaL_error(L, "not enough memory to encode the results");
    }
    if (err < 0) {
      char prefix[32];
      snprintf(prefix, sizeof prefix, "result %u", (unsigned)k);
      plg_conv_push(L, &conv, prefix);
      return lua_error(L);
    }
  }
  return 0;
}

/*
** call_finish ends a method call whose results start at index 2 (the instance is at 1): Lua's `nil, err`
** failure convention raises err, anything else is encoded. It is also the continuation of a call that yielded
** for an HTTP request (k is the method's index).
*/
static int call_finish(lua_State *L, int status, lua_KContext k) {
  (void)status;
  const plg_method *m = &plg_the_schema.methods[k];
  int n = lua_gettop(L) - 1;
  if (is_failure(L, 2, n)) {
    lua_pushvalue(L, 3);
    return lua_error(L);
  }
  return encode_results(L, m, 2, n);
}

/* call_protected calls inst:<method calling>(args...); in a multiplexed step it is the body of the call's coroutine. */
static int call_protected(lua_State *L) {
  const plg_method *m = &plg_the_schema.methods[calling];
  fail_status = PLG_ABI;
  luaL_checkstack(L, (int)m->nparams + 8, "too many arguments");
  lua_rawgeti(L, LUA_REGISTRYINDEX, instref); /* 1 */
  lua_getfield(L, 1, m->name);                 /* 2 */
  if (calling == iface_methods && !callable(L, 2)) { /* Validate, which the plugin may lack */
    fail_status = PLG_LOAD;
    return luaL_error(L, "the table Create returned has no method %s", m->name);
  }
  lua_pushvalue(L, 1);
  for (uint32_t k = 0; k < m->nparams; k++) {
    const char *err = plg_push_value(L, &plg_the_schema, m->params[k], &args, 0);
    if (err != NULL) return luaL_error(L, "parameter %d: %s", (int)k + 1, err);
  }
  if (args.p != args.end) return luaL_error(L, "trailing bytes after the arguments");
  fail_status = PLG_ERROR;
  lua_callk(L, (int)m->nparams + 1, LUA_MULTRET, (lua_KContext)calling, call_finish);
  return call_finish(L, LUA_OK, (lua_KContext)calling);
}

EXPORT("plg_call") uint64_t plg_call(uint32_t method, uint32_t len) {
  if (!initialized) return cmessage(PLG_ABI, "plg_call before a successful plg_init");
  if (method >= plg_host_base) return cmessage(PLG_ABI, "unknown method");
  if (len > incap) return cmessage(PLG_ABI, "arguments beyond the input buffer");
  args = (plg_reader){inbuf, inbuf + len};
  calling = method;
  lua_settop(L, 0);
  lua_pushcfunction(L, call_protected);
  int code = lua_pcall(L, 0, 0, 0);
  uint64_t res = code == LUA_OK ? finish() : failed(code);
  lua_settop(L, 0);
  collect_if_starved();
  return res;
}

/* ---- multiplexed steps: plg_start and plg_resume ---- */

/*
** In a multiplexed step every call runs in a coroutine of its own. http.request (capi.c) hands its request to
** the host with http_start and yields; plg_resume resumes the coroutine with the answer. A step runs one
** coroutine, so it finishes at most one call. Where the call's coroutine cannot yield (Create and Close run
** outside steps, a coroutine of the plugin's own, a C function such as table.sort's comparator in between),
** http.request blocks the instance as in plg_call.
*/
typedef struct waiting {
  lua_State *thread;
  int ref;          /* anchors thread in the registry */
  uint32_t call;    /* the host's number for the call */
  uint32_t request; /* the request the call waits for */
} waiting;

static waiting *waits; /* the calls in flight */
static size_t nwaits, capwaits;
static plg_buf steps;     /* the result buffer of steps */
static uint32_t finished; /* calls the step in progress finished */
static lua_State *new_thread;
static int new_ref;

lua_State *plg_call_thread;
uint32_t plg_call_id;
uint32_t plg_wait_request;
plg_reader plg_answer;
char plg_yield_key;

static void begin_step(void) {
  steps.len = 0;
  steps.oom = 0;
  plg_buf_put(&steps, &(uint8_t){PLG_OK}, 1);
  plg_buf_u32(&steps, 0);
  finished = 0;
}

static uint64_t end_step(void) {
  lua_settop(L, 0);
  collect_if_starved();
  if (steps.oom) return packed(oom_result, sizeof oom_result);
  plg_buf_patch_u32(&steps, 1, finished);
  return packed(steps.data, steps.len);
}

/* complete lists call as finished with the result buffer res (packed ptr<<32 | len). */
static void complete(uint32_t call, uint64_t res) {
  plg_buf_u32(&steps, call);
  plg_buf_u32(&steps, (uint32_t)res);
  plg_buf_put(&steps, (const uint8_t *)(uintptr_t)(res >> 32), (uint32_t)res);
  finished++;
}

/* new_call_protected creates a coroutine running call_protected and anchors it in the registry. */
static int new_call_protected(lua_State *L) {
  new_thread = lua_newthread(L);
  new_ref = luaL_ref(L, LUA_REGISTRYINDEX);
  lua_pushcfunction(new_thread, call_protected);
  return 0;
}

/* run resumes the coroutine of waits[i] until it waits for a request or ends; an ended call is listed. */
static void run(size_t i) {
  lua_State *thread = waits[i].thread;
  uint32_t call = waits[i].call;
  plg_call_thread = thread;
  plg_call_id = call;
  plg_wait_request = 0;
  fail_status = PLG_ERROR;
  int nres = 0;
  int code = lua_resume(thread, L, 0, &nres);
  plg_call_thread = NULL;
  if (code == LUA_YIELD && nres == 1 && lua_touserdata(thread, -1) == &plg_yield_key && plg_wait_request != 0) {
    lua_pop(thread, 1);
    waits[i].request = plg_wait_request;
    return;
  }
  lua_settop(L, 0);
  if (code == LUA_YIELD) {
    complete(call, cmessage(PLG_ERROR, "attempt to yield from outside a coroutine"));
  } else if (code == LUA_OK) {
    complete(call, finish());
  } else {
    lua_xmove(thread, L, 1);
    complete(call, failed(code));
  }
  luaL_unref(L, LUA_REGISTRYINDEX, waits[i].ref);
  waits[i] = waits[--nwaits];
}

/* plg_start starts call number call of the interface method with the given index (arguments as for plg_call). */
EXPORT("plg_start") uint64_t plg_start(uint32_t call, uint32_t method, uint32_t len) {
  if (!initialized) return cmessage(PLG_ABI, "plg_start before a successful plg_init");
  if (method >= iface_methods) return cmessage(PLG_ABI, "unknown method");
  if (len > incap) return cmessage(PLG_ABI, "arguments beyond the input buffer");
  for (size_t i = 0; i < nwaits; i++)
    if (waits[i].call == call) return cmessage(PLG_ABI, "call number already in flight");
  if (nwaits == capwaits) {
    size_t cap = capwaits ? 2 * capwaits : 16;
    waiting *grown = realloc(waits, cap * sizeof *grown);
    if (grown == NULL) return cmessage(PLG_PANIC, "not enough memory");
    waits = grown;
    capwaits = cap;
  }
  begin_step();
  args = (plg_reader){inbuf, inbuf + len};
  calling = method;
  lua_settop(L, 0);
  lua_pushcfunction(L, new_call_protected);
  int code = lua_pcall(L, 0, 0, 0);
  if (code != LUA_OK) {
    complete(call, failed(code));
    return end_step();
  }
  waits[nwaits++] = (waiting){.thread = new_thread, .ref = new_ref, .call = call};
  run(nwaits - 1);
  return end_step();
}

/* plg_resume delivers the answer to request number request (an http_roundtrip answer) to the call waiting for it. */
EXPORT("plg_resume") uint64_t plg_resume(uint32_t request, uint32_t len) {
  if (!initialized) return cmessage(PLG_ABI, "plg_resume before a successful plg_init");
  if (len > incap) return cmessage(PLG_ABI, "answer beyond the input buffer");
  size_t i = 0;
  while (i < nwaits && waits[i].request != request) i++;
  if (request == 0 || i == nwaits) return cmessage(PLG_ABI, "no call waits for this request");
  begin_step();
  plg_answer = (plg_reader){inbuf, inbuf + len};
  waits[i].request = 0;
  run(i);
  return end_step();
}

static int close_protected(lua_State *L) {
  fail_status = PLG_ERROR;
  lua_rawgeti(L, LUA_REGISTRYINDEX, instref); /* 1 */
  lua_getfield(L, 1, "Close");
  if (lua_isnil(L, -1)) return 0;
  lua_pushvalue(L, 1);
  lua_call(L, 1, LUA_MULTRET);
  int n = lua_gettop(L) - 1;
  if (is_failure(L, 2, n)) {
    lua_pushvalue(L, 3);
    return lua_error(L);
  }
  return 0;
}

/* plg_close calls the plugin's optional Close method: inst:Close() failing like any method fails the close. */
EXPORT("plg_close") uint64_t plg_close(void) {
  if (!initialized) return ok_result();
  initialized = 0;
  lua_settop(L, 0);
  lua_pushcfunction(L, close_protected);
  int code = lua_pcall(L, 0, 0, 0);
  if (code != LUA_OK) return failed(code);
  lua_settop(L, 0);
  return ok_result();
}
