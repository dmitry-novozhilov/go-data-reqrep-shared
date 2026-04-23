//go:build linux

//go:generate go run gen/gen.go

package reqrep

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ── Public errors ─────────────────────────────────────────────────────────────

var (
	ErrTooLong    = errors.New("reqrep: request data exceeds maximum length")
	ErrRespTooLong = errors.New("reqrep: response data exceeds slot capacity")
	ErrStaleSlot  = errors.New("reqrep: response slot was cancelled or reused")
)

// ── Options ───────────────────────────────────────────────────────────────────

type Options struct {
	ReqCap      uint32 // request queue capacity (power of 2), default 256
	ReqArenaCap uint32 // request arena bytes; default max(ReqCap*256, 4096)
	RespSlots   uint32 // number of response slots, default ReqCap
	RespDataMax uint32 // max response bytes per slot, default 4096
	SpinCount   int    // spin iterations before futex; default layoutSpinLimit (32)
}

// ── Server ────────────────────────────────────────────────────────────────────

type Server struct {
	s      *shm
	data   []byte
	closed atomic.Bool
}

// NewServer creates or opens the mmap file at path and returns a Server.
// If the file is empty it is initialised with opts; otherwise opts is ignored
// and the existing layout is used (must have compatible magic/version/mode).
//
// File deletion: if the file is removed (e.g. rm) while a Server or Client has
// it mapped, the mapping stays valid — Linux keeps the inode alive until all
// mappings are released. New NewServer/NewClient calls on the same path will
// fail with ENOENT. Neither side detects the deletion automatically.
func NewServer(path string, opts Options) (*Server, error) {
	data, err := openOrCreate(path, opts)
	if err != nil {
		return nil, err
	}
	return &Server{s: setupShm(data), data: data}, nil
}

// Serve blocks, calling handler for each incoming request.
// Multiple goroutines may call Serve simultaneously.
// Each request is dispatched in its own goroutine; callers that need to bound
// concurrency should gate handler invocations with a semaphore.
func (s *Server) Serve(ctx context.Context, handler func([]byte) ([]byte, error)) error {
	for {
		data, id, err := recvWait(ctx, s.s)
		if err != nil {
			return err
		}
		go func(req []byte, id uint64) {
			// req is a fresh copy allocated by recvLocked; the handler may retain it.
			defer func() {
				if r := recover(); r != nil {
					cancelRequest(s.s, id)
					panic(r) // re-raise: handler panics are application bugs, not library errors
				}
			}()
			resp, herr := handler(req)
			if herr != nil {
				cancelRequest(s.s, id)
				return
			}
			writeResponse(s.s, id, resp) //nolint:errcheck — best-effort
		}(data, id)
	}
}

func (s *Server) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	return unix.Munmap(s.data)
}

// ── Client ────────────────────────────────────────────────────────────────────

type Client struct {
	s      *shm
	data   []byte
	closed atomic.Bool
}

// NewClient opens an existing mmap file created by a Server (Go or Perl).
func NewClient(path string) (*Client, error) {
	data, err := openExisting(path)
	if err != nil {
		return nil, err
	}
	return &Client{s: setupShm(data), data: data}, nil
}

// Query sends req and waits for a response. Safe for concurrent use.
func (c *Client) Query(ctx context.Context, req []byte) ([]byte, error) {
	id, err := sendWait(ctx, c.s, req)
	if err != nil {
		return nil, err
	}
	resp, err := waitResponse(ctx, c.s, id)
	if err != nil {
		// Cancel the slot (CAS ACQUIRED→FREE, then bumps generation) and drain any
		// response that arrived between the timeout and the cancel. The generation
		// bump ensures tryDrainResponse cannot release a slot that cancelRequest
		// already freed and another sender re-acquired.
		cancelRequest(c.s, id)
		tryDrainResponse(c.s, id)
		return nil, err
	}
	return resp, nil
}

func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	return unix.Munmap(c.data)
}

