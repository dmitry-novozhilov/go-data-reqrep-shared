//go:build linux

package reqrep

import (
	"sync/atomic"
	"unsafe"
)

// writeArenaLocked copies data into the request arena and returns the arena
// offset and skip value. Must be called with the arena mutex held.
//
// Port of the arena-allocation logic inside reqrep_send_locked from reqrep.h.
//
// alloc is rounded up to 8-byte alignment (minimum 8). arena_skip covers both
// the wasted tail bytes on wraparound AND the allocated bytes, so the receiver
// can subtract a single value from arena_used.
func writeArenaLocked(s *shm, data []byte) (arenaOff, arenaSkip uint32, ok bool) {
	length := uint32(len(data))
	if length > s.reqArenaCap {
		return 0, 0, false
	}
	alloc := (length + 7) &^ uint32(7)
	if alloc == 0 {
		alloc = 8
	}

	wpos := atomic.LoadUint32(hdrArenaWpos(s.base))
	skip := uint64(alloc)

	// Wraparound: data doesn't fit between wpos and end of arena.
	// Edge case: after a full 8-byte allocation ending at the last byte, wpos
	// equals reqArenaCap exactly; the condition is still true and wpos resets to 0.
	if uint64(wpos)+uint64(alloc) > uint64(s.reqArenaCap) {
		skip += uint64(s.reqArenaCap) - uint64(wpos)
		wpos = 0
	}

	arenaUsed := atomic.LoadUint32(hdrArenaUsed(s.base))

	// Arena full check.
	if uint64(arenaUsed)+skip > uint64(s.reqArenaCap) {
		// Safe to reset only when the queue is empty (all data already consumed).
		head := atomic.LoadUint64(hdrReqHead(s.base))
		tail := atomic.LoadUint64(hdrReqTail(s.base))
		if tail == head {
			atomic.StoreUint32(hdrArenaWpos(s.base), 0)
			atomic.StoreUint32(hdrArenaUsed(s.base), 0)
			wpos = 0
			skip = uint64(alloc)
		} else {
			atomic.AddUint64(hdrStatSendFull(s.base), 1)
			return 0, 0, false
		}
	}

	arena := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(s.base)+s.arenaOff)), s.reqArenaCap)
	copy(arena[wpos:wpos+length], data)

	arenaOff = wpos
	arenaSkip = uint32(skip)
	atomic.StoreUint32(hdrArenaWpos(s.base), wpos+alloc)
	atomic.AddUint32(hdrArenaUsed(s.base), uint32(skip))
	return arenaOff, arenaSkip, true
}

// readArena returns a slice that aliases the shared-memory arena directly.
// The slice is valid only while the arena mutex is held — callers must copy
// out the data before releasing the mutex (see recvLocked).
func readArena(s *shm, off, length uint32) []byte {
	arena := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(s.base)+s.arenaOff)), s.reqArenaCap)
	return arena[off : off+length]
}

// releaseArenaLocked decrements arena_used by skip and resets wpos to 0 when
// the arena becomes empty. Must be called with the arena mutex held.
//
// Resetting wpos when used==0 is safe for two reasons: (1) the mutex
// serialises all consumers, so only one recvLocked runs at a time, and it
// copies arena data before calling releaseArenaLocked; (2) arena_used
// accounts for every byte owned by a live queue slot (via arenaSkip), so
// used==0 implies all slots have been dequeued and their data already copied.
func releaseArenaLocked(s *shm, skip uint32) {
	used := atomic.LoadUint32(hdrArenaUsed(s.base))
	if used >= skip {
		used -= skip
	} else {
		used = 0
	}
	atomic.StoreUint32(hdrArenaUsed(s.base), used)
	if used == 0 {
		atomic.StoreUint32(hdrArenaWpos(s.base), 0)
	}
}
