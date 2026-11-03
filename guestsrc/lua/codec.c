/*
** The wire codec of the Lua guest: the schema the host sends at init, and the conversion of wire values to Lua
** values (arguments, Host results) and back (results, Host arguments), following internal/wire.
**
** Mapping: bool = boolean; integers = Lua integers (|n| <= 2^53 and within the Go kind's range on the way back;
** integral floats are accepted); floats = numbers; string and []byte = strings ([]byte nil = nil); time.Time =
** {seconds = <unix seconds>, nanos = <0..999999999>}; slices = sequences (nil = nil); string-keyed maps =
** tables (nil = nil); pointers = the value or nil; structs = tables keyed by the field's json name (else its Go
** name). On the way back a missing struct field is its Go zero value, and json.null counts as nil.
*/
#include <math.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "lauxlib.h"
#include "plg.h"

/* time.Time{} as Unix seconds: January 1, year 1, UTC. */
#define ZERO_TIME_SECONDS (-62135596800LL)
#define MAX_NANOS 999999999LL

/* ---- buffers and readers ---- */

int plg_buf_reserve(plg_buf *b, size_t more) {
  if (b->oom) return 0;
  if (more <= b->cap - b->len) return 1;
  if (more > SIZE_MAX / 2 - b->len) {
    b->oom = 1;
    return 0;
  }
  size_t cap = b->cap ? b->cap : 256;
  while (cap - b->len < more) cap *= 2;
  uint8_t *grown = realloc(b->data, cap);
  if (grown == NULL) {
    b->oom = 1;
    return 0;
  }
  b->data = grown;
  b->cap = cap;
  return 1;
}

void plg_buf_put(plg_buf *b, const void *src, size_t n) {
  if (n == 0 || !plg_buf_reserve(b, n)) return;
  memcpy(b->data + b->len, src, n);
  b->len += n;
}

void plg_buf_u32(plg_buf *b, uint32_t v) {
  uint8_t le[4] = {(uint8_t)v, (uint8_t)(v >> 8), (uint8_t)(v >> 16), (uint8_t)(v >> 24)};
  plg_buf_put(b, le, sizeof le);
}

void plg_buf_u64(plg_buf *b, uint64_t v) {
  uint8_t le[8];
  for (int i = 0; i < 8; i++) le[i] = (uint8_t)(v >> (8 * i));
  plg_buf_put(b, le, sizeof le);
}

void plg_buf_str(plg_buf *b, const char *s, size_t n) {
  plg_buf_u32(b, (uint32_t)n);
  plg_buf_put(b, s, n);
}

void plg_buf_patch_u32(plg_buf *b, size_t at, uint32_t v) {
  if (b->oom) return;
  for (int i = 0; i < 4; i++) b->data[at + i] = (uint8_t)(v >> (8 * i));
}

int plg_read_bytes(plg_reader *r, size_t n, const uint8_t **p) {
  if ((size_t)(r->end - r->p) < n) return 0;
  *p = r->p;
  r->p += n;
  return 1;
}

int plg_read_u8(plg_reader *r, uint8_t *v) {
  const uint8_t *p;
  if (!plg_read_bytes(r, 1, &p)) return 0;
  *v = p[0];
  return 1;
}

int plg_read_u32(plg_reader *r, uint32_t *v) {
  const uint8_t *p;
  if (!plg_read_bytes(r, 4, &p)) return 0;
  *v = (uint32_t)p[0] | (uint32_t)p[1] << 8 | (uint32_t)p[2] << 16 | (uint32_t)p[3] << 24;
  return 1;
}

int plg_read_u64(plg_reader *r, uint64_t *v) {
  const uint8_t *p;
  if (!plg_read_bytes(r, 8, &p)) return 0;
  uint64_t x = 0;
  for (int i = 7; i >= 0; i--) x = x << 8 | p[i];
  *v = x;
  return 1;
}

int plg_read_str(plg_reader *r, const char **s, size_t *n) {
  uint32_t len;
  const uint8_t *p;
  if (!plg_read_u32(r, &len) || !plg_read_bytes(r, len, &p)) return 0;
  *s = (const char *)p;
  *n = len;
  return 1;
}