// ── shm ───────────────────────────────────────────────────────────────────────

// shm holds immutable cached layout values derived from the mmap header.
// All mutable header state is accessed through base via atomic ops.
type shm struct {
	base        unsafe.Pointer
	mmapSize    uintptr
	reqCap      uint32
	reqCapMask  uint32
	reqArenaCap uint32
	respSlots   uint32
	respDataMax uint32
	respStride  uintptr
	slotsOff    uintptr
	arenaOff    uintptr
	respOff     uintptr
}

// setupShm builds the immutable shm descriptor from a validated mmap region.
// Precondition: data must reference a fully-initialised header (written by
// initHeader or confirmed valid by validateHeader). All header fields are read
// once here and cached in shm; later accesses go through the cached fields or
// via atomic ops on base.
func setupShm(data []byte) *shm {
	base := unsafe.Pointer(&data[0])
	return &shm{
		base:        base,
		mmapSize:    uintptr(len(data)),
		reqCap:      *hdrReqCap(base),
		reqCapMask:  *hdrReqCap(base) - 1,
		reqArenaCap: *hdrReqArenaCap(base),
		respSlots:   *hdrRespSlots(base),
		respDataMax: *hdrRespDataMax(base),
		respStride:  uintptr(*hdrRespStride(base)),
		slotsOff:    uintptr(*hdrReqSlotsOff(base)),
		arenaOff:    uintptr(*hdrReqArenaOff(base)),
		respOff:     uintptr(*hdrRespOff(base)),
	}
}

// ── mmap file management ──────────────────────────────────────────────────────

type mmapLayout struct {
	reqSlotsOff uint32
	reqArenaOff uint32
	reqArenaCap uint32
	respStride  uint32
	respOff     uint32
	totalSize   uint64
}

func computeLayout(reqCap, respSlots, respDataMax uint32, arenaHint uint64) (mmapLayout, error) {
	if reqCap == 0 || reqCap&(reqCap-1) != 0 {
		return mmapLayout{}, fmt.Errorf("reqrep: req_cap %d is not a power of 2", reqCap)
	}
	reqSlotsOff := uint32(layoutSizeofHeader)
	slotsEnd := uint64(reqSlotsOff) + uint64(reqCap)*uint64(layoutSizeofReqSlot)
	reqArenaOff64 := (slotsEnd + 7) &^ uint64(7)
	if reqArenaOff64 > math.MaxUint32 {
		return mmapLayout{}, errors.New("reqrep: layout overflow in req_arena_off")
	}
	reqArenaOff := uint32(reqArenaOff64)

	reqArenaCap := uint32(arenaHint)
	if reqArenaCap < 4096 {
		reqArenaCap = 4096
	}

	respStride := (uint32(layoutSizeofRespSlot) + respDataMax + 63) &^ uint32(63)
	respOff64 := (uint64(reqArenaOff) + uint64(reqArenaCap) + 63) &^ uint64(63)
	if respOff64 > math.MaxUint32 {
		return mmapLayout{}, errors.New("reqrep: layout overflow in resp_off")
	}

	totalSize := respOff64 + uint64(respSlots)*uint64(respStride)
	if totalSize > uint64(math.MaxInt) {
		return mmapLayout{}, errors.New("reqrep: layout total size overflows int")
	}
	return mmapLayout{
		reqSlotsOff: reqSlotsOff,
		reqArenaOff: reqArenaOff,
		reqArenaCap: reqArenaCap,
		respStride:  respStride,
		respOff:     uint32(respOff64),
		totalSize:   totalSize,
	}, nil
}

