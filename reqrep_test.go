//go:build linux

package reqrep

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// wakeRecv bumps the recv_futex and wakes all sleeping recvWait goroutines
// so they notice a cancelled context quickly instead of after the 2-second
// periodic timeout.
func wakeRecv(base unsafe.Pointer) {
	atomic.AddUint32(hdrRecvFutex(base), 1)
	futexWake(hdrRecvFutex(base), math.MaxInt32)
}

// echoSetup creates a Server+Client pair backed by a temp file, starts
// `handlers` echo goroutines, and returns the Client plus a cleanup func.
// Cleanup cancels the server context, wakes blocked recvWait goroutines,
// waits for all handler goroutines, then unmaps both ends.
func echoSetup(t *testing.T, opts Options, handlers int) (*Client, func()) {
	t.Helper()
	path := t.TempDir() + "/reqrep.shm"

	srv, err := NewServer(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := NewClient(path)
	if err != nil {
		_ = srv.Close()
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < handlers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv.Serve(ctx, func(req []byte) ([]byte, error) {
				return append([]byte(nil), req...), nil
			})
		}()
	}

	return cli, func() {
		cancel()
		wakeRecv(srv.s.base)
		wg.Wait()
		_ = cli.Close()
		_ = srv.Close()
	}
}

func TestEcho(t *testing.T) {
	cli, cleanup := echoSetup(t, Options{}, 1)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	want := []byte("hello, reqrep")
	got, err := cli.Query(ctx, want)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestEmptyPayload(t *testing.T) {
	cli, cleanup := echoSetup(t, Options{}, 1)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := cli.Query(ctx, []byte{})
	if err != nil {
		t.Fatalf("Query empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty response, got %q", got)
	}
}

// TestConcurrentQueries exercises concurrent senders against multiple server
// goroutines, covering the MPMC path and all futex wake paths.
func TestConcurrentQueries(t *testing.T) {
	cli, cleanup := echoSetup(t, Options{ReqCap: 64, RespSlots: 64}, 4)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const senders = 8
	const queriesEach = 50

	errs := make(chan string, senders*queriesEach)
	var wg sync.WaitGroup

	for g := 0; g < senders; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < queriesEach; i++ {
				want := []byte(fmt.Sprintf("g%d-q%d", g, i))
				got, err := cli.Query(ctx, want)
				if err != nil {
					errs <- fmt.Sprintf("sender %d query %d: %v", g, i, err)
					return
				}
				if !bytes.Equal(got, want) {
					errs <- fmt.Sprintf("sender %d query %d: got %q, want %q", g, i, got, want)
				}
			}
		}(g)
	}

	wg.Wait()
	close(errs)

	for msg := range errs {
		t.Error(msg)
	}
}

// TestContextTimeout verifies that Query returns a context error when no server
// is running and the deadline expires.
func TestContextTimeout(t *testing.T) {
	path := t.TempDir() + "/reqrep.shm"

	srv, err := NewServer(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	cli, err := NewClient(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	// No Serve goroutine — server will never respond.
	// 2s gives sufficient headroom on slow CI machines running under -race:
	// the spin phase runs 32 × runtime.Gosched, and each Gosched can stall
	// tens of milliseconds under the race detector.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = cli.Query(ctx, []byte("timeout-me"))
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

// TestNoMissedWakeup verifies that a sender waiting for a free response slot
// wakes up promptly after another sender's slot is released. With the old
// (buggy) wakeSlotWaiters that only bumped the futex when waiters>0, a sender
// that loaded fseq before registering in waiters would miss the bump and block
// for the full context deadline.
func TestNoMissedWakeup(t *testing.T) {
	// Single response slot forces the second sender to wait for the first.
	cli, cleanup := echoSetup(t, Options{RespSlots: 1, ReqCap: 4}, 1)
	defer cleanup()

	// 50 pairs × 2 concurrent senders maximises the chance of hitting the race.
	const pairs = 50
	for i := 0; i < pairs; i++ {
		var wg sync.WaitGroup
		var failed atomic.Bool
		for j := 0; j < 2; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// 200 ms is far more than a correct wakeup needs (~µs), but
				// tight enough to fail reliably when a wakeup is missed.
				ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				defer cancel()
				if _, err := cli.Query(ctx, []byte("x")); err != nil {
					failed.Store(true)
				}
			}()
		}
		wg.Wait()
		if failed.Load() {
			t.Fatalf("pair %d: query timed out — missed wakeup on slot release", i)
		}
	}
}

// TestNoMissedWakeupRecv targets the wakeConsumers path: a server handler
// blocked in recvWait must wake up promptly when a request is enqueued.
// With the old (buggy) wakeConsumers that skipped the futex bump when
// waiters==0, a handler that had loaded fseq but not yet incremented
// recv_waiters would miss the bump and sleep for the full 2-second periodic
// timeout. 100 rapid sequential queries maximise the chance of hitting that
// window; a missed wake shows up as a query timeout.
func TestNoMissedWakeupRecv(t *testing.T) {
	cli, cleanup := echoSetup(t, Options{}, 1)
	defer cleanup()

	const queries = 100
	for i := 0; i < queries; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, err := cli.Query(ctx, []byte("x"))
		cancel()
		if err != nil {
			t.Fatalf("query %d: %v — possible missed wakeup in wakeConsumers", i, err)
		}
	}
}

