/*
** The globals through which a Lua plugin reaches its host:
**
**   host.<Method>(args...)  calls the registered Host's method (only with plg.WithHost): arguments and results map
**                           as for plugin methods (several results come back as one array table); a Host error
**                           raises a Lua error with the Host's message.
**   http.request{method = "GET", url = "...", headers = {name = "value" or {"v1", "v2"}}, body = "...",
**                redirect = true}
**                           makes a request through the Register's HTTP policy, following redirects, and
**                           returns {status = 200, headers = {name = "v1, v2"}, body = "..."}, or nil and the
**                           reason the host refused or failed it.
*/
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "lauxlib.h"
#include "plg.h"

#define IMPORT(name) __attribute__((import_module("plg"), import_name(name)))

IMPORT("host_call") uint32_t plg_host_call(uint32_t method, uint32_t ptr, uint32_t len);
IMPORT("http_roundtrip") uint32_t plg_http_roundtrip(uint32_t ptr, uint32_t len);
IMPORT("take_result") void plg_take_result(uint32_t ptr, uint32_t len);
IMPORT("http_start") uint32_t plg_http_start(uint32_t call, uint32_t ptr, uint32_t len);

/*
** The scratch of a Host call or a blocking http request: the request being encoded, the answer being decoded and a
** conversion failure. Lua code runs meanwhile (metamethods, and the finalizers any allocation may run), and may
** make another Host call or request: every one runs in a region (below), and one started in another's gets a
** scratch of its own, so the outer one's buffers stay as they were.
*/
typedef struct scratch {
  plg_buf request, answer;
  plg_conv conv;
} scratch;

static scratch shared;         /* the scratch of the outermost region, kept from one to the next */
static scratch *scr = &shared; /* the scratch of the region running */

/*
** The C stack address of the region using the shared scratch, or 0. The stack grows down, so a region started in
** it runs below that address. A region that failed leaves its address behind: one starting at or above it cannot
** have been started in it, and takes the shared scratch over.
*/
static uintptr_t owner;

/* A region's body: it returns its results' count, or -1 with an error on the stack's top for the region to raise. */
typedef int (*body)(lua_State *L, uint32_t arg);

/* The body and argument of the nested region starting, for run_nested. */
static body nested_body;
static uint32_t nested_arg;

static uint32_t addr(const void *p) {
  return (uint32_t)(uintptr_t)p;
}

static int run_nested(lua_State *L) {
  int n = nested_body(L, nested_arg);
  return n < 0 ? lua_error(L) : n;
}

/*
** region runs b(L, arg) and returns its results. The outermost region uses the shared scratch, at no cost; one
** started inside another uses a fresh scratch, freed when it ends, and runs protected so that a Lua error frees it
** too and restores the outer one's before it is raised again.
*/
static int region(lua_State *L, body b, uint32_t arg) {
  char here;
  uintptr_t at = (uintptr_t)&here;
  if (owner == 0 || at >= owner) {
    owner = at;
    int n = b(L, arg);
    owner = 0;
    return n < 0 ? lua_error(L) : n;
  }
  scratch mine = {0}, *outer = scr;
  scr = &mine;
  nested_body = b;
  nested_arg = arg;
  int status = plg_protect(L, run_nested);
  scr = outer;
  free(mine.request.data);
  free(mine.answer.data);
  free(mine.conv.path.data);
  return status == LUA_OK ? lua_gettop(L) : lua_error(L);
}

/* take copies the host's pending answer of n bytes into the scratch's answer and returns a reader over it. */
static plg_reader take(lua_State *L, uint32_t n) {
  plg_buf *answer = &scr->answer;
  answer->len = 0;
  answer->oom = 0;
  if (n == 0 || !plg_buf_reserve(answer, n)) luaL_error(L, "not enough memory for the host's answer");
  plg_take_result(addr(answer->data), n);
  answer->len = n;
  return (plg_reader){answer->data, answer->data + n};
}

static void check_request(lua_State *L) {
  if (scr->request.oom) luaL_error(L, "not enough memory for the request");
}

const char *plg_push_results(lua_State *L, const plg_method *m, plg_reader *r, int *pushed) {
  *pushed = m->nresults > 1 ? 1 : (int)m->nresults;
  if (m->nresults > 1) lua_createtable(L, (int)m->nresults, 0);
  for (uint32_t k = 0; k < m->nresults; k++) {
    const char *err = plg_push_value(L, &plg_the_schema, m->results[k], r, 0);
    if (err != NULL) return err;
    if (m->nresults > 1) lua_rawseti(L, -2, (lua_Integer)k + 1);
  }
  return r->p == r->end ? NULL : "trailing bytes";
}

