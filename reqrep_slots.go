//go:build linux

package reqrep

import (
	"sync/atomic"
	"syscall"
)

// acquireRespSlot scans resp slots for a free one and CAS FREE→ACQUIRED.
// On success returns (slotIndex, true, generation). Returns (0, false, 0) if none available.
// Port of reqrep_slot_acquire from reqrep.h.
func acquireRespSlot(s *shm) (idx uint32, ok bool, gen uint32) {
	hint := atomic.LoadUint32(hdrRespHint(s.base))
	mypid := uint32(syscall.Getpid())
	n := s.respSlots

	// First pass: scan from hint looking for a FREE slot.
	for i := uint32(0); i < n; i++ {
		slotIdx := (hint + i) % n
		slot := respSlotPtr(s.base, s.respOff, s.respStride, uintptr(slotIdx))
		if atomic.CompareAndSwapUint32(respSlotState(slot), layoutRespFree, layoutRespAcquired) {
			atomic.StoreUint32(respSlotOwnerPid(slot), mypid)
			gen = atomic.AddUint32(respSlotGeneration(slot), 1)
			if gen == 0 {
				// gen==0 with slotIdx==0 produces id==0, which sendWait uses as a failure sentinel.
				gen = atomic.AddUint32(respSlotGeneration(slot), 1)
			}
			atomic.StoreUint32(hdrRespHint(s.base), (slotIdx+1)%n)
			return slotIdx, true, gen
		}
	}

	// Second pass: stale recovery — free slots owned by dead processes.
	for i := uint32(0); i < n; i++ {
		slot := respSlotPtr(s.base, s.respOff, s.respStride, uintptr(i))
		state := atomic.LoadUint32(respSlotState(slot))
		if state != layoutRespAcquired && state != layoutRespReady {
			continue
		}
		pid := atomic.LoadUint32(respSlotOwnerPid(slot))
		if pid == 0 || pidAlive(pid) {
			continue
		}
		// Clear ownerPid before the FREE CAS so any concurrent observer sees a
		// clean state (pid==0 → skip) rather than a stale dead PID.
		atomic.StoreUint32(respSlotOwnerPid(slot), 0)
		if !atomic.CompareAndSwapUint32(respSlotState(slot), state, layoutRespFree) {
			continue
		}
		atomic.AddUint32(hdrStatRecoveries(s.base), 1)
		// Try to grab the just-freed slot before another goroutine does.
		if atomic.CompareAndSwapUint32(respSlotState(slot), layoutRespFree, layoutRespAcquired) {
			atomic.StoreUint32(respSlotOwnerPid(slot), mypid)
			gen = atomic.AddUint32(respSlotGeneration(slot), 1)
			if gen == 0 {
				gen = atomic.AddUint32(respSlotGeneration(slot), 1)
			}
			return i, true, gen
		}
		// Lost the race — let other waiters know a slot became available.
		wakeSlotWaiters(s.base)
	}

	return 0, false, 0
}

// releaseRespSlot marks a slot FREE and wakes any waiters.
// NOTE: does NOT increment generation — only acquire and cancel do that.
// Port of reqrep_slot_release from reqrep.h.
func releaseRespSlot(s *shm, idx uint32) {
	slot := respSlotPtr(s.base, s.respOff, s.respStride, uintptr(idx))
	atomic.StoreUint32(respSlotOwnerPid(slot), 0)
	atomic.StoreUint32(respSlotState(slot), layoutRespFree)
	wakeSlotWaiters(s.base)
}