func initHeader(base unsafe.Pointer, reqCap, respSlots, respDataMax uint32, l mmapLayout) {
	// Write all structural fields before magic. If the process crashes mid-init,
	// magic stays 0 (ftruncate zero-fills), which openOrCreate detects and
	// recovers from by re-running initHeader on the next open.
	*hdrVersion(base) = layoutVersion
	*hdrMode(base) = layoutModeStr
	*hdrReqCap(base) = reqCap
	*hdrTotalSize(base) = l.totalSize
	*hdrReqSlotsOff(base) = l.reqSlotsOff
	*hdrReqArenaOff(base) = l.reqArenaOff
	*hdrReqArenaCap(base) = l.reqArenaCap
	*hdrRespSlots(base) = respSlots
	*hdrRespDataMax(base) = respDataMax
	*hdrRespOff(base) = l.respOff
	*hdrRespStride(base) = l.respStride
	// All other fields (head, tail, arena, stats) stay at zero — POSIX guarantees
	// that ftruncate zero-fills new pages, so only the non-zero structural fields
	// above need explicit writes.
	// Use a store with release semantics so all preceding structural writes are
	// visible to any reader that observes magic != 0.
	atomic.StoreUint32(hdrMagic(base), layoutMagic)
}

func validateHeader(base unsafe.Pointer, fileSize uint64) error {
	if *hdrMagic(base) != layoutMagic {
		return fmt.Errorf("reqrep: bad magic 0x%08X", *hdrMagic(base))
	}
	if *hdrVersion(base) != layoutVersion {
		return fmt.Errorf("reqrep: unsupported version %d", *hdrVersion(base))
	}
	if *hdrMode(base) != layoutModeStr {
		return fmt.Errorf("reqrep: expected string mode (0), got %d", *hdrMode(base))
	}
	reqCap := *hdrReqCap(base)
	if reqCap == 0 || reqCap&(reqCap-1) != 0 {
		return fmt.Errorf("reqrep: req_cap %d is not a power of 2", reqCap)
	}
	if *hdrTotalSize(base) != fileSize {
		return fmt.Errorf("reqrep: total_size %d != file size %d", *hdrTotalSize(base), fileSize)
	}
	if *hdrReqSlotsOff(base) != layoutSizeofHeader {
		return errors.New("reqrep: unexpected req_slots_off")
	}
	if *hdrRespSlots(base) == 0 {
		return errors.New("reqrep: resp_slots == 0")
	}
	if *hdrRespStride(base) < layoutSizeofRespSlot {
		return errors.New("reqrep: resp_stride too small")
	}
	return nil
}

func openOrCreate(path string, opts Options) ([]byte, error) {
	if opts.ReqCap == 0 {
		opts.ReqCap = 256
	}
	if opts.RespDataMax == 0 {
		opts.RespDataMax = 4096
	}
	// Guard against overflow in the respStride computation inside computeLayout:
	//   respStride = (layoutSizeofRespSlot + respDataMax + 63) &^ 63
	const maxRespDataMax = math.MaxUint32 - uint32(layoutSizeofRespSlot) - 63
	if opts.RespDataMax > maxRespDataMax {
		return nil, fmt.Errorf("reqrep: RespDataMax %d exceeds maximum (%d)", opts.RespDataMax, maxRespDataMax)
	}
	reqCap := nextPow2(opts.ReqCap)
	if opts.RespSlots == 0 {
		opts.RespSlots = reqCap
	}
	arenaHint := uint64(opts.ReqArenaCap)
	if arenaHint == 0 {
		arenaHint = uint64(reqCap) * 256
	}

	l, err := computeLayout(reqCap, opts.RespSlots, opts.RespDataMax, arenaHint)
	if err != nil {
		return nil, err
	}

	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT, 0666)
	if err != nil {
		return nil, fmt.Errorf("reqrep: open %s: %w", path, err)
	}
	// Closing fd after Mmap is safe: the kernel holds its own reference to the
	// mapped region until Munmap is called.
	defer unix.Close(fd)

	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return nil, fmt.Errorf("reqrep: flock %s: %w", path, err)
	}
	defer func() { _ = unix.Flock(fd, unix.LOCK_UN) }()

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("reqrep: fstat %s: %w", path, err)
	}

	isNew := st.Size == 0

	if !isNew && uint64(st.Size) < layoutSizeofHeader {
		return nil, fmt.Errorf("reqrep: %s: file too small (%d bytes)", path, st.Size)
	}

	mapSize := l.totalSize
	if !isNew {
		mapSize = uint64(st.Size)
	} else {
		if err := unix.Ftruncate(fd, int64(l.totalSize)); err != nil {
			return nil, fmt.Errorf("reqrep: ftruncate %s: %w", path, err)
		}
	}

	if mapSize > uint64(math.MaxInt) {
		return nil, fmt.Errorf("reqrep: %s: file size %d overflows int", path, mapSize)
	}
	data, err := unix.Mmap(fd, 0, int(mapSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("reqrep: mmap %s: %w", path, err)
	}

	base := unsafe.Pointer(&data[0])
	if isNew {
		initHeader(base, reqCap, opts.RespSlots, opts.RespDataMax, l)
	} else {
		// A crash during a previous isNew init leaves the file ftruncated (size>0)
		// but with all bytes zero (ftruncate guarantees this). Detect this case by
		// checking that the entire header region is zero AND the file size matches
		// our layout — checking only magic would false-positive on any foreign
		// zero-filled file of the same size.
		if isHeaderZero(base) && uint64(st.Size) == l.totalSize {
			initHeader(base, reqCap, opts.RespSlots, opts.RespDataMax, l)
		} else if err := validateHeader(base, uint64(st.Size)); err != nil {
			unix.Munmap(data)
			return nil, fmt.Errorf("reqrep: %s: %w", path, err)
		}
	}
	return data, nil
}