/* ---- schema ---- */

static const char *errTruncatedSchema = "malformed schema: truncated";
static const char *errSchemaMemory = "out of memory reading the schema";

/* dupname copies a name, NUL-terminated. */
static char *dupname(const char *s, size_t n) {
  char *copy = malloc(n + 1);
  if (copy == NULL) return NULL;
  memcpy(copy, s, n);
  copy[n] = 0;
  return copy;
}

/* readcount reads a count and checks that the input can hold count items of at least min bytes each. */
static int readcount(plg_reader *r, uint32_t *n, size_t min) {
  return plg_read_u32(r, n) && (size_t)*n <= (size_t)(r->end - r->p) / min;
}

static const char *parse_type(plg_reader *r, plg_type *ty) {
  if (!plg_read_u8(r, &ty->kind)) return errTruncatedSchema;
  switch (ty->kind) {
    case KIND_INT:
    case KIND_UINT:
    case KIND_FLOAT:
      if (!plg_read_u8(r, &ty->bits)) return errTruncatedSchema;
      if (ty->bits != 8 && ty->bits != 16 && ty->bits != 32 && ty->bits != 64) return "malformed schema: size";
      if (ty->kind == KIND_FLOAT && ty->bits < 32) return "malformed schema: float size";
      return NULL;
    case KIND_SLICE:
    case KIND_MAP:
    case KIND_POINTER:
      return plg_read_u32(r, &ty->elem) ? NULL : errTruncatedSchema;
    case KIND_STRUCT:
      if (!readcount(r, &ty->nfields, 8)) return errTruncatedSchema;
      if (ty->nfields == 0) return NULL;
      ty->fields = calloc(ty->nfields, sizeof *ty->fields);
      if (ty->fields == NULL) return errSchemaMemory;
      for (uint32_t k = 0; k < ty->nfields; k++) {
        const char *name;
        size_t n;
        if (!plg_read_str(r, &name, &n) || !plg_read_u32(r, &ty->fields[k].type)) return errTruncatedSchema;
        if (memchr(name, 0, n) != NULL) return "malformed schema: field name";
        ty->fields[k].name = dupname(name, n);
        ty->fields[k].name_len = n;
        if (ty->fields[k].name == NULL) return errSchemaMemory;
      }
      return NULL;
    case KIND_BOOL:
    case KIND_STRING:
    case KIND_BYTES:
    case KIND_TIME:
    case KIND_ERROR:
      return NULL;
    default:
      return "malformed schema: kind";
  }
}

static const char *parse_indexes(plg_reader *r, uint32_t ntypes, uint32_t *n, uint32_t **out) {
  if (!readcount(r, n, 4)) return errTruncatedSchema;
  if (*n == 0) return NULL;
  *out = calloc(*n, sizeof **out);
  if (*out == NULL) return errSchemaMemory;
  for (uint32_t k = 0; k < *n; k++) {
    if (!plg_read_u32(r, &(*out)[k])) return errTruncatedSchema;
    if ((*out)[k] >= ntypes) return "malformed schema: type index";
  }
  return NULL;
}

/* check_refs checks that every type a type refers to exists. */
static const char *check_refs(const plg_schema *s) {
  for (uint32_t i = 0; i < s->ntypes; i++) {
    const plg_type *ty = &s->types[i];
    if ((ty->kind == KIND_SLICE || ty->kind == KIND_MAP || ty->kind == KIND_POINTER) && ty->elem >= s->ntypes)
      return "malformed schema: type index";
    for (uint32_t k = 0; k < ty->nfields; k++)
      if (ty->fields[k].type >= s->ntypes) return "malformed schema: type index";
  }
  return NULL;
}

