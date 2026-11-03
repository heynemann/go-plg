/*
** The json module bundled into the Lua guest (global `json`, also `require("json")`):
**
**   json.decode(text)     parses JSON (RFC 8259) into Lua values: objects and arrays become tables marked with
**                         the json.object / json.array metatables (so empty ones round-trip), null becomes
**                         json.null, integers that fit 64 bits become Lua integers and other numbers floats.
**                         \u escapes become UTF-8 (a lone surrogate becomes U+FFFD). Malformed JSON, invalid
**                         UTF-8 and nesting deeper than 1000 levels raise a Lua error naming the byte offset.
**   json.encode(value)    returns the JSON text of value: nil and json.null are null; booleans, numbers (finite
**                         only; integral floats print as integers) and strings (valid UTF-8 only) map directly;
**                         a table marked json.array, or whose keys are exactly 1..n (n >= 1), is an array;
**                         any other table is an object, whose keys must be strings, written in sorted order.
**                         Tables are read raw (metatables are not consulted). Cycles and nesting deeper than 1000
**                         levels, functions, userdata and threads raise a Lua error.
**   json.array(t), json.object(t)  mark t (default: a new table) as an array or an object and return it.
**   json.null             the value standing for JSON null.
*/
#include <math.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "lauxlib.h"
#include "plg.h"

#define MAX_JSON_DEPTH 1000
#define ARRAY_MT "plg.json.array"
#define OBJECT_MT "plg.json.object"

/* ---- decoding ---- */

typedef struct decoder {
  lua_State *L;
  const char *start, *p, *end;
} decoder;

static int fail(decoder *d, const char *what) {
  return luaL_error(d->L, "json.decode: %s at byte %d", what, (int)(d->p - d->start) + 1);
}

static void skip_space(decoder *d) {
  while (d->p < d->end && (*d->p == ' ' || *d->p == '\t' || *d->p == '\n' || *d->p == '\r')) d->p++;
}

/* utf8_len returns the length of the valid UTF-8 sequence at p (at most n bytes), or 0. */
static size_t utf8_len(const unsigned char *p, size_t n) {
  unsigned char c = p[0];
  size_t len;
  unsigned int cp, min;
  if (c < 0x80) return 1;
  if (c >= 0xC2 && c <= 0xDF) {
    len = 2;
    cp = c & 0x1F;
    min = 0x80;
  } else if (c >= 0xE0 && c <= 0xEF) {
    len = 3;
    cp = c & 0x0F;
    min = 0x800;
  } else if (c >= 0xF0 && c <= 0xF4) {
    len = 4;
    cp = c & 0x07;
    min = 0x10000;
  } else {
    return 0;
  }
  if (n < len) return 0;
  for (size_t i = 1; i < len; i++) {
    if ((p[i] & 0xC0) != 0x80) return 0;
    cp = cp << 6 | (p[i] & 0x3F);
  }
  if (cp < min || cp > 0x10FFFF || (cp >= 0xD800 && cp <= 0xDFFF)) return 0;
  return len;
}

static int hex4(decoder *d, unsigned int *out) {
  if (d->end - d->p < 4) return 0;
  unsigned int v = 0;
  for (int i = 0; i < 4; i++) {
    char c = d->p[i];
    v <<= 4;
    if (c >= '0' && c <= '9')
      v |= (unsigned int)(c - '0');
    else if (c >= 'a' && c <= 'f')
      v |= (unsigned int)(c - 'a' + 10);
    else if (c >= 'A' && c <= 'F')
      v |= (unsigned int)(c - 'A' + 10);
    else
      return 0;
  }
  d->p += 4;
  *out = v;
  return 1;
}

