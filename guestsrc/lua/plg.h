/*
** The Lua guest of plg: shared declarations of the glue (glue.c), the wire codec (codec.c), the host and http
** globals (capi.c) and the bundled json module (json.c).
*/
#ifndef PLG_LUA_H
#define PLG_LUA_H

#include <stddef.h>
#include <stdint.h>

#include "lua.h"

/* Result statuses: the first byte of every result buffer (internal/wire Status). */
enum {
  PLG_OK = 1,
  PLG_ERROR = 2,
  PLG_PANIC = 3,
  PLG_LOAD = 4,
  PLG_ABI = 5,
  PLG_CONVERT = 6
};

/* Schema kinds (internal/wire Kind). */
enum {
  KIND_BOOL = 1,
  KIND_INT,
  KIND_UINT,
  KIND_FLOAT,
  KIND_STRING,
  KIND_BYTES,
  KIND_TIME,
  KIND_ERROR,
  KIND_SLICE,
  KIND_MAP,
  KIND_POINTER,
  KIND_STRUCT
};

/* wire.MaxDepth: nesting of slices, maps, pointers and structs inside one value. */
#define PLG_MAX_DEPTH 10000

/* wire.MaxSafeInt: the largest integer magnitude that crosses. */
#define PLG_MAX_SAFE_INT (((int64_t)1) << 53)

/* The length prefix of a nil slice, map or []byte. */
#define PLG_NIL_LENGTH 0xFFFFFFFFu

typedef struct plg_field {
  const char *name; /* NUL-terminated, interned by the schema */
  size_t name_len;
  uint32_t type;
} plg_field;

typedef struct plg_type {
  uint8_t kind;
  uint8_t bits;      /* numeric kinds */
  uint32_t elem;     /* slices, maps, pointers */
  uint32_t min;      /* bytes of the smallest encoding, against which decoders check length prefixes */
  uint8_t sizing;    /* min's state while the schema is parsed: 0 unknown, 1 being computed, 2 known */
  uint32_t nfields;  /* structs */
  plg_field *fields; /* structs */
} plg_type;

typedef struct plg_method {
  const char *name;
  uint32_t nparams, nresults;
  uint32_t *params, *results;
} plg_method;

typedef struct plg_schema {
  uint32_t ntypes, nmethods;
  plg_type *types;
  plg_method *methods;
} plg_schema;

/* A growable byte buffer on the C heap. A failed growth sets oom and keeps the buffer as it was. */
typedef struct plg_buf {
  uint8_t *data;
  size_t len, cap;
  int oom;
} plg_buf;

/* A bounded reader over a byte range. */
typedef struct plg_reader {
  const uint8_t *p, *end;
} plg_reader;

/* Buffers and readers (codec.c). The readers return 1, or 0 when the input is too short. */
int plg_buf_reserve(plg_buf *b, size_t more);
void plg_buf_put(plg_buf *b, const void *src, size_t n);
void plg_buf_u32(plg_buf *b, uint32_t v);
void plg_buf_u64(plg_buf *b, uint64_t v);
void plg_buf_str(plg_buf *b, const char *s, size_t n);
void plg_buf_patch_u32(plg_buf *b, size_t at, uint32_t v);

int plg_read_u8(plg_reader *r, uint8_t *v);
int plg_read_u32(plg_reader *r, uint32_t *v);
int plg_read_u64(plg_reader *r, uint64_t *v);
int plg_read_bytes(plg_reader *r, size_t n, const uint8_t **p);
int plg_read_str(plg_reader *r, const char **s, size_t *n);

/* Schema (codec.c). plg_schema_parse returns NULL on success, else a static message. */
const char *plg_schema_parse(plg_reader *r, plg_schema *s);

/*
** Values (codec.c). plg_push_value decodes one wire value of type t and pushes it; it returns NULL or a static
** message for a malformed encoding. It raises Lua errors only for memory errors.
*/
const char *plg_push_value(lua_State *L, const plg_schema *s, uint32_t t, plg_reader *r, int depth);

/*
** A conversion failure: the message, and the path to the failing value (".field", "[i]", "[\"key\"]" segments),
** prepended by each level on the way up.
*/
typedef struct plg_conv {
  plg_buf path;
  int cut; /* the path reached its maximum length and starts with "..." */
  char msg[192];
} plg_conv;

/*
** plg_encode_value encodes the Lua value at the absolute index idx as type t into b. It returns 0, or -1 having
** filled cv. It may raise Lua errors (metamethods, memory); callers run it under lua_pcall. A failed growth of b
** sets b->oom and fails with -1.
*/
int plg_encode_value(lua_State *L, const plg_schema *s, uint32_t t, int idx, plg_buf *b, plg_conv *cv, int depth);

/* Formats cv's message. */
void plg_conv_fail(plg_conv *cv, const char *fmt, ...);

/* Pushes "<prefix>: <path>: <message>" (the path without a leading dot, omitted when empty) and resets cv. */
void plg_conv_push(lua_State *L, plg_conv *cv, const char *prefix);

/* The guest's schema: the interface's methods (plg_call indexes), then the Host's (glue.c). */
extern plg_schema plg_the_schema;
extern uint32_t plg_iface_methods;

/* Globals (capi.c, json.c). */
void plg_open_host(lua_State *L);
void plg_open_http(lua_State *L);
int luaopen_plg_json(lua_State *L);

/* Pushes the values of a status-OK body of method m's results: nothing, one value or an array table. */
const char *plg_push_results(lua_State *L, const plg_method *m, plg_reader *r, int *pushed);

/*
** Multiplexed steps (glue.c). While a step runs a call's coroutine, plg_call_thread is that coroutine and
** plg_call_id the host's number for the call (otherwise plg_call_thread is NULL). http.request started there
** hands its request to the host with http_start, sets plg_wait_request to the request's number and yields
** &plg_yield_key; the step that resumes it sets plg_answer to the host's answer.
*/
extern lua_State *plg_call_thread;
extern uint32_t plg_call_id;
extern uint32_t plg_wait_request;
extern plg_reader plg_answer;
extern char plg_yield_key;

#endif
