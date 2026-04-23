//go:build linux

package reqrep

import (
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// FUTEX_WAIT and FUTEX_WAKE without FUTEX_PRIVATE_FLAG — must match the C
// implementation which uses plain FUTEX_WAIT/WAKE (no PRIVATE flag).
const (
	futexWaitOp = 0 // FUTEX_WAIT
	futexWakeOp = 1 // FUTEX_WAKE
)

// futexWait blocks until *addr != val or the timeout expires.
// timeout nil means no timeout. Returns nil on EAGAIN and EINTR (spurious wakeup).
func futexWait(addr *uint32, val uint32, timeout *unix.Timespec) error {
	_, _, errno := unix.Syscall6(
		unix.SYS_FUTEX,
		uintptr(unsafe.Pointer(addr)),
		futexWaitOp,
		uintptr(val),
		uintptr(unsafe.Pointer(timeout)),
		0, 0,
	)
	if errno != 0 && errno != syscall.EAGAIN && errno != syscall.EINTR {
		return errno
	}
	return nil
}

// futexWake wakes up to n waiters on addr.
func futexWake(addr *uint32, n uint32) {
	unix.Syscall6(
		unix.SYS_FUTEX,
		uintptr(unsafe.Pointer(addr)),
		futexWakeOp,
		uintptr(n),
		0, 0, 0,
	)
}

// spinPause is a cooperative scheduler yield, not an x86 PAUSE instruction.
// Go provides no direct access to the hardware PAUSE/YIELD instruction; Gosched
// is the idiomatic equivalent for inter-goroutine spin loops.
func spinPause() {
	runtime.Gosched()
}