static void add_utf8(luaL_Buffer *b, unsigned int cp) {
  char s[4];
  size_t n;
  if (cp < 0x80) {
    s[0] = (char)cp;
    n = 1;
  } else if (cp < 0x800) {
    s[0] = (char)(0xC0 | cp >> 6);
    s[1] = (char)(0x80 | (cp & 0x3F));
    n = 2;
  } else if (cp < 0x10000) {
    s[0] = (char)(0xE0 | cp >> 12);
    s[1] = (char)(0x80 | (cp >> 6 & 0x3F));
    s[2] = (char)(0x80 | (cp & 0x3F));
    n = 3;
  } else {
    s[0] = (char)(0xF0 | cp >> 18);
    s[1] = (char)(0x80 | (cp >> 12 & 0x3F));
    s[2] = (char)(0x80 | (cp >> 6 & 0x3F));
    s[3] = (char)(0x80 | (cp & 0x3F));
    n = 4;
  }
  luaL_addlstring(b, s, n);
}

static void unicode_escape(decoder *d, luaL_Buffer *b) {
  unsigned int cp, low;
  if (!hex4(d, &cp)) fail(d, "invalid \\u escape");
  if (cp >= 0xD800 && cp <= 0xDBFF) {
    if (d->end - d->p >= 6 && d->p[0] == '\\' && d->p[1] == 'u') {
      const char *save = d->p;
      d->p += 2;
      if (hex4(d, &low) && low >= 0xDC00 && low <= 0xDFFF) {
        add_utf8(b, 0x10000 + ((cp - 0xD800) << 10) + (low - 0xDC00));
        return;
      }
      d->p = save;
    }
    cp = 0xFFFD;
  } else if (cp >= 0xDC00 && cp <= 0xDFFF) {
    cp = 0xFFFD;
  }
  add_utf8(b, cp);
}

static void decode_string(decoder *d) {
  luaL_Buffer b;
  luaL_buffinit(d->L, &b);
  d->p++; /* the opening quote */
  for (;;) {
    const char *run = d->p;
    while (d->p < d->end && *d->p != '"' && *d->p != '\\') {
      unsigned char c = (unsigned char)*d->p;
      if (c < 0x20) fail(d, "control character in string");
      size_t n = utf8_len((const unsigned char *)d->p, (size_t)(d->end - d->p));
      if (n == 0) fail(d, "invalid UTF-8");
      d->p += n;
    }
    luaL_addlstring(&b, run, (size_t)(d->p - run));
    if (d->p >= d->end) fail(d, "unterminated string");
    if (*d->p == '"') {
      d->p++;
      luaL_pushresult(&b);
      return;
    }
    d->p++; /* backslash */
    if (d->p >= d->end) fail(d, "unterminated string");
    char c = *d->p++;
    switch (c) {
      case '"':
      case '\\':
      case '/':
        luaL_addchar(&b, c);
        break;
      case 'b':
        luaL_addchar(&b, '\b');
        break;
      case 'f':
        luaL_addchar(&b, '\f');
        break;
      case 'n':
        luaL_addchar(&b, '\n');
        break;
      case 'r':
        luaL_addchar(&b, '\r');
        break;
      case 't':
        luaL_addchar(&b, '\t');
        break;
      case 'u':
        unicode_escape(d, &b);
        break;
      default:
        d->p--;
        fail(d, "invalid escape");
    }
  }
}

static int digits(decoder *d) {
  const char *from = d->p;
  while (d->p < d->end && *d->p >= '0' && *d->p <= '9') d->p++;
  return d->p > from;
}

static void decode_number(decoder *d) {
  const char *from = d->p;
  if (*d->p == '-') d->p++;
  if (d->p < d->end && *d->p == '0') {
    d->p++;
  } else if (!digits(d)) {
    fail(d, "invalid number");
  }
  if (d->p < d->end && *d->p == '.') {
    d->p++;
    if (!digits(d)) fail(d, "invalid number");
  }
  if (d->p < d->end && (*d->p == 'e' || *d->p == 'E')) {
    d->p++;
    if (d->p < d->end && (*d->p == '+' || *d->p == '-')) d->p++;
    if (!digits(d)) fail(d, "invalid number");
  }
  /* The grammar above is a subset of Lua's numerals: integers that fit become integers, others floats. */
  lua_pushlstring(d->L, from, (size_t)(d->p - from));
  if (lua_stringtonumber(d->L, lua_tostring(d->L, -1)) == 0) fail(d, "invalid number");
  lua_remove(d->L, -2);
}

