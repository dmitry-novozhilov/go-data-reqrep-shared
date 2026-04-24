//go:build linux && interop

package reqrep

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// perlLib is the PERL5LIB path set by TestMain after building the Perl library.
var perlLib string

// TestMain builds the Perl library into a temporary directory before running
// interop tests and removes it afterwards.
func TestMain(m *testing.M) {
	srcDir := os.Getenv("PERL_REQREP_SRC")
	if srcDir == "" {
		home, _ := os.UserHomeDir()
		srcDir = filepath.Join(home, "perl5-data-reqrep-shared")
	}

	tmpdir, err := os.MkdirTemp("", "perl-reqrep-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "interop: MkdirTemp:", err)
		os.Exit(1)
	}

	if err := buildPerlLib(srcDir, tmpdir); err != nil {
		fmt.Fprintln(os.Stderr, "interop: build Perl library:", err)
		os.RemoveAll(tmpdir)
		os.Exit(1)
	}

	perlLib = filepath.Join(tmpdir, "lib", "perl5")
	code := m.Run()
	os.RemoveAll(tmpdir)
	os.RemoveAll("local")
	os.Exit(code)
}

func buildPerlLib(srcDir, installDir string) error {
	cmd := exec.Command("cpanm", "--local-lib", installDir, srcDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cpanm %s: %w\n%s", srcDir, err, out)
	}
	return nil
}

func perlExec(ctx context.Context, script string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "perl", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), "PERL5LIB="+perlLib)
	return cmd
}

// startPerlServer starts testdata/perl_server.pl with the given args, waits for
// it to print "ready", and returns the running Cmd. The caller must defer
// srv.Wait() and ensure the context is cancelled to shut the server down.
func startPerlServer(t *testing.T, ctx context.Context, args ...string) *exec.Cmd {
	t.Helper()
	srv := perlExec(ctx, "testdata/perl_server.pl", args...)
	stdout, err := srv.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	srv.Stderr = os.Stderr
	if err := srv.Start(); err != nil {
		t.Fatalf("start perl server: %v", err)
	}
	ready := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if sc.Text() == "ready" {
				close(ready)
				return
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("perl server did not signal ready in time")
	}
	return srv
}

// TestPerlServerGoClient runs the Perl echo server and queries it from Go.
func TestPerlServerGoClient(t *testing.T) {
	path := t.TempDir() + "/reqrep.shm"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	srv := perlExec(ctx, "testdata/perl_server.pl", path)
	stdout, err := srv.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	srv.Stderr = os.Stderr
	if err := srv.Start(); err != nil {
		t.Fatalf("start perl server: %v", err)
	}
	defer func() { _ = srv.Wait() }()

	// Wait for "ready\n" before opening the client.
	ready := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if sc.Text() == "ready" {
				close(ready)
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("perl server did not signal ready in time")
	}

	cli, err := NewClient(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	msgs := []string{"hello", "world", "one two three", ""}
	for _, msg := range msgs {
		got, err := cli.Query(ctx, []byte(msg))
		if err != nil {
			t.Fatalf("Query(%q): %v", msg, err)
		}
		if !bytes.Equal(got, []byte(msg)) {
			t.Fatalf("Query(%q): got %q", msg, got)
		}
	}
}

// TestGoServerPerlClient runs the Go echo server and queries it from a Perl client.
func TestGoServerPerlClient(t *testing.T) {
	path := t.TempDir() + "/reqrep.shm"

	srv, err := NewServer(path, Options{ReqCap: 256, RespSlots: 64, RespDataMax: 4096})
	if err != nil {
		t.Fatal(err)
	}

	srvCtx, srvCancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		srv.Serve(srvCtx, func(req []byte) ([]byte, error) {
			return append([]byte(nil), req...), nil
		})
	}()
	defer func() {
		srvCancel()
		wakeRecv(srv.s.base)
		wg.Wait()
		srv.Close()
	}()

	msgs := []string{"ping", "hello from perl", "goodbye"}

	cmdCtx, cmdCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cmdCancel()

	cmd := perlExec(cmdCtx, "testdata/perl_client.pl", append([]string{path}, msgs...)...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("perl client: %v", err)
	}

	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != len(msgs) {
		t.Fatalf("expected %d response lines, got %d:\n%s", len(msgs), len(lines), out)
	}
	for i, want := range msgs {
		if lines[i] != want {
			t.Fatalf("line %d: got %q, want %q", i, lines[i], want)
		}
	}
}