const char *plg_schema_parse(plg_reader *r, plg_schema *s) {
  if (!readcount(r, &s->ntypes, 1)) return errTruncatedSchema;
  s->types = calloc(s->ntypes ? s->ntypes : 1, sizeof *s->types);
  if (s->types == NULL) return errSchemaMemory;
  for (uint32_t i = 0; i < s->ntypes; i++) {
    const char *err = parse_type(r, &s->types[i]);
    if (err != NULL) return err;
  }
  const char *err = check_refs(s);
  if (err != NULL) return err;
  if (!readcount(r, &s->nmethods, 12)) return errTruncatedSchema;
  s->methods = calloc(s->nmethods ? s->nmethods : 1, sizeof *s->methods);
  if (s->methods == NULL) return errSchemaMemory;
  for (uint32_t i = 0; i < s->nmethods; i++) {
    plg_method *m = &s->methods[i];
    const char *name;
    size_t n;
    if (!plg_read_str(r, &name, &n)) return errTruncatedSchema;
    if (n == 0 || memchr(name, 0, n) != NULL) return "malformed schema: method name";
    m->name = dupname(name, n);
    if (m->name == NULL) return errSchemaMemory;
    err = parse_indexes(r, s->ntypes, &m->nparams, &m->params);
    if (err == NULL) err = parse_indexes(r, s->ntypes, &m->nresults, &m->results);
    if (err != NULL) return err;
  }
  return r->p == r->end ? NULL : "malformed schema: trailing bytes";
}

/* ---- wire to Lua ---- */

static const char *errTruncated = "truncated value";
static const char *errInvalid = "invalid encoding";

static void push_time(lua_State *L, int64_t seconds, int64_t nanos) {
  lua_createtable(L, 0, 2);
  lua_pushinteger(L, seconds);
  lua_setfield(L, -2, "seconds");
  lua_pushinteger(L, nanos);
  lua_setfield(L, -2, "nanos");
}

static int safe(int64_t v) {
  return v <= PLG_MAX_SAFE_INT && v >= -PLG_MAX_SAFE_INT;
}

static const char *push_scalar(lua_State *L, const plg_type *ty, plg_reader *r) {
  uint64_t word, nanos;
  uint8_t flag;
  const char *str;
  size_t n;
  switch (ty->kind) {
    case KIND_BOOL:
      if (!plg_read_u8(r, &flag)) return errTruncated;
      if (flag > 1) return errInvalid;
      lua_pushboolean(L, flag);
      return NULL;
    case KIND_INT:
    case KIND_UINT:
      if (!plg_read_u64(r, &word)) return errTruncated;
      if (!safe((int64_t)word) || (ty->kind == KIND_UINT && (int64_t)word < 0)) return "integer beyond ±2^53";
      lua_pushinteger(L, (lua_Integer)(int64_t)word);
      return NULL;
    case KIND_FLOAT: {
      if (!plg_read_u64(r, &word)) return errTruncated;
      double d;
      memcpy(&d, &word, sizeof d);
      lua_pushnumber(L, d);
      return NULL;
    }
    case KIND_STRING:
      if (!plg_read_str(r, &str, &n)) return errTruncated;
      lua_pushlstring(L, str, n);
      return NULL;
    case KIND_TIME:
      if (!plg_read_u64(r, &word) || !plg_read_u64(r, &nanos)) return errTruncated;
      if (!safe((int64_t)word) || nanos > (uint64_t)MAX_NANOS) return errInvalid;
      push_time(L, (int64_t)word, (int64_t)nanos);
      return NULL;
    case KIND_ERROR:
      if (!plg_read_u8(r, &flag)) return errTruncated;
      if (flag > 1) return errInvalid;
      if (flag == 0) {
        lua_pushnil(L);
        return NULL;
      }
      if (!plg_read_str(r, &str, &n)) return errTruncated;
      lua_pushlstring(L, str, n);
      return NULL;
    default:
      return errInvalid;
  }
}

/*
** push_element pushes an element of a slice or a map: a nil one as json.null, since a nil would leave a hole in
** a sequence and no entry in a table.
*/
static const char *push_element(lua_State *L, const plg_schema *s, uint32_t t, plg_reader *r, int depth) {
  const char *err = plg_push_value(L, s, t, r, depth + 1);
  if (err == NULL && lua_isnil(L, -1)) {
    lua_pop(L, 1);
    lua_pushlightuserdata(L, NULL);
  }
  return err;
}