static void literal(decoder *d, const char *word, size_t n) {
  if ((size_t)(d->end - d->p) < n || memcmp(d->p, word, n) != 0) fail(d, "invalid literal");
  d->p += n;
}

static void decode_value(decoder *d, int depth);

static void decode_array(decoder *d, int depth) {
  lua_State *L = d->L;
  d->p++;
  lua_newtable(L);
  luaL_setmetatable(L, ARRAY_MT);
  skip_space(d);
  if (d->p < d->end && *d->p == ']') {
    d->p++;
    return;
  }
  for (lua_Integer i = 1;; i++) {
    decode_value(d, depth + 1);
    lua_rawseti(L, -2, i);
    skip_space(d);
    if (d->p >= d->end) fail(d, "unterminated array");
    if (*d->p == ']') {
      d->p++;
      return;
    }
    if (*d->p != ',') fail(d, "expected ',' or ']'");
    d->p++;
  }
}

static void decode_object(decoder *d, int depth) {
  lua_State *L = d->L;
  d->p++;
  lua_newtable(L);
  luaL_setmetatable(L, OBJECT_MT);
  skip_space(d);
  if (d->p < d->end && *d->p == '}') {
    d->p++;
    return;
  }
  for (;;) {
    skip_space(d);
    if (d->p >= d->end || *d->p != '"') fail(d, "expected a string key");
    decode_string(d);
    skip_space(d);
    if (d->p >= d->end || *d->p != ':') fail(d, "expected ':'");
    d->p++;
    decode_value(d, depth + 1);
    lua_rawset(L, -3);
    skip_space(d);
    if (d->p >= d->end) fail(d, "unterminated object");
    if (*d->p == '}') {
      d->p++;
      return;
    }
    if (*d->p != ',') fail(d, "expected ',' or '}'");
    d->p++;
  }
}

static void decode_value(decoder *d, int depth) {
  if (depth >= MAX_JSON_DEPTH) fail(d, "nested too deeply");
  luaL_checkstack(d->L, 4, "json.decode: nested too deeply");
  skip_space(d);
  if (d->p >= d->end) fail(d, "unexpected end of input");
  switch (*d->p) {
    case '{':
      decode_object(d, depth);
      return;
    case '[':
      decode_array(d, depth);
      return;
    case '"':
      decode_string(d);
      return;
    case 't':
      literal(d, "true", 4);
      lua_pushboolean(d->L, 1);
      return;
    case 'f':
      literal(d, "false", 5);
      lua_pushboolean(d->L, 0);
      return;
    case 'n':
      literal(d, "null", 4);
      lua_pushlightuserdata(d->L, NULL);
      return;
    default:
      if (*d->p == '-' || (*d->p >= '0' && *d->p <= '9')) {
        decode_number(d);
        return;
      }
      fail(d, "unexpected character");
  }
}

static int json_decode(lua_State *L) {
  size_t n;
  const char *text = luaL_checklstring(L, 1, &n);
  decoder d = {L, text, text, text + n};
  lua_settop(L, 1);
  decode_value(&d, 0);
  skip_space(&d);
  if (d.p != d.end) fail(&d, "trailing characters");
  return 1;
}

/* ---- encoding ---- */

/* The text being built and the sorted object keys of every table being written; encoding runs no Lua code. */
static plg_buf text;

typedef struct key {
  const char *s;
  size_t n;
} key;

static plg_buf keys;

static void put(lua_State *L, const char *s, size_t n) {
  plg_buf_put(&text, s, n);
  if (text.oom) luaL_error(L, "json.encode: not enough memory");
}