// TestPerlServerGoClientVariousSizes sends messages of many sizes — from empty
// to near-maximum — through the Perl echo server, exercising arena wraparound
// and resp-slot boundary conditions.
func TestPerlServerGoClientVariousSizes(t *testing.T) {
	const (
		respDataMax = 65536
		arena       = 131072
	)
	sizes := []int{0, 1, 255, 256, 1000, 4095, 4096, 4097, 16384, 65535}

	path := t.TempDir() + "/reqrep.shm"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	srv := startPerlServer(t, ctx, path,
		"256", "64",
		fmt.Sprintf("%d", respDataMax),
		fmt.Sprintf("%d", arena),
	)
	defer func() { _ = srv.Wait() }()

	cli, err := NewClient(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	for _, n := range sizes {
		payload := bytes.Repeat([]byte("x"), n)
		got, err := cli.Query(ctx, payload)
		if err != nil {
			t.Fatalf("size %d: %v", n, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("size %d: echo mismatch (got %d bytes)", n, len(got))
		}
	}
}

// TestPerlServerGoClientConcurrent fires many goroutines at the Perl echo server
// simultaneously, verifying concurrent use of a single *Client.
func TestPerlServerGoClientConcurrent(t *testing.T) {
	const (
		goroutines = 16
		msgsEach   = 50
	)

	path := t.TempDir() + "/reqrep.shm"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// req_cap=1024 resp_slots=256: easily fits 16 concurrent in-flight requests.
	srv := startPerlServer(t, ctx, path, "1024", "256", "256")
	defer func() { _ = srv.Wait() }()

	cli, err := NewClient(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	var wg sync.WaitGroup
	errc := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < msgsEach; i++ {
				msg := fmt.Sprintf("g%d-m%d", id, i)
				got, err := cli.Query(ctx, []byte(msg))
				if err != nil {
					errc <- fmt.Errorf("goroutine %d msg %d: %w", id, i, err)
					return
				}
				if string(got) != msg {
					errc <- fmt.Errorf("goroutine %d msg %d: got %q", id, i, got)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
}

// TestGoServerMultiplePerlClients runs several Perl client processes in parallel
// against a Go server with multiple worker goroutines, sending large and varied
// payloads to stress both sides simultaneously.
func TestGoServerMultiplePerlClients(t *testing.T) {
	const (
		workers     = 4
		procs       = 4
		respDataMax = 65536
		arenaCap    = 524288
	)
	sizes := []string{"0", "1", "255", "1000", "16384", "65535"}

	path := t.TempDir() + "/reqrep.shm"
	srv, err := NewServer(path, Options{
		ReqCap:      256,
		RespSlots:   128,
		RespDataMax: respDataMax,
		ReqArenaCap: arenaCap,
	})
	if err != nil {
		t.Fatal(err)
	}

	srvCtx, srvCancel := context.WithCancel(context.Background())
	var srvWg sync.WaitGroup
	for i := 0; i < workers; i++ {
		srvWg.Add(1)
		go func() {
			defer srvWg.Done()
			srv.Serve(srvCtx, func(req []byte) ([]byte, error) {
				return append([]byte(nil), req...), nil
			})
		}()
	}
	defer func() {
		srvCancel()
		wakeRecv(srv.s.base)
		srvWg.Wait()
		srv.Close()
	}()

	cmdCtx, cmdCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cmdCancel()

	var wg sync.WaitGroup
	errc := make(chan error, procs)
	for p := 0; p < procs; p++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			cmd := perlExec(cmdCtx, "testdata/perl_client_stress.pl", append([]string{path}, sizes...)...)
			cmd.Stderr = os.Stderr
			out, err := cmd.Output()
			if err != nil {
				errc <- fmt.Errorf("proc %d: %w", id, err)
				return
			}
			lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
			if len(lines) != len(sizes) {
				errc <- fmt.Errorf("proc %d: expected %d lines, got %d:\n%s", id, len(sizes), len(lines), out)
				return
			}
			for _, line := range lines {
				if !strings.HasPrefix(line, "ok ") {
					errc <- fmt.Errorf("proc %d: %s", id, line)
					return
				}
			}
		}(p)
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
}
