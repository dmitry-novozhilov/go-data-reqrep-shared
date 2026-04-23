package reqrep

import "unsafe"

// ── ReqRepHeader accessors ────────────────────────────────────────────────────

func hdrMagic(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrMagic)
}
func hdrVersion(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrVersion)
}
func hdrMode(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrMode)
}
func hdrReqCap(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrReqCap)
}
func hdrTotalSize(base unsafe.Pointer) *uint64 {
	return hdrField64(base, layoutHdrTotalSize)
}
func hdrReqSlotsOff(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrReqSlotsOff)
}
func hdrReqArenaOff(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrReqArenaOff)
}
func hdrReqArenaCap(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrReqArenaCap)
}
func hdrRespSlots(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrRespSlots)
}
func hdrRespDataMax(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrRespDataMax)
}
func hdrRespOff(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrRespOff)
}
func hdrRespStride(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrRespStride)
}
func hdrReqHead(base unsafe.Pointer) *uint64 {
	return hdrField64(base, layoutHdrReqHead)
}
func hdrRecvWaiters(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrRecvWaiters)
}
func hdrRecvFutex(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrRecvFutex)
}
func hdrReqTail(base unsafe.Pointer) *uint64 {
	return hdrField64(base, layoutHdrReqTail)
}
func hdrSendWaiters(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrSendWaiters)
}
func hdrSendFutex(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrSendFutex)
}
func hdrMutex(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrMutex)
}
func hdrMutexWaiters(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrMutexWaiters)
}
func hdrArenaWpos(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrArenaWpos)
}
func hdrArenaUsed(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrArenaUsed)
}
func hdrRespHint(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrRespHint)
}
func hdrStatRecoveries(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrStatRecoveries)
}
func hdrSlotFutex(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrSlotFutex)
}
func hdrSlotWaiters(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrSlotWaiters)
}
func hdrStatRequests(base unsafe.Pointer) *uint64 {
	return hdrField64(base, layoutHdrStatRequests)
}
func hdrStatReplies(base unsafe.Pointer) *uint64 {
	return hdrField64(base, layoutHdrStatReplies)
}
func hdrStatSendFull(base unsafe.Pointer) *uint64 {
	return hdrField64(base, layoutHdrStatSendFull)
}
func hdrStatRecvEmpty(base unsafe.Pointer) *uint32 {
	return hdrField32(base, layoutHdrStatRecvEmpty)
}

// ── ReqSlot accessors ─────────────────────────────────────────────────────────

// reqSlotPtr returns a pointer to ReqSlot at index i within the slots array.
func reqSlotPtr(base unsafe.Pointer, slotsOff, i uintptr) unsafe.Pointer {
	return unsafe.Pointer(uintptr(base) + slotsOff + i*layoutSizeofReqSlot)
}

func reqSlotArenaOff(s unsafe.Pointer) *uint32 {
	return (*uint32)(unsafe.Pointer(uintptr(s) + layoutReqArenaOff))
}
func reqSlotPackedLen(s unsafe.Pointer) *uint32 {
	return (*uint32)(unsafe.Pointer(uintptr(s) + layoutReqPackedLen))
}
func reqSlotArenaSkip(s unsafe.Pointer) *uint32 {
	return (*uint32)(unsafe.Pointer(uintptr(s) + layoutReqArenaSkip))
}
func reqSlotRespSlot(s unsafe.Pointer) *uint32 {
	return (*uint32)(unsafe.Pointer(uintptr(s) + layoutReqRespSlot))
}
func reqSlotRespGen(s unsafe.Pointer) *uint32 {
	return (*uint32)(unsafe.Pointer(uintptr(s) + layoutReqRespGen))
}

// ── RespSlotHeader accessors ──────────────────────────────────────────────────

// respSlotPtr returns a pointer to RespSlotHeader at index i.
func respSlotPtr(base unsafe.Pointer, respOff, stride, i uintptr) unsafe.Pointer {
	return unsafe.Pointer(uintptr(base) + respOff + i*stride)
}

func respSlotState(s unsafe.Pointer) *uint32 {
	return (*uint32)(unsafe.Pointer(uintptr(s) + layoutRespState))
}
func respSlotWaiters(s unsafe.Pointer) *uint32 {
	return (*uint32)(unsafe.Pointer(uintptr(s) + layoutRespWaiters))
}
func respSlotOwnerPid(s unsafe.Pointer) *uint32 {
	return (*uint32)(unsafe.Pointer(uintptr(s) + layoutRespOwnerPid))
}
func respSlotLen(s unsafe.Pointer) *uint32 {
	return (*uint32)(unsafe.Pointer(uintptr(s) + layoutRespLen))
}
func respSlotFlags(s unsafe.Pointer) *uint32 {
	return (*uint32)(unsafe.Pointer(uintptr(s) + layoutRespFlags))
}
func respSlotGeneration(s unsafe.Pointer) *uint32 {
	return (*uint32)(unsafe.Pointer(uintptr(s) + layoutRespGeneration))
}

// respSlotData returns a pointer to the response data area (immediately after
// the RespSlotHeader, at offset RespDataStart within the slot).
func respSlotData(s unsafe.Pointer) unsafe.Pointer {
	return unsafe.Pointer(uintptr(s) + layoutRespDataStart)
}