static void encode_string(lua_State *L, const char *s, size_t n) {
  static const char hexdigits[] = "0123456789abcdef";
  put(L, "\"", 1);
  size_t run = 0;
  for (size_t i = 0; i < n;) {
    unsigned char c = (unsigned char)s[i];
    if (c >= 0x20 && c != '"' && c != '\\') {
      size_t len = utf8_len((const unsigned char *)s + i, n - i);
      if (len == 0) luaL_error(L, "json.encode: invalid UTF-8 in string");
      i += len;
      continue;
    }
    put(L, s + run, i - run);
    char esc[6] = {'\\', 0, 0, 0, 0, 0};
    size_t elen = 2;
    switch (c) {
      case '"':
      case '\\':
        esc[1] = (char)c;
        break;
      case '\b':
        esc[1] = 'b';
        break;
      case '\f':
        esc[1] = 'f';
        break;
      case '\n':
        esc[1] = 'n';
        break;
      case '\r':
        esc[1] = 'r';
        break;
      case '\t':
        esc[1] = 't';
        break;
      default:
        memcpy(esc + 1, "u00", 3);
        esc[4] = hexdigits[c >> 4];
        esc[5] = hexdigits[c & 0xF];
        elen = 6;
    }
    put(L, esc, elen);
    run = ++i;
  }
  put(L, s + run, n - run);
  put(L, "\"", 1);
}

static void encode_number(lua_State *L, int idx) {
  char num[40];
  int n;
  if (lua_isinteger(L, idx)) {
    n = snprintf(num, sizeof num, "%lld", (long long)lua_tointeger(L, idx));
  } else {
    double d = (double)lua_tonumber(L, idx);
    if (!isfinite(d)) luaL_error(L, "json.encode: cannot encode %s", isnan(d) ? "NaN" : "infinity");
    /* The shortest of %.15g to %.17g that reads back exactly. */
    for (int prec = 15;; prec++) {
      n = snprintf(num, sizeof num, "%.*g", prec, d);
      if (prec == 17 || strtod(num, NULL) == d) break;
    }
  }
  put(L, num, (size_t)n);
}

static int compare_keys(const void *a, const void *b) {
  const key *x = a, *y = b;
  int c = memcmp(x->s, y->s, x->n < y->n ? x->n : y->n);
  if (c != 0) return c;
  return x->n < y->n ? -1 : x->n > y->n;
}

static void encode_value(lua_State *L, int idx, int depth);

static void encode_array(lua_State *L, int idx, lua_Unsigned n, int depth) {
  put(L, "[", 1);
  for (lua_Unsigned i = 1; i <= n; i++) {
    if (i > 1) put(L, ",", 1);
    lua_rawgeti(L, idx, (lua_Integer)i);
    encode_value(L, lua_gettop(L), depth + 1);
    lua_pop(L, 1);
  }
  put(L, "]", 1);
}

/* encode_object writes the table at idx as an object of its count string keys, sorted. */
static void encode_object(lua_State *L, int idx, size_t count, int depth) {
  size_t first = keys.len / sizeof(key);
  lua_pushnil(L);
  while (lua_next(L, idx) != 0) {
    if (lua_type(L, -2) != LUA_TSTRING)
      luaL_error(L, "json.encode: object keys must be strings, got %s (or make the table a sequence)",
                 luaL_typename(L, -2));
    key k;
    k.s = lua_tolstring(L, -2, &k.n); /* the table keeps the key alive */
    plg_buf_put(&keys, &k, sizeof k);
    if (keys.oom) luaL_error(L, "json.encode: not enough memory");
    lua_pop(L, 1);
  }
  key *mine = (key *)keys.data + first;
  qsort(mine, count, sizeof(key), compare_keys);
  put(L, "{", 1);
  for (size_t i = 0; i < count; i++) {
    mine = (key *)keys.data + first; /* nested objects may move the buffer */
    if (i > 0) put(L, ",", 1);
    encode_string(L, mine[i].s, mine[i].n);
    put(L, ":", 1);
    lua_pushlstring(L, mine[i].s, mine[i].n);
    lua_rawget(L, idx);
    encode_value(L, lua_gettop(L), depth + 1);
    lua_pop(L, 1);
  }
  put(L, "}", 1);
  keys.len = first * sizeof(key);
}