/* host_call makes the Host call of method index in a region. */
static int host_call(lua_State *L, uint32_t index) {
  const plg_method *m = &plg_the_schema.methods[index];
  int nargs = lua_gettop(L);
  luaL_checkstack(L, 4, NULL);
  scr->request.len = 0;
  scr->request.oom = 0;
  for (uint32_t k = 0; k < m->nparams; k++) {
    int idx = (int)k + 1;
    if (idx > nargs) {
      lua_pushnil(L);
      idx = lua_gettop(L);
    }
    int err = plg_encode_value(L, &plg_the_schema, m->params[k], idx, &scr->request, &scr->conv, 0);
    check_request(L);
    if (err < 0) {
      char prefix[160];
      snprintf(prefix, sizeof prefix, "host.%s: parameter %u", m->name, (unsigned)k + 1);
      plg_conv_push(L, &scr->conv, prefix);
      return -1;
    }
  }
  uint32_t n = plg_host_call(index - plg_iface_methods, addr(scr->request.data), (uint32_t)scr->request.len);
  plg_reader r = take(L, n);
  uint8_t status = *r.p++;
  if (status != PLG_OK) {
    const char *msg;
    size_t len;
    if (plg_read_str(&r, &msg, &len))
      lua_pushlstring(L, msg, len);
    else
      lua_pushfstring(L, "host.%s: malformed answer", m->name);
    return -1;
  }
  lua_settop(L, 0);
  int pushed;
  const char *err = plg_push_results(L, m, &r, &pushed);
  if (err != NULL) {
    lua_pushfstring(L, "host.%s: malformed answer: %s", m->name, err);
    return -1;
  }
  return pushed;
}

static int host_method(lua_State *L) {
  return region(L, host_call, (uint32_t)lua_tointeger(L, lua_upvalueindex(1)));
}

void plg_open_host(lua_State *L) {
  if (plg_the_schema.nmethods == plg_iface_methods) return; /* no Host: no host global */
  lua_createtable(L, 0, (int)(plg_the_schema.nmethods - plg_iface_methods));
  for (uint32_t i = plg_iface_methods; i < plg_the_schema.nmethods; i++) {
    lua_pushinteger(L, (lua_Integer)i);
    lua_pushcclosure(L, host_method, 1);
    lua_setfield(L, -2, plg_the_schema.methods[i].name);
  }
  lua_setglobal(L, "host");
}

/* ---- http ---- */

static void put_text(const char *text, size_t n) {
  plg_buf_str(&scr->request, text, n);
}

/* put_headers appends the header pairs of the table at idx and returns their count. */
static uint64_t put_headers(lua_State *L, int idx) {
  uint64_t pairs = 0;
  lua_pushnil(L);
  while (lua_next(L, idx) != 0) {
    if (lua_type(L, -2) != LUA_TSTRING) luaL_error(L, "http.request: header names must be strings");
    size_t nlen, vlen;
    const char *name = lua_tolstring(L, -2, &nlen);
    if (lua_type(L, -1) == LUA_TSTRING) {
      const char *value = lua_tolstring(L, -1, &vlen);
      put_text(name, nlen);
      put_text(value, vlen);
      pairs++;
    } else if (lua_istable(L, -1)) {
      lua_Unsigned n = lua_rawlen(L, -1);
      for (lua_Unsigned i = 1; i <= n; i++) {
        if (lua_rawgeti(L, -1, (lua_Integer)i) != LUA_TSTRING)
          luaL_error(L, "http.request: header %s: want strings, got %s", name, luaL_typename(L, -1));
        const char *value = lua_tolstring(L, -1, &vlen);
        put_text(name, nlen);
        put_text(value, vlen);
        lua_pop(L, 1);
        pairs++;
      }
    } else {
      luaL_error(L, "http.request: header %s: want a string or a list of strings, got %s", name,
                 luaL_typename(L, -1));
    }
    lua_pop(L, 1);
  }
  return pairs;
}