// TestNoMissedWakeupQueueFull targets the wakeProducers path: a sender blocked
// in sendWait because the request queue is full must wake up promptly when the
// handler dequeues a request. With the old wakeProducers that skipped the bump
// when waiters==0, a sender that loaded fseq on a full queue but had not yet
// registered in send_waiters would miss the bump and block until ctx expired.
// Unlike recvWait, sendWait has no periodic 2-second rescue timeout, so with
// a context.Background() the sender would block indefinitely.
func TestNoMissedWakeupQueueFull(t *testing.T) {
	// ReqCap=1 forces the second concurrent sender to hit a full queue.
	cli, cleanup := echoSetup(t, Options{ReqCap: 1, RespSlots: 4}, 1)
	defer cleanup()

	const pairs = 50
	for i := 0; i < pairs; i++ {
		var wg sync.WaitGroup
		var failed atomic.Bool
		for j := 0; j < 2; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				defer cancel()
				if _, err := cli.Query(ctx, []byte("x")); err != nil {
					failed.Store(true)
				}
			}()
		}
		wg.Wait()
		if failed.Load() {
			t.Fatalf("pair %d: sender timed out on full queue — missed wakeup in wakeProducers", i)
		}
	}
}

// TestGenWrapNonZero verifies that acquireRespSlot never returns gen==0.
// gen==0 with slotIdx==0 would produce id==0, the failure sentinel in sendWait.
func TestGenWrapNonZero(t *testing.T) {
	path := t.TempDir() + "/reqrep.shm"
	srv, err := NewServer(path, Options{RespSlots: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	s := srv.s
	slot := respSlotPtr(s.base, s.respOff, s.respStride, 0)
	// Force generation to MaxUint32 so the next AddUint32 wraps to 0.
	atomic.StoreUint32(respSlotGeneration(slot), math.MaxUint32)

	idx, ok, gen := acquireRespSlot(s)
	if !ok {
		t.Fatal("acquireRespSlot returned !ok")
	}
	if idx != 0 {
		t.Fatalf("expected slot 0, got %d", idx)
	}
	if gen == 0 {
		t.Fatal("acquireRespSlot returned gen==0: id would equal 0 for slotIdx==0")
	}
	releaseRespSlot(s, 0)
}

// TestArenaTooSmall verifies that a request larger than ReqArenaCap is rejected
// without a panic. The call must return a non-nil error (context deadline exceeded
// because writeArenaLocked returns false and sendWait waits until timeout).
// ReqArenaCap is set to the minimum (4096) so that one byte over triggers the guard.
func TestArenaTooSmall(t *testing.T) {
	const arenaCap = 4096 // minimum enforced by computeLayout; passed directly to keep it exact
	cli, cleanup := echoSetup(t, Options{ReqArenaCap: arenaCap}, 1)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// arenaCap+1 fits within layoutStrLenMask so ErrTooLong won't fire,
	// but writeArenaLocked's guard must reject it without panicking.
	_, err := cli.Query(ctx, make([]byte, arenaCap+1))
	if err == nil {
		t.Fatal("expected error for oversized request, got nil")
	}
}

func TestTooLong(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) < 8 {
		t.Skip("requires 64-bit platform: len field is int, 0x80000000 is negative on 32-bit")
	}
	cli, cleanup := echoSetup(t, Options{}, 1)
	defer cleanup()

	// Forge a slice header with len > layoutStrLenMask without allocating 2 GiB.
	// trySend checks len(data) and returns ErrTooLong before reading any bytes,
	// so the single-byte backing array is never accessed out of bounds.
	//
	// unsafe.Slice(&b, layoutStrLenMask+1) would panic under -race because
	// checkptr validates that len*sizeof(T) fits within the backing allocation.
	// Writing directly to the Len/Cap fields bypasses that check — checkptr only
	// instruments unsafe.Pointer conversions, not direct struct field writes.
	// Forge a slice header backed by a single byte but with len > layoutStrLenMask,
	// using a local struct that mirrors Go's internal slice layout. This avoids
	// reflect.SliceHeader (deprecated) and unsafe.Slice (rejected by -race checkptr
	// when len > backing allocation size). The data bytes are never read on this path.
	type sliceHeader struct {
		Data unsafe.Pointer
		Len  int
		Cap  int
	}
	var b byte
	var huge []byte
	sh := (*sliceHeader)(unsafe.Pointer(&huge))
	sh.Data = unsafe.Pointer(&b)
	sh.Len = int(layoutStrLenMask) + 1
	sh.Cap = sh.Len
	_, err := cli.Query(context.Background(), huge)
	if err != ErrTooLong {
		t.Fatalf("expected ErrTooLong, got %v", err)
	}
}

// TestHandlerError verifies that when the handler returns an error, the server
// calls cancelRequest which wakes the client with ErrStaleSlot immediately.
func TestHandlerError(t *testing.T) {
	path := t.TempDir() + "/reqrep.shm"

	srv, err := NewServer(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	cli, err := NewClient(path)
	if err != nil {
		_ = srv.Close()
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		srv.Serve(ctx, func(req []byte) ([]byte, error) {
			return nil, errors.New("handler error")
		})
	}()
	defer func() {
		cancel()
		wakeRecv(srv.s.base)
		wg.Wait()
		_ = cli.Close()
		_ = srv.Close()
	}()

	qctx, qcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer qcancel()

	_, err = cli.Query(qctx, []byte("probe"))
	if err != ErrStaleSlot {
		t.Fatalf("expected ErrStaleSlot, got %v", err)
	}
}

// TestServerReopen verifies that a second NewServer call on an existing
// (already-initialised) shm file succeeds and the client still works.
func TestServerReopen(t *testing.T) {
	path := t.TempDir() + "/reqrep.shm"

	srv1, err := NewServer(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv1.Close(); err != nil {
		t.Fatal(err)
	}

	srv2, err := NewServer(path, Options{})
	if err != nil {
		t.Fatalf("second NewServer: %v", err)
	}
	cli, err := NewClient(path)
	if err != nil {
		_ = srv2.Close()
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		srv2.Serve(ctx, func(req []byte) ([]byte, error) {
			return append([]byte(nil), req...), nil
		})
	}()
	defer func() {
		cancel()
		wakeRecv(srv2.s.base)
		wg.Wait()
		_ = cli.Close()
		_ = srv2.Close()
	}()

	qctx, qcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer qcancel()

	want := []byte("reopen-test")
	got, err := cli.Query(qctx, want)
	if err != nil {
		t.Fatalf("Query after reopen: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestPartialInitRecovery simulates a crash between ftruncate and the
// completion of initHeader: the file exists with the right size but magic==0.
// The next NewServer call must detect the partial init and re-initialise.
func TestPartialInitRecovery(t *testing.T) {
	path := t.TempDir() + "/reqrep.shm"
	opts := Options{}

	srv, err := NewServer(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash right after ftruncate: zero the entire header region,
	// not just magic. ftruncate guarantees all bytes are zero, so the recovery
	// detector (isHeaderZero) needs to see a fully-zeroed header.
	header := unsafe.Slice((*byte)(srv.s.base), layoutSizeofHeader)
	for i := range header {
		header[i] = 0
	}
	_ = srv.Close()

	// Next open must recover, not fail with "bad magic".
	srv2, err := NewServer(path, opts)
	if err != nil {
		t.Fatalf("expected recovery after partial init, got: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		srv2.Serve(ctx, func(req []byte) ([]byte, error) {
			return append([]byte(nil), req...), nil
		})
	}()
	defer func() {
		cancel()
		wakeRecv(srv2.s.base)
		wg.Wait()
		_ = srv2.Close()
	}()

	cli, err := NewClient(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	qctx, qcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer qcancel()

	want := []byte("recovery-test")
	got, err := cli.Query(qctx, want)
	if err != nil {
		t.Fatalf("query after partial-init recovery: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}
