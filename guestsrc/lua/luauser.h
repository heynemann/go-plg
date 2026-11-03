/*
** The Lua guest's configuration of Lua itself, included by lua.h through LUA_USER_H (luaconf.h's local
** configuration), so the released sources build unchanged.
*/
#ifndef PLG_LUAUSER_H
#define PLG_LUAUSER_H

/*
** Lua's stack limit, in slots. The default (1000000 slots, 16 MiB, plus the call frames) does not fit the
** default 64 MiB instance alongside a plugin's data, so unbounded recursion would run the instance out of memory
** (and discard it) instead of raising Lua's ordinary, catchable "stack overflow" error. 200000 slots still allow
** tens of thousands of nested Lua calls.
*/
#undef LUAI_MAXSTACK
#define LUAI_MAXSTACK 200000

/* Lua's hooks need no signals: wasip1 has none, and nothing interrupts the guest asynchronously. */
#define l_signalT int

/*
** table.sort randomizes its pivot from clock(), which wasip1 lacks, only once a partition is badly unbalanced;
** a fixed value is the alternative its source suggests. A plugin can only slow down its own call, which its
** deadline bounds.
*/
#define l_randomizePivot() (~0u)

#endif