/* push_response pushes the response table of a StatusOK answer. */
static int push_response(lua_State *L, plg_reader *r) {
  uint64_t code, pairs;
  if (!plg_read_u64(r, &code) || !plg_read_u64(r, &pairs) || pairs > (uint64_t)(r->end - r->p) / 8)
    return luaL_error(L, "http.request: malformed answer");
  lua_createtable(L, 0, 3);
  lua_pushinteger(L, (lua_Integer)code);
  lua_setfield(L, -2, "status");
  lua_createtable(L, 0, (int)pairs);
  for (uint64_t i = 0; i < pairs; i++) {
    const char *name, *value;
    size_t nlen, vlen;
    if (!plg_read_str(r, &name, &nlen) || !plg_read_str(r, &value, &vlen))
      return luaL_error(L, "http.request: malformed answer");
    lua_pushlstring(L, name, nlen);
    if (lua_rawget(L, -2) == LUA_TSTRING) {
      lua_pushliteral(L, ", ");
      lua_pushlstring(L, value, vlen);
      lua_concat(L, 3);
    } else {
      lua_pop(L, 1);
      lua_pushlstring(L, value, vlen);
    }
    lua_pushlstring(L, name, nlen);
    lua_insert(L, -2);
    lua_rawset(L, -3);
  }
  lua_setfield(L, -2, "headers");
  lua_pushlstring(L, (const char *)r->p, (size_t)(r->end - r->p));
  lua_setfield(L, -2, "body");
  return 1;
}

/* answered returns http.request's values for an answer: the response table, or nil and the host's reason. */
static int answered(lua_State *L, plg_reader *r) {
  uint8_t status = *r->p++;
  if (status == PLG_OK) return push_response(L, r);
  const char *msg;
  size_t len;
  if (!plg_read_str(r, &msg, &len)) return luaL_error(L, "http.request: malformed answer");
  luaL_pushfail(L);
  lua_pushlstring(L, msg, len);
  return 2;
}

static int roundtrip_answered(lua_State *L, int status, lua_KContext k);

/* put_request encodes the request of roundtrip's arguments into the scratch's request and returns it. */
static plg_buf *put_request(lua_State *L) {
  size_t mlen, ulen, blen = 0;
  const char *method = luaL_checklstring(L, 1, &mlen);
  const char *url = luaL_checklstring(L, 2, &ulen);
  const char *body = luaL_optlstring(L, 4, "", &blen);
  int headers = lua_istable(L, 3);
  lua_settop(L, 3);

  plg_buf *request = &scr->request;
  request->len = 0;
  request->oom = 0;
  put_text(method, mlen);
  put_text(url, ulen);
  size_t at = request->len;
  plg_buf_u64(request, 0);
  uint64_t pairs = headers ? put_headers(L, 3) : 0;
  if (!request->oom)
    for (int i = 0; i < 8; i++) request->data[at + (size_t)i] = (uint8_t)(pairs >> (8 * i));
  plg_buf_put(request, body, blen);
  check_request(L);
  return request;
}

/* roundtrip_blocking makes roundtrip's request in a region, blocking until its answer. */
static int roundtrip_blocking(lua_State *L, uint32_t arg) {
  (void)arg;
  plg_buf *request = put_request(L);
  plg_reader r = take(L, plg_http_roundtrip(addr(request->data), (uint32_t)request->len));
  return answered(L, &r);
}

/*
** roundtrip(method, url, headers, body) makes one request through the host: the prelude below has checked the
** arguments' types (headers is a table or nil, body a string or nil). In a multiplexed step it yields until the
** answer arrives, letting the instance serve other calls meanwhile; elsewhere it blocks, in a region. A yielding
** request uses the shared scratch outside a region: no region can be running, since none spans a yield and one
** running below would make the coroutine unyieldable, and encoding the request runs no Lua code; its answer
** arrives in plg_answer.
*/
static int roundtrip(lua_State *L) {
  if (L != plg_call_thread || !lua_isyieldable(L)) return region(L, roundtrip_blocking, 0);
  plg_buf *request = put_request(L);
  plg_wait_request = plg_http_start(plg_call_id, addr(request->data), (uint32_t)request->len);
  lua_pushlightuserdata(L, &plg_yield_key);
  return lua_yieldk(L, 1, 0, roundtrip_answered);
}

/* roundtrip_answered continues a roundtrip that yielded for its answer, which the resuming step put in plg_answer. */
static int roundtrip_answered(lua_State *L, int status, lua_KContext k) {
  (void)status;
  (void)k;
  plg_reader r = plg_answer;
  if (r.p == r.end) return luaL_error(L, "http.request: malformed answer");
  return answered(L, &r);
}

