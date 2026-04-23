//go:build linux

package reqrep

import (
	"context"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// sendLocked enqueues data into the request queue. Must be called with the
// arena mutex held. Returns false if the queue or arena is full.
// Port of reqrep_send_locked from reqrep.h.
//
// Memory ordering — tail publish: slot fields are written with plain stores
// before atomic.AddUint64(tail, 1). Per Go's memory model (go.dev/ref/mem,
// "Atomic Values"), if the effect of an atomic operation A is observed by
// atomic operation B, then A is synchronized before B. A consumer that
// observes the incremented tail via atomic.LoadUint64 therefore sees all plain
// slot field writes that preceded the Add. Plain reads in recvLocked are safe.
//
// Memory ordering — head observe: sendLocked reads head via atomic.LoadUint64.
// recvLocked releases the arena (atomic stores to ArenaUsed/ArenaWpos) before
// atomic.AddUint64(head, 1), so by the same rule, the arena release is visible
// to sendLocked once it observes the incremented head.
//
// head and tail are uint64 monotonic counters. At 10^9 requests/second they
// would overflow after ~585 years, so wraparound is not a practical concern.
// If overflow did occur, tail-head would briefly appear >= reqCap (false full),
// causing senders to block momentarily — not corruption.
func sendLocked(s *shm, data []byte, respSlotIdx, respGen uint32) bool {
	if respSlotIdx >= s.respSlots {
		panic("reqrep: sendLocked: respSlotIdx out of bounds")
	}
	head := atomic.LoadUint64(hdrReqHead(s.base))
	tail := atomic.LoadUint64(hdrReqTail(s.base))

	if tail-head >= uint64(s.reqCap) {
		atomic.AddUint64(hdrStatSendFull(s.base), 1)
		return false
	}

	arenaOff, arenaSkip, ok := writeArenaLocked(s, data)
	if !ok {
		return false
	}

	idx := uintptr(tail) & uintptr(s.reqCapMask)
	slot := reqSlotPtr(s.base, s.slotsOff, idx)
	*reqSlotArenaOff(slot) = arenaOff
	*reqSlotPackedLen(slot) = uint32(len(data)) // no UTF-8 flag: Go uses []byte
	*reqSlotArenaSkip(slot) = arenaSkip
	*reqSlotRespSlot(slot) = respSlotIdx
	*reqSlotRespGen(slot) = respGen

	atomic.AddUint64(hdrReqTail(s.base), 1)
	atomic.AddUint64(hdrStatRequests(s.base), 1)
	return true
}

// trySend acquires a resp slot, then enqueues data under the mutex.
// Returns (id, noSlots, err):
//   - id != 0, err == nil: success
//   - err == ErrTooLong: data too large, do not retry
//   - id == 0, noSlots == true: no resp slots available, wait on slot_futex
//   - id == 0, noSlots == false: queue/arena full, wait on send_futex
func trySend(s *shm, data []byte) (id uint64, noSlots bool, err error) {
	if len(data) > int(layoutStrLenMask) {
		return 0, false, ErrTooLong
	}

	slotIdx, ok, gen := acquireRespSlot(s)
	if !ok {
		return 0, true, nil
	}

	mutexLock(s.base)
	sent := sendLocked(s, data, slotIdx, gen)
	mutexUnlock(s.base)

	if sent {
		wakeConsumers(s.base)
		return (uint64(gen) << 32) | uint64(slotIdx), false, nil
	}

	releaseRespSlot(s, slotIdx)
	return 0, false, nil
}

// sendWait is a blocking send that retries until ctx is done or data is queued.
// Port of reqrep_send_wait from reqrep.h.
func sendWait(ctx context.Context, s *shm, data []byte) (uint64, error) {
	id, noSlots, err := trySend(s, data)
	if err != nil || id != 0 {
		return id, err
	}

	for {
		var futexPtr, waitersPtr *uint32
		if noSlots {
			futexPtr = hdrSlotFutex(s.base)
			waitersPtr = hdrSlotWaiters(s.base)
		} else {
			futexPtr = hdrSendFutex(s.base)
			waitersPtr = hdrSendWaiters(s.base)
		}

		fseq := atomic.LoadUint32(futexPtr)

		id, noSlots, err = trySend(s, data)
		if err != nil || id != 0 {
			return id, err
		}

		// Use a periodic 2-second cap so ctx cancellation is detected even if the
		// consumer dies and never calls wakeProducers/wakeSlotWaiters.
		ts := sendTimeout(ctx)
		if ts.Sec == 0 && ts.Nsec == 0 {
			// Deadline passed; ctx.Err() may not be set yet if the timer goroutine
			// hasn't fired, so fall back to DeadlineExceeded.
			if e := ctx.Err(); e != nil {
				return 0, e
			}
			return 0, context.DeadlineExceeded
		}

		atomic.AddUint32(waitersPtr, 1)
		futexWait(futexPtr, fseq, ts)
		atomic.AddUint32(waitersPtr, ^uint32(0))

		if e := ctx.Err(); e != nil {
			return 0, e
		}

		id, noSlots, err = trySend(s, data)
		if err != nil || id != 0 {
			return id, err
		}
	}
}

// recvLocked dequeues one request from the queue. Must be called with the
// arena mutex held. Returns (data, id, true) on success, (nil, 0, false) if empty.
// Port of reqrep_recv_locked from reqrep.h.
//
// Caller invariant: the only caller is tryRecv, which wraps this call with
// mutexLock/mutexUnlock. releaseArenaLocked inside this function also requires
// the mutex, and that requirement is satisfied through the same lock.
//
// Memory ordering: slot fields are read with plain loads after
// atomic.LoadUint64(tail). The happens-before established by sendLocked's
// atomic.AddUint64(tail, 1) guarantees that all slot field writes are visible
// once this goroutine observes the incremented tail. See sendLocked for details.
func recvLocked(s *shm) (data []byte, id uint64, ok bool) {
	head := atomic.LoadUint64(hdrReqHead(s.base))
	tail := atomic.LoadUint64(hdrReqTail(s.base))

	if tail == head {
		// stat_recv_empty is uint32 in the shared layout (unlike the uint64 stat_requests/
		// stat_replies/stat_send_full counters). It wraps at ~4 billion empty polls, which
		// is a protocol-level constraint inherited from the C implementation.
		atomic.AddUint32(hdrStatRecvEmpty(s.base), 1)
		return nil, 0, false
	}

	idx := uintptr(head) & uintptr(s.reqCapMask)
	slot := reqSlotPtr(s.base, s.slotsOff, idx)

	arenaOff := *reqSlotArenaOff(slot)
	packedLen := *reqSlotPackedLen(slot)
	arenaSkip := *reqSlotArenaSkip(slot)
	respSlot := *reqSlotRespSlot(slot)
	respGen := *reqSlotRespGen(slot)

	length := packedLen & layoutStrLenMask

	// Copy before releasing the arena: the arena may be reused immediately.
	data = make([]byte, length)
	copy(data, readArena(s, arenaOff, length))

	releaseArenaLocked(s, arenaSkip)

	id = (uint64(respGen) << 32) | uint64(respSlot)
	atomic.AddUint64(hdrReqHead(s.base), 1)
	return data, id, true
}

// tryRecv performs a non-blocking dequeue.
func tryRecv(s *shm) (data []byte, id uint64, ok bool) {
	mutexLock(s.base)
	data, id, ok = recvLocked(s)
	mutexUnlock(s.base)
	if ok {
		wakeProducers(s.base)
	}
	return
}

// recvWait blocks until a request arrives or ctx is done.
// Uses a 2-second periodic futex timeout for stale mutex recovery.
// Port of reqrep_recv_wait from reqrep.h.
func recvWait(ctx context.Context, s *shm) (data []byte, id uint64, err error) {
	if data, id, ok := tryRecv(s); ok {
		return data, id, nil
	}

	// Same 2-consecutive-ETIMEDOUT guard as mutexLock: require the same holder
	// PID across two independent 2-second checks before calling recoverStaleMutex.
	// A single ETIMEDOUT is not enough because pidAlive can transiently return
	// false in cross-namespace container environments (see mutexLock for details).
	var recvConsecutiveTimeouts int
	var recvLastObservedVal uint32

	for {
		fseq := atomic.LoadUint32(hdrRecvFutex(s.base))

		if data, id, ok := tryRecv(s); ok {
			return data, id, nil
		}

		if e := ctx.Err(); e != nil {
			return nil, 0, e
		}

		ts := recvTimeout(ctx)

		atomic.AddUint32(hdrRecvWaiters(s.base), 1)
		futexErr := futexWait(hdrRecvFutex(s.base), fseq, ts)
		atomic.AddUint32(hdrRecvWaiters(s.base), ^uint32(0))

		// Periodic stale mutex recovery on ETIMEDOUT — requires 2 consecutive
		// timeouts with the same holder value before acting (mirrors mutexLock).
		if futexErr == syscall.ETIMEDOUT {
			val := atomic.LoadUint32(hdrMutex(s.base))
			if val >= layoutMutexWriterBit {
				if !pidAlive(val & layoutMutexPidMask) {
					if val == recvLastObservedVal {
						recvConsecutiveTimeouts++
					} else {
						recvConsecutiveTimeouts = 1
						recvLastObservedVal = val
					}
					if recvConsecutiveTimeouts >= 2 {
						recoverStaleMutex(s.base, val)
						recvConsecutiveTimeouts = 0
					}
				} else {
					recvConsecutiveTimeouts = 0
					recvLastObservedVal = 0
				}
			}
		}

		if e := ctx.Err(); e != nil {
			return nil, 0, e
		}

		if data, id, ok := tryRecv(s); ok {
			return data, id, nil
		}
	}
}

// ctxTimespec returns the remaining time until ctx's deadline as a Timespec,
// or nil if ctx has no deadline. Returns a zero Timespec if already expired.
func ctxTimespec(ctx context.Context) *unix.Timespec {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil
	}
	d := time.Until(deadline)
	if d <= 0 {
		return &unix.Timespec{}
	}
	return durationToTimespec(d)
}

// periodicTimeout returns min(cap, remaining ctx time). Never returns nil.
// Used by recvWait and sendWait so that a dead peer does not block either
// side indefinitely regardless of whether ctx has a deadline.
func periodicTimeout(ctx context.Context, cap time.Duration) *unix.Timespec {
	deadline, ok := ctx.Deadline()
	if !ok {
		return durationToTimespec(cap)
	}
	d := time.Until(deadline)
	if d <= 0 {
		return &unix.Timespec{}
	}
	if d >= cap {
		return durationToTimespec(cap)
	}
	return durationToTimespec(d)
}

func recvTimeout(ctx context.Context) *unix.Timespec { return periodicTimeout(ctx, 2*time.Second) }
func sendTimeout(ctx context.Context) *unix.Timespec { return periodicTimeout(ctx, 2*time.Second) }

func durationToTimespec(d time.Duration) *unix.Timespec {
	if d < 0 {
		d = 0
	}
	sec := int64(d / time.Second)
	nsec := int64(d % time.Second)
	return &unix.Timespec{Sec: sec, Nsec: nsec}
}