func openExisting(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("reqrep: open %s: %w", path, err)
	}
	defer unix.Close(fd)

	// LOCK_SH: multiple clients may open the same file simultaneously.
	if err := unix.Flock(fd, unix.LOCK_SH); err != nil {
		return nil, fmt.Errorf("reqrep: flock %s: %w", path, err)
	}
	defer func() { _ = unix.Flock(fd, unix.LOCK_UN) }()

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("reqrep: fstat %s: %w", path, err)
	}
	if st.Size < 0 {
		return nil, fmt.Errorf("reqrep: %s: negative file size %d", path, st.Size)
	}
	if uint64(st.Size) < layoutSizeofHeader {
		return nil, fmt.Errorf("reqrep: %s: file too small or not initialised", path)
	}
	if uint64(st.Size) > uint64(math.MaxInt) {
		return nil, fmt.Errorf("reqrep: %s: file size %d overflows int", path, st.Size)
	}
	data, err := unix.Mmap(fd, 0, int(st.Size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("reqrep: mmap %s: %w", path, err)
	}

	if err := validateHeader(unsafe.Pointer(&data[0]), uint64(st.Size)); err != nil {
		unix.Munmap(data)
		return nil, fmt.Errorf("reqrep: %s: %w", path, err)
	}
	return data, nil
}

// ── Response slot operations ──────────────────────────────────────────────────

// writeResponse copies response data into the slot and CAS ACQUIRED→READY.
// Port of reqrep_reply from reqrep.h.
func writeResponse(s *shm, id uint64, response []byte) error {
	slotIdx := uint32(id)
	expectedGen := uint32(id >> 32)
	if slotIdx >= s.respSlots {
		return ErrStaleSlot
	}
	if len(response) > int(s.respDataMax) {
		return ErrRespTooLong
	}

	slot := respSlotPtr(s.base, s.respOff, s.respStride, uintptr(slotIdx))

	if atomic.LoadUint32(respSlotState(slot)) != layoutRespAcquired {
		return ErrStaleSlot
	}
	if atomic.LoadUint32(respSlotGeneration(slot)) != expectedGen {
		return ErrStaleSlot
	}

	if len(response) > 0 {
		dataArea := unsafe.Slice((*byte)(respSlotData(slot)), s.respDataMax)
		copy(dataArea, response)
	}
	*respSlotLen(slot) = uint32(len(response))
	// resp_flags bit 0 = UTF-8 (protocol field read by C/Perl clients via
	// reqrep_get_wait). Go always writes 0: []byte is not UTF-8-tagged.
	*respSlotFlags(slot) = 0

	if !atomic.CompareAndSwapUint32(respSlotState(slot), layoutRespAcquired, layoutRespReady) {
		return ErrStaleSlot
	}

	if atomic.LoadUint32(respSlotWaiters(slot)) > 0 {
		futexWake(respSlotState(slot), 1)
	}
	atomic.AddUint64(hdrStatReplies(s.base), 1)
	return nil
}