/*
** http.request, in Lua: it checks the request and follows redirects (up to 10; set redirect = false to get the
** 3xx response instead) as Go's http.Client and fetch do, each hop being a new request the host checks against
** the policy. A 303, and a 301 or 302 to anything but GET or HEAD, continues as a GET without a body. Once a
** redirect leaves the first request's origin (scheme, host and port), this hop and every later one go without
** the credential headers (Authorization, Cookie, ...): a copy of req.headers without them, req.headers itself
** left as it is.
*/
static const char prelude[] =
    "local roundtrip = ...\n"
    "local function check(value, name, want, optional)\n"
    "  if (value == nil and optional) or type(value) == want then return end\n"
    "  error('http.request: ' .. name .. ': want a ' .. want .. ', got ' .. type(value), 3)\n"
    "end\n"
    "local function resolve(base, loc)\n"
    "  if loc:find('^%a[%w+.-]*:') then return loc end\n"
    "  local scheme, authority, path = base:match('^(%a[%w+.-]*:)//([^/?#]*)([^?#]*)')\n"
    "  if not scheme then return nil end\n"
    "  if loc:sub(1, 2) == '//' then return scheme .. loc end\n"
    "  if loc:sub(1, 1) == '/' then return scheme .. '//' .. authority .. loc end\n"
    "  if loc:sub(1, 1) == '?' then return scheme .. '//' .. authority .. path .. loc end\n"
    "  return scheme .. '//' .. authority .. (path:match('^(.*/)') or '/') .. loc\n"
    "end\n"
    /* origin returns url's scheme, host and port (its scheme's default port if it has none), ASCII letters in
       lower case, or nil for a URL without an authority. */
    "local ports = { http = '80', https = '443' }\n"
    "local function origin(url)\n"
    "  local scheme, authority = url:match('^(%a[%w+.-]*)://([^/?#]*)')\n"
    "  if not scheme then return nil end\n"
    "  scheme = scheme:lower()\n"
    "  local hostport = authority:match('^.*@(.*)$') or authority\n"
    "  local host, port = hostport:match('^(%b[]):?(.*)$')\n"
    "  if not host then host, port = hostport:match('^([^:]*):?(.*)$') end\n"
    "  if port == '' then port = ports[scheme] or '' end\n"
    "  return scheme .. '://' .. host:lower() .. ':' .. port\n"
    "end\n"
    /* The request headers carrying credentials (net/http's list), which a redirect never takes to another
       origin. */
    "local credentials = { authorization = true, cookie = true, cookie2 = true, ['proxy-authenticate'] = true,\n"
    "  ['proxy-authorization'] = true, ['www-authenticate'] = true }\n"
    "local function uncredentialed(headers)\n"
    "  local copy = {}\n"
    "  for name, value in next, headers do\n"
    "    if type(name) ~= 'string' or not credentials[name:lower()] then copy[name] = value end\n"
    "  end\n"
    "  return copy\n"
    "end\n"
    "local redirects = { [301] = true, [302] = true, [303] = true, [307] = true, [308] = true }\n"
    "return function(req)\n"
    "  check(req, 'request', 'table')\n"
    "  local method, url, headers, body = req.method or 'GET', req.url, req.headers, req.body\n"
    "  check(method, 'method', 'string')\n"
    "  check(url, 'url', 'string')\n"
    "  check(headers, 'headers', 'table', true)\n"
    "  check(body, 'body', 'string', true)\n"
    "  local start, first, left = url, nil, false\n"
    "  for _ = 0, 10 do\n"
    "    local resp, err = roundtrip(method, url, headers, body)\n"
    "    if not resp then return nil, err end\n"
    "    local loc, status = resp.headers.Location, resp.status\n"
    "    local nexturl = req.redirect ~= false and redirects[status] and loc and resolve(url, loc)\n"
    "    if not nexturl then return resp end\n"
    "    url = nexturl\n"
    "    if not left then\n"
    "      first = first or origin(start)\n"
    "      if first == nil or origin(url) ~= first then\n"
    "        left = true\n"
    "        if headers then headers = uncredentialed(headers) end\n"
    "      end\n"
    "    end\n"
    "    if status == 303 or (status <= 302 and method ~= 'GET' and method ~= 'HEAD') then\n"
    "      method, body = 'GET', nil\n"
    "    end\n"
    "  end\n"
    "  return nil, 'http.request: stopped after 10 redirects'\n"
    "end\n";

void plg_open_http(lua_State *L) {
  lua_createtable(L, 0, 1);
  if (luaL_loadbufferx(L, prelude, sizeof prelude - 1, "=http", "t") != LUA_OK) lua_error(L);
  lua_pushcfunction(L, roundtrip);
  lua_call(L, 1, 1);
  lua_setfield(L, -2, "request");
  lua_setglobal(L, "http");
}