static const char *push_slice(lua_State *L, const plg_schema *s, const plg_type *ty, plg_reader *r, int depth) {
  uint32_t n;
  if (!plg_read_u32(r, &n)) return errTruncated;
  if (n == PLG_NIL_LENGTH) {
    lua_pushnil(L);
    return NULL;
  }
  if (ty->kind == KIND_BYTES) {
    const uint8_t *p;
    if (!plg_read_bytes(r, n, &p)) return errTruncated;
    lua_pushlstring(L, (const char *)p, n);
    return NULL;
  }
  /* Every element type encodes as one byte at least: the planner rejects the others. */
  if (n > (size_t)(r->end - r->p)) return errTruncated;
  lua_createtable(L, (int)(n < (uint32_t)INT32_MAX ? n : 0), 0);
  for (uint32_t i = 0; i < n; i++) {
    const char *err = push_element(L, s, ty->elem, r, depth);
    if (err != NULL) return err;
    lua_rawseti(L, -2, (lua_Integer)i + 1);
  }
  return NULL;
}

static const char *push_map(lua_State *L, const plg_schema *s, const plg_type *ty, plg_reader *r, int depth) {
  uint32_t n;
  if (!plg_read_u32(r, &n)) return errTruncated;
  if (n == PLG_NIL_LENGTH) {
    lua_pushnil(L);
    return NULL;
  }
  if (n > (size_t)(r->end - r->p) / 5) return errTruncated;
  lua_createtable(L, 0, (int)n);
  for (uint32_t i = 0; i < n; i++) {
    const char *key;
    size_t len;
    if (!plg_read_str(r, &key, &len)) return errTruncated;
    lua_pushlstring(L, key, len);
    lua_pushvalue(L, -1);
    if (lua_rawget(L, -3) != LUA_TNIL) return "duplicate map key";
    lua_pop(L, 1);
    const char *err = push_element(L, s, ty->elem, r, depth);
    if (err != NULL) return err;
    lua_rawset(L, -3);
  }
  return NULL;
}

const char *plg_push_value(lua_State *L, const plg_schema *s, uint32_t t, plg_reader *r, int depth) {
  if (depth > PLG_MAX_DEPTH) return "value nested too deeply";
  if (!lua_checkstack(L, 4)) return "value nested too deeply";
  const plg_type *ty = &s->types[t];
  switch (ty->kind) {
    case KIND_SLICE:
    case KIND_BYTES:
      return push_slice(L, s, ty, r, depth);
    case KIND_MAP:
      return push_map(L, s, ty, r, depth);
    case KIND_POINTER: {
      uint8_t present;
      if (!plg_read_u8(r, &present)) return errTruncated;
      if (present > 1) return errInvalid;
      if (!present) {
        lua_pushnil(L);
        return NULL;
      }
      return plg_push_value(L, s, ty->elem, r, depth + 1);
    }
    case KIND_STRUCT:
      lua_createtable(L, 0, (int)ty->nfields);
      for (uint32_t k = 0; k < ty->nfields; k++) {
        const char *err = plg_push_value(L, s, ty->fields[k].type, r, depth + 1);
        if (err != NULL) return err;
        lua_setfield(L, -2, ty->fields[k].name);
      }
      return NULL;
    default:
      return push_scalar(L, ty, r);
  }
}

/* ---- Lua to wire ---- */

void plg_conv_fail(plg_conv *cv, const char *fmt, ...) {
  va_list ap;
  va_start(ap, fmt);
  vsnprintf(cv->msg, sizeof cv->msg, fmt, ap);
  va_end(ap);
}

/* The longest path a conversion error names: deeper values show only its innermost part, after "...". */
#define MAX_PATH 1024

/* prepend puts a path segment in front of the path built so far. */
static void prepend(plg_conv *cv, const char *seg, size_t n) {
  plg_buf *p = &cv->path;
  if (cv->cut) return;
  if (p->len + n > MAX_PATH) {
    cv->cut = 1;
    seg = "...";
    n = 3;
  }
  if (!plg_buf_reserve(p, n)) return;
  memmove(p->data + n, p->data, p->len);
  memcpy(p->data, seg, n);
  p->len += n;
}