// waitResponse spins then blocks on slot.state until READY or ctx is done.
// Port of reqrep_get_wait from reqrep.h.
func waitResponse(ctx context.Context, s *shm, id uint64) ([]byte, error) {
	slotIdx := uint32(id)
	expectedGen := uint32(id >> 32)
	slot := respSlotPtr(s.base, s.respOff, s.respStride, uintptr(slotIdx))

	// Spin phase.
	for i := 0; i < layoutSpinLimit; i++ {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		if atomic.LoadUint32(respSlotGeneration(slot)) != expectedGen {
			return nil, ErrStaleSlot
		}
		if atomic.LoadUint32(respSlotState(slot)) == layoutRespReady {
			return readAndRelease(s, slot, slotIdx)
		}
		spinPause()
	}

	// Futex phase — register as waiter once for the entire futex phase.
	// Staying registered means writeResponse always sees waiters > 0 and calls
	// futexWake; if state becomes READY before futexWait, the futex value has
	// changed and futexWait returns EAGAIN immediately, so no wakeup is missed.
	// defer guarantees the decrement on every return path.
	atomic.AddUint32(respSlotWaiters(slot), 1)
	defer atomic.AddUint32(respSlotWaiters(slot), ^uint32(0))
	for {
		state := atomic.LoadUint32(respSlotState(slot))
		if atomic.LoadUint32(respSlotGeneration(slot)) != expectedGen {
			return nil, ErrStaleSlot
		}
		if state == layoutRespReady {
			return readAndRelease(s, slot, slotIdx)
		}
		if e := ctx.Err(); e != nil {
			return nil, e
		}

		ts := ctxTimespec(ctx)
		if ts != nil && ts.Sec == 0 && ts.Nsec == 0 {
			// Deadline passed; ctx.Err() may not be set yet if the timer goroutine
			// hasn't fired, so fall back to DeadlineExceeded.
			if e := ctx.Err(); e != nil {
				return nil, e
			}
			return nil, context.DeadlineExceeded
		}

		// Wait on slot.state — NOT on hdr.slot_futex.
		futexWait(respSlotState(slot), state, ts)

		if e := ctx.Err(); e != nil {
			return nil, e
		}
	}
}

func readAndRelease(s *shm, slot unsafe.Pointer, slotIdx uint32) ([]byte, error) {
	length := atomic.LoadUint32(respSlotLen(slot))
	if length > s.respDataMax {
		releaseRespSlot(s, slotIdx)
		return nil, ErrRespTooLong
	}
	dataArea := unsafe.Slice((*byte)(respSlotData(slot)), s.respDataMax)
	result := make([]byte, length)
	copy(result, dataArea[:length])
	releaseRespSlot(s, slotIdx)
	return result, nil
}

// cancelRequest cancels a pending request. Port of reqrep_cancel from reqrep.h.
func cancelRequest(s *shm, id uint64) {
	slotIdx := uint32(id)
	expectedGen := uint32(id >> 32)
	if slotIdx >= s.respSlots {
		return
	}
	slot := respSlotPtr(s.base, s.respOff, s.respStride, uintptr(slotIdx))

	if atomic.LoadUint32(respSlotGeneration(slot)) != expectedGen {
		return
	}
	if !atomic.CompareAndSwapUint32(respSlotState(slot), layoutRespAcquired, layoutRespFree) {
		return
	}
	atomic.StoreUint32(respSlotOwnerPid(slot), 0)
	atomic.AddUint32(respSlotGeneration(slot), 1)

	// Wake ALL waiters on slot.state — they will see the generation mismatch.
	if atomic.LoadUint32(respSlotWaiters(slot)) > 0 {
		futexWake(respSlotState(slot), math.MaxInt32)
	}
	wakeSlotWaiters(s.base)
}

