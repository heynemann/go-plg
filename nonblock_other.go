//go:build !unix && !windows

package plg

// oNonblock is the open flag that keeps an open from waiting (see nonblockingRead). The syscall package of these
// platforms (js and wasip1) has no such flag for os.OpenFile, so their opens are as os.Root's own.
const oNonblock = 0
