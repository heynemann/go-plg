//go:build unix || windows

package plg

import "syscall"

// oNonblock is the open flag that keeps an open from waiting (see nonblockingRead).
const oNonblock = syscall.O_NONBLOCK
