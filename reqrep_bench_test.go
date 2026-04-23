//go:build linux

package reqrep

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// benchEchoSetup is like echoSetup but exposes the server for bench cleanup.
func benchEchoSetup(b *testing.B, opts Options, handlers int) (*Client, func()) {
	b.Helper()
	path := b.TempDir() + "/reqrep.shm"

	srv, err := NewServer(path, opts)
	if err != nil {
		b.Fatal(err)
	}
	cli, err := NewClient(path)
	if err != nil {
		_ = srv.Close()
		b.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < handlers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv.Serve(ctx, func(req []byte) ([]byte, error) {
				return req, nil
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

func benchQuery(b *testing.B, payloadSize int, senders int) {
	b.Helper()
	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = byte(i & 0xFF)
	}

	opts := Options{
		ReqCap:      256,
		RespSlots:   256,
		RespDataMax: uint32(payloadSize + 64),
	}
	cli, cleanup := benchEchoSetup(b, opts, senders)
	defer cleanup()

	ctx := context.Background()
	b.SetBytes(int64(payloadSize))
	b.ResetTimer()

	if senders == 1 {
		for i := 0; i < b.N; i++ {
			if _, err := cli.Query(ctx, payload); err != nil {
				b.Fatal(err)
			}
		}
		return
	}

	// Parallel senders: spread b.N queries across senders goroutines.
	b.SetParallelism(senders)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := cli.Query(ctx, payload); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkQuery1KB_1(b *testing.B)  { benchQuery(b, 1024, 1) }
func BenchmarkQuery1KB_4(b *testing.B)  { benchQuery(b, 1024, 4) }
func BenchmarkQuery1KB_8(b *testing.B)  { benchQuery(b, 1024, 8) }
func BenchmarkQuery64KB_1(b *testing.B) { benchQuery(b, 64*1024, 1) }
func BenchmarkQuery64KB_4(b *testing.B) { benchQuery(b, 64*1024, 4) }

// BenchmarkQueryThroughput measures aggregate throughput across N parallel
// clients all sending to the same server.
func BenchmarkQueryThroughput(b *testing.B) {
	sizes := []int{256, 4096, 65536}
	parallelism := []int{1, 4, 8}

	for _, sz := range sizes {
		for _, par := range parallelism {
			b.Run(fmt.Sprintf("sz=%d/par=%d", sz, par), func(b *testing.B) {
				benchQuery(b, sz, par)
			})
		}
	}
}

// BenchmarkQueryLatency measures single-sender round-trip latency for small
// payloads — a proxy for the protocol overhead.
func BenchmarkQueryLatency(b *testing.B) {
	cli, cleanup := benchEchoSetup(b, Options{}, 1)
	defer cleanup()

	payload := []byte("ping")
	ctx := context.Background()
	b.ResetTimer()

	start := time.Now()
	for i := 0; i < b.N; i++ {
		if _, err := cli.Query(ctx, payload); err != nil {
			b.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	if b.N > 0 {
		b.ReportMetric(float64(elapsed.Nanoseconds())/float64(b.N), "ns/rtt")
	}
}
