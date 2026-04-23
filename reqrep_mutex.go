//go:build linux

package reqrep

import (
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// mutexLock acquires the arena mutex. Exact port of reqrep_mutex_lock from reqrep.h:
// spin(32) → futex wait with 2s timeout → stale recovery on ETIMEDOUT.
//
// PID-reuse race — two known failure modes:
//
//   (a) Deadlock: original holder died, its PID was immediately reused by an
//       unrelated process. pidAlive returns true, recovery is skipped, and the
//       mutex stays locked indefinitely.
//
//   (b) Corruption: pidAlive returns false for a still-live holder (possible in
//       cross-namespace container environments). recoverStaleMutex then frees the
//       mutex while the original holder still thinks it owns it, allowing two
//       goroutines into the critical section concurrently.
//
// Mitigation for (b): recoverStaleMutex is called only after 2 consecutive
// ETIMEDOUT intervals (≥4 s) with the same holder value and pidAlive returning
// false. A transient false negative from pidAlive is extremely unlikely to persist
// across two independent 2-second checks, so this substantially reduces corruption
// risk. A full solution would require a per-lock generation counter.
// See also: recoverStaleMutex, pidAlive.
func mutexLock(base unsafe.Pointer) {
	mypid := layoutMutexWriterBit | (uint32(syscall.Getpid()) & layoutMutexPidMask)
	mutexPtr := hdrMutex(base)
	waitersPtr := hdrMutexWaiters(base)

	var consecutiveTimeouts int
	var lastObservedVal uint32

	for {
		for spin := 0; spin < layoutSpinLimit; spin++ {
			if atomic.CompareAndSwapUint32(mutexPtr, 0, mypid) {
				return
			}
			spinPause()
		}

		atomic.AddUint32(waitersPtr, 1)
		cur := atomic.LoadUint32(mutexPtr)
		if cur != 0 {
			ts := unix.Timespec{Sec: layoutLockTimeoutSec}
			err := futexWait(mutexPtr, cur, &ts)
			if err == syscall.ETIMEDOUT {
				atomic.AddUint32(waitersPtr, ^uint32(0))
				val := atomic.LoadUint32(mutexPtr)
				if val >= layoutMutexWriterBit {
					if !pidAlive(val & layoutMutexPidMask) {
						if val == lastObservedVal {
							consecutiveTimeouts++
						} else {
							consecutiveTimeouts = 1
							lastObservedVal = val
						}
						if consecutiveTimeouts >= 2 {
							recoverStaleMutex(base, val)
							consecutiveTimeouts = 0
						}
					} else {
						consecutiveTimeouts = 0
						lastObservedVal = 0
					}
				}
				continue
			}
		}
		atomic.AddUint32(waitersPtr, ^uint32(0))
		// Fast path: try to acquire immediately after being woken before
		// re-entering the spin loop.
		if atomic.CompareAndSwapUint32(mutexPtr, 0, mypid) {
			return
		}
	}
}

// mutexUnlock releases the arena mutex. Port of reqrep_mutex_unlock.
func mutexUnlock(base unsafe.Pointer) {
	mutexPtr := hdrMutex(base)
	atomic.StoreUint32(mutexPtr, 0)
	if atomic.LoadUint32(hdrMutexWaiters(base)) > 0 {
		futexWake(mutexPtr, 1)
	}
}

// recoverStaleMutex CAS-frees a mutex held by a dead process.
// Port of reqrep_recover_stale_mutex.
//
// Limitation: recovery restores the mutex to the unlocked state but cannot
// detect or repair arena data that the dead holder may have left partially
// written. Callers must tolerate the possibility of a corrupt in-flight
// request slot after recovery.
func recoverStaleMutex(base unsafe.Pointer, observed uint32) {
	mutexPtr := hdrMutex(base)
	if !atomic.CompareAndSwapUint32(mutexPtr, observed, 0) {
		return
	}
	atomic.AddUint32(hdrStatRecoveries(base), 1)
	if atomic.LoadUint32(hdrMutexWaiters(base)) > 0 {
		futexWake(mutexPtr, 1)
	}
}

// pidAlive reports whether the process pid exists.
// Port of reqrep_pid_alive: kill(pid, 0) returns ESRCH only if the process is gone.
//
// Limitation: kill(pid, 0) is PID-namespace-local. In a containerised environment
// where the mutex holder lives in a different PID namespace, the same numeric PID
// may refer to an unrelated process (or nothing), producing a false positive or
// false negative. For the PID-reuse deadlock risk this creates, see mutexLock.
func pidAlive(pid uint32) bool {
	if pid == 0 {
		return true
	}
	err := syscall.Kill(int(pid), 0)
	return err == nil || err == syscall.EPERM
}