static void prepend_index(plg_conv *cv, lua_Integer i) {
  char seg[32];
  int n = snprintf(seg, sizeof seg, "[%lld]", (long long)i);
  prepend(cv, seg, (size_t)n);
}

static void prepend_key(plg_conv *cv, const char *key, size_t len) {
  /* ["key"], cut to keep messages bounded */
  char seg[80];
  size_t shown = len < 64 ? len : 64;
  int n = snprintf(seg, sizeof seg, "[\"%.*s%s\"]", (int)shown, key, len > shown ? "..." : "");
  prepend(cv, seg, (size_t)n);
}

static void prepend_field(plg_conv *cv, const plg_field *f) {
  prepend(cv, f->name, f->name_len);
  prepend(cv, ".", 1);
}

void plg_conv_push(lua_State *L, plg_conv *cv, const char *prefix) {
  const char *path = (const char *)cv->path.data;
  size_t n = cv->path.oom ? 0 : cv->path.len;
  if (n > 0 && path[0] == '.') {
    path++;
    n--;
  }
  if (n > 0)
    lua_pushfstring(L, "%s: %s: %s", prefix, lua_pushlstring(L, path, n), cv->msg);
  else
    lua_pushfstring(L, "%s: %s", prefix, cv->msg);
  if (n > 0) lua_remove(L, -2);
  cv->path.len = 0;
  cv->path.oom = 0;
  cv->cut = 0;
  cv->msg[0] = 0;
}

/* isnil reports whether the value at idx is nil or json.null. */
static int isnil(lua_State *L, int idx) {
  return lua_isnil(L, idx) || (lua_islightuserdata(L, idx) && lua_touserdata(L, idx) == NULL);
}

static int want(lua_State *L, plg_conv *cv, const char *what, int idx) {
  plg_conv_fail(cv, "want %s, got %s", what, luaL_typename(L, idx));
  return -1;
}

/* tointeger reads an integer (or an integral float) within ±2^53. */
static int tointeger(lua_State *L, int idx, plg_conv *cv, int64_t *out) {
  if (lua_type(L, idx) != LUA_TNUMBER) return want(L, cv, "an integer", idx);
  if (lua_isinteger(L, idx)) {
    *out = (int64_t)lua_tointeger(L, idx);
  } else {
    double d = lua_tonumber(L, idx);
    if (d != floor(d) || isinf(d)) {
      plg_conv_fail(cv, "want an integer, got %.17g", d);
      return -1;
    }
    if (fabs(d) > (double)PLG_MAX_SAFE_INT) {
      plg_conv_fail(cv, "wire: integer beyond ±2^53: %.17g", d);
      return -1;
    }
    *out = (int64_t)d;
  }
  if (!safe(*out)) {
    plg_conv_fail(cv, "wire: integer beyond ±2^53: %lld", (long long)*out);
    return -1;
  }
  return 0;
}

static int encode_int(lua_State *L, const plg_type *ty, int idx, plg_buf *b, plg_conv *cv) {
  int64_t v;
  if (tointeger(L, idx, cv, &v) < 0) return -1;
  if (ty->kind == KIND_UINT) {
    if (v < 0 || (ty->bits < 64 && (uint64_t)v > (UINT64_C(1) << ty->bits) - 1)) {
      plg_conv_fail(cv, "wire: value out of range: %lld does not fit uint%d", (long long)v, ty->bits);
      return -1;
    }
  } else if (ty->bits < 64) {
    int64_t limit = INT64_C(1) << (ty->bits - 1);
    if (v < -limit || v >= limit) {
      plg_conv_fail(cv, "wire: value out of range: %lld does not fit int%d", (long long)v, ty->bits);
      return -1;
    }
  }
  plg_buf_u64(b, (uint64_t)v);
  return 0;
}

/* encode_float encodes a number; a float32 is rounded as Go's float32(x) rounds (beyond its range to ±Inf). */
static int encode_float(lua_State *L, const plg_type *ty, int idx, plg_buf *b, plg_conv *cv) {
  if (lua_type(L, idx) != LUA_TNUMBER) return want(L, cv, "a number", idx);
  double d = (double)lua_tonumber(L, idx);
  if (ty->bits == 32) d = (double)(float)d; /* wasm's f32.demote_f64: round to nearest, overflow to ±Inf */
  uint64_t word;
  memcpy(&word, &d, sizeof word);
  plg_buf_u64(b, word);
  return 0;
}