// tryDrainResponse frees a READY slot after a failed waitResponse+cancel.
// Handles the race where the server wrote READY between our timeout and cancel.
func tryDrainResponse(s *shm, id uint64) {
	slotIdx := uint32(id)
	expectedGen := uint32(id >> 32)
	if slotIdx >= s.respSlots {
		return
	}
	slot := respSlotPtr(s.base, s.respOff, s.respStride, uintptr(slotIdx))
	if atomic.LoadUint32(respSlotGeneration(slot)) != expectedGen {
		return
	}
	if atomic.LoadUint32(respSlotState(slot)) != layoutRespReady {
		return
	}
	releaseRespSlot(s, slotIdx)
}

// ── Wake helpers ──────────────────────────────────────────────────────────────

// All three wake helpers follow the same pattern:
//
//  1. Bump the futex counter unconditionally. This closes the classic
//     lost-wakeup race: a waiter that loaded fseq but has not yet entered
//     futexWait will see the new value and get EAGAIN instead of blocking.
//     (The race window is between the waiter's Load(fseq) and its futexWait
//     call, not between the waiter's waiters++ and futexWait.)
//
//  2. Call futexWake(1) — not math.MaxInt32 — because each call corresponds to
//     exactly one unit of available work: one enqueued request, one freed queue
//     slot, or one freed response slot. Waking more goroutines than there is
//     work causes thundering herd with no benefit.
//     This is safe because senders waiting for "no resp slots" and senders
//     waiting for "queue full" sleep on different futex addresses (slot_futex
//     vs send_futex), so futexWake(1) on each address wakes the right group.
//
//  3. Skip the futexWake syscall when *_waiters == 0 to save the kernel
//     round-trip. The bump in step 1 is sufficient for the not-yet-sleeping
//     waiter; the syscall is only needed for the already-sleeping waiter.

func wakeConsumers(base unsafe.Pointer) {
	atomic.AddUint32(hdrRecvFutex(base), 1)
	if atomic.LoadUint32(hdrRecvWaiters(base)) > 0 {
		futexWake(hdrRecvFutex(base), 1)
	}
}

func wakeProducers(base unsafe.Pointer) {
	atomic.AddUint32(hdrSendFutex(base), 1)
	if atomic.LoadUint32(hdrSendWaiters(base)) > 0 {
		futexWake(hdrSendFutex(base), 1)
	}
}

func wakeSlotWaiters(base unsafe.Pointer) {
	atomic.AddUint32(hdrSlotFutex(base), 1)
	if atomic.LoadUint32(hdrSlotWaiters(base)) > 0 {
		futexWake(hdrSlotFutex(base), 1)
	}
}

// ── Utility ───────────────────────────────────────────────────────────────────

// isHeaderZero reports whether every byte in the header region is zero.
// Used to detect a file left by a crash between ftruncate and the first
// non-zero write in initHeader.
func isHeaderZero(base unsafe.Pointer) bool {
	b := unsafe.Slice((*byte)(base), layoutSizeofHeader)
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// nextPow2 returns the smallest power of 2 >= v, with a minimum of 2.
// The minimum of 2 ensures reqCapMask = reqCap-1 is never 0 (which would
// collapse the ring-buffer index to slot 0 on every enqueue).
func nextPow2(v uint32) uint32 {
	if v < 2 {
		return 2
	}
	v--
	v |= v >> 1
	v |= v >> 2
	v |= v >> 4
	v |= v >> 8
	v |= v >> 16
	return v + 1
}