static void encode_table(lua_State *L, int idx, int depth) {
  int marked = 0;
  if (lua_getmetatable(L, idx)) {
    luaL_getmetatable(L, ARRAY_MT);
    marked = lua_rawequal(L, -1, -2) ? 1 : 0;
    lua_pop(L, 1);
    luaL_getmetatable(L, OBJECT_MT);
    if (lua_rawequal(L, -1, -2)) marked = 2;
    lua_pop(L, 2);
  }
  if (marked == 1) {
    encode_array(L, idx, lua_rawlen(L, idx), depth);
    return;
  }
  size_t count = 0;
  lua_Integer max = 0;
  int sequence = 1;
  lua_pushnil(L);
  while (lua_next(L, idx) != 0) {
    count++;
    if (lua_isinteger(L, -2) && lua_tointeger(L, -2) >= 1) {
      if (lua_tointeger(L, -2) > max) max = lua_tointeger(L, -2);
    } else {
      sequence = 0;
    }
    lua_pop(L, 1);
  }
  if (marked == 0 && sequence && count > 0 && (size_t)max == count)
    encode_array(L, idx, (lua_Unsigned)count, depth);
  else
    encode_object(L, idx, count, depth);
}

static void encode_value(lua_State *L, int idx, int depth) {
  if (depth >= MAX_JSON_DEPTH) luaL_error(L, "json.encode: nested too deeply (a cycle?)");
  luaL_checkstack(L, 4, "json.encode: nested too deeply");
  switch (lua_type(L, idx)) {
    case LUA_TNIL:
      put(L, "null", 4);
      return;
    case LUA_TBOOLEAN:
      if (lua_toboolean(L, idx))
        put(L, "true", 4);
      else
        put(L, "false", 5);
      return;
    case LUA_TNUMBER:
      encode_number(L, idx);
      return;
    case LUA_TSTRING: {
      size_t n;
      const char *s = lua_tolstring(L, idx, &n);
      encode_string(L, s, n);
      return;
    }
    case LUA_TTABLE:
      encode_table(L, idx, depth);
      return;
    case LUA_TLIGHTUSERDATA:
      if (lua_touserdata(L, idx) == NULL) {
        put(L, "null", 4);
        return;
      }
      /* fallthrough */
    default:
      luaL_error(L, "json.encode: cannot encode a %s", luaL_typename(L, idx));
  }
}

static int json_encode(lua_State *L) {
  luaL_checkany(L, 1);
  lua_settop(L, 1);
  text.len = 0;
  text.oom = 0;
  keys.len = 0;
  keys.oom = 0;
  encode_value(L, 1, 0);
  lua_pushlstring(L, (const char *)text.data, text.len);
  return 1;
}

static int mark(lua_State *L, const char *mt) {
  if (lua_isnoneornil(L, 1)) {
    lua_settop(L, 0);
    lua_newtable(L);
  }
  luaL_checktype(L, 1, LUA_TTABLE);
  lua_settop(L, 1);
  luaL_setmetatable(L, mt);
  return 1;
}

static int json_array(lua_State *L) {
  return mark(L, ARRAY_MT);
}

static int json_object(lua_State *L) {
  return mark(L, OBJECT_MT);
}

int luaopen_plg_json(lua_State *L) {
  static const luaL_Reg funcs[] = {
      {"decode", json_decode}, {"encode", json_encode}, {"array", json_array}, {"object", json_object}, {NULL, NULL},
  };
  luaL_newmetatable(L, ARRAY_MT);
  luaL_newmetatable(L, OBJECT_MT);
  lua_pop(L, 2);
  luaL_newlib(L, funcs);
  lua_pushlightuserdata(L, NULL);
  lua_setfield(L, -2, "null");
  return 1;
}