/* time_field reads t.<name>, an integer (0 when absent), and pops it. */
static int time_field(lua_State *L, int idx, const char *name, plg_conv *cv, int64_t *v) {
  *v = 0;
  lua_pushstring(L, name);
  lua_rawget(L, idx);
  int err = lua_isnil(L, -1) ? 0 : tointeger(L, lua_gettop(L), cv, v);
  lua_pop(L, 1);
  if (err < 0) {
    prepend(cv, name, strlen(name));
    prepend(cv, ".", 1);
  }
  return err;
}

static int encode_time(lua_State *L, int idx, plg_buf *b, plg_conv *cv) {
  if (!lua_istable(L, idx)) return want(L, cv, "a time table {seconds, nanos}", idx);
  int64_t seconds, nanos;
  if (time_field(L, idx, "seconds", cv, &seconds) < 0 || time_field(L, idx, "nanos", cv, &nanos) < 0) return -1;
  if (nanos < 0 || nanos > MAX_NANOS) {
    plg_conv_fail(cv, "nanos %lld out of range 0 to 999999999", (long long)nanos);
    return -1;
  }
  plg_buf_u64(b, (uint64_t)seconds);
  plg_buf_u64(b, (uint64_t)nanos);
  return 0;
}

static int encode_string(lua_State *L, const plg_type *ty, int idx, plg_buf *b, plg_conv *cv) {
  if (ty->kind != KIND_STRING && isnil(L, idx)) {
    if (ty->kind == KIND_ERROR)
      plg_buf_put(b, "", 1);
    else
      plg_buf_u32(b, PLG_NIL_LENGTH);
    return 0;
  }
  if (lua_type(L, idx) != LUA_TSTRING) return want(L, cv, "a string", idx);
  size_t n;
  const char *s = lua_tolstring(L, idx, &n);
  if (n >= PLG_NIL_LENGTH) {
    plg_conv_fail(cv, "string too long");
    return -1;
  }
  if (ty->kind == KIND_ERROR) plg_buf_put(b, "\1", 1);
  plg_buf_str(b, s, n);
  return 0;
}

static int encode_slice(lua_State *L, const plg_schema *s, const plg_type *ty, int idx, plg_buf *b, plg_conv *cv,
                        int depth) {
  if (isnil(L, idx)) {
    plg_buf_u32(b, PLG_NIL_LENGTH);
    return 0;
  }
  if (!lua_istable(L, idx)) return want(L, cv, "a sequence table", idx);
  lua_Unsigned n = lua_rawlen(L, idx);
  if (n >= PLG_NIL_LENGTH) {
    plg_conv_fail(cv, "sequence too long");
    return -1;
  }
  plg_buf_u32(b, (uint32_t)n);
  for (lua_Unsigned i = 1; i <= n; i++) {
    lua_rawgeti(L, idx, (lua_Integer)i);
    int err = plg_encode_value(L, s, ty->elem, lua_gettop(L), b, cv, depth + 1);
    lua_pop(L, 1);
    if (err < 0) {
      prepend_index(cv, (lua_Integer)i - 1); /* Go's index */
      return -1;
    }
  }
  return 0;
}

static int encode_map(lua_State *L, const plg_schema *s, const plg_type *ty, int idx, plg_buf *b, plg_conv *cv,
                      int depth) {
  if (isnil(L, idx)) {
    plg_buf_u32(b, PLG_NIL_LENGTH);
    return 0;
  }
  if (!lua_istable(L, idx)) return want(L, cv, "a table", idx);
  size_t at = b->len;
  uint32_t n = 0;
  plg_buf_u32(b, 0);
  lua_pushnil(L);
  while (lua_next(L, idx) != 0) {
    if (lua_type(L, -2) != LUA_TSTRING) {
      plg_conv_fail(cv, "map key must be a string, got %s", luaL_typename(L, -2));
      lua_pop(L, 2);
      return -1;
    }
    size_t len;
    const char *key = lua_tolstring(L, -2, &len);
    plg_buf_str(b, key, len);
    int err = plg_encode_value(L, s, ty->elem, lua_gettop(L), b, cv, depth + 1);
    if (err < 0) {
      prepend_key(cv, key, len);
      lua_pop(L, 2);
      return -1;
    }
    lua_pop(L, 1);
    n++;
  }
  plg_buf_patch_u32(b, at, n);
  return 0;
}

/* encode_zero encodes the Go zero value of type t: what a missing struct field holds. */
static void encode_zero(const plg_schema *s, uint32_t t, plg_buf *b) {
  const plg_type *ty = &s->types[t];
  switch (ty->kind) {
    case KIND_BOOL:
    case KIND_ERROR:
    case KIND_POINTER:
      plg_buf_put(b, "", 1);
      return;
    case KIND_STRING:
      plg_buf_u32(b, 0);
      return;
    case KIND_BYTES:
    case KIND_SLICE:
    case KIND_MAP:
      plg_buf_u32(b, PLG_NIL_LENGTH);
      return;
    case KIND_TIME:
      plg_buf_u64(b, (uint64_t)ZERO_TIME_SECONDS);
      plg_buf_u64(b, 0);
      return;
    case KIND_STRUCT:
      for (uint32_t k = 0; k < ty->nfields; k++) encode_zero(s, ty->fields[k].type, b);
      return;
    default: /* numbers: zero is all zero bits */
      plg_buf_u64(b, 0);
  }
}

static int encode_struct(lua_State *L, const plg_schema *s, const plg_type *ty, int idx, plg_buf *b, plg_conv *cv,
                         int depth) {
  if (!lua_istable(L, idx)) return want(L, cv, "a table", idx);
  for (uint32_t k = 0; k < ty->nfields; k++) {
    const plg_field *f = &ty->fields[k];
    lua_pushlstring(L, f->name, f->name_len);
    lua_rawget(L, idx);
    int err = 0;
    if (isnil(L, -1))
      encode_zero(s, f->type, b);
    else
      err = plg_encode_value(L, s, f->type, lua_gettop(L), b, cv, depth + 1);
    lua_pop(L, 1);
    if (err < 0) {
      prepend_field(cv, f);
      return -1;
    }
  }
  return 0;
}

int plg_encode_value(lua_State *L, const plg_schema *s, uint32_t t, int idx, plg_buf *b, plg_conv *cv, int depth) {
  if (depth > PLG_MAX_DEPTH || !lua_checkstack(L, 4)) {
    plg_conv_fail(cv, "value nested too deeply (a cycle?)");
    return -1;
  }
  const plg_type *ty = &s->types[t];
  int err;
  switch (ty->kind) {
    case KIND_BOOL:
      if (!lua_isboolean(L, idx)) return want(L, cv, "a boolean", idx);
      plg_buf_put(b, lua_toboolean(L, idx) ? "\1" : "", 1);
      err = 0;
      break;
    case KIND_INT:
    case KIND_UINT:
      err = encode_int(L, ty, idx, b, cv);
      break;
    case KIND_FLOAT:
      err = encode_float(L, ty, idx, b, cv);
      break;
    case KIND_STRING:
    case KIND_BYTES:
    case KIND_ERROR:
      err = encode_string(L, ty, idx, b, cv);
      break;
    case KIND_TIME:
      err = encode_time(L, idx, b, cv);
      break;
    case KIND_SLICE:
      err = encode_slice(L, s, ty, idx, b, cv, depth);
      break;
    case KIND_MAP:
      err = encode_map(L, s, ty, idx, b, cv, depth);
      break;
    case KIND_POINTER:
      if (isnil(L, idx)) {
        plg_buf_put(b, "", 1);
        err = 0;
        break;
      }
      plg_buf_put(b, "\1", 1);
      err = plg_encode_value(L, s, ty->elem, idx, b, cv, depth + 1);
      break;
    default:
      err = encode_struct(L, s, ty, idx, b, cv, depth);
  }
  if (err == 0 && b->oom) {
    plg_conv_fail(cv, "out of memory");
    return -1;
  }
  return err;
}
