//go:build unix

package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// serve opens its port only once the board is restored: a hook that
// connects during the restore is refused, and tries again, rather than wait
// in the kernel's queue to be read and decided after its client has gone
// ahead without it. Here the restore waits on a pipe until the test has
// tried to connect.
func TestServeOpensItsPortOnceRestored(t *testing.T) {
	dir := t.TempDir()
	var setup safeBuffer
	team := filepath.Join(dir, "team.json")
	if code := (&App{In: strings.NewReader(""), Out: &setup, Err: &setup, Version: "test", Dir: dir}).Run(context.Background(),
		[]string{"token", "add", "alice", "--config", team}); code != 0 {
		t.Fatalf("token add: %s", setup.String())
	}
	data := filepath.Join(dir, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(data, "board.json")
	if err := syscall.Mkfifo(snapshot, 0o600); err != nil {
		t.Skipf("no named pipes here: %v", err)
	}
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := free.Addr().String()
	_ = free.Close()

	// Opening the pipe to write waits for serve to open it to read: then
	// the restore is under way, and waits for what the test writes.
	opened := make(chan *os.File, 1)
	go func() {
		f, err := os.OpenFile(snapshot, os.O_WRONLY, 0)
		if err != nil {
			f = nil
		}
		opened <- f
	}()
	var errb safeBuffer
	app := &App{In: strings.NewReader(""), Out: &errb, Err: &errb, Version: "test", Dir: dir}
	listening := make(chan string, 1)
	app.Listening = func(addr string) { listening <- addr }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- app.Run(ctx, []string{"serve", "--config", team, "--data", data, "--addr", addr})
	}()
	var pipe *os.File
	select {
	case pipe = <-opened:
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not start to restore the board")
	}
	if pipe == nil {
		t.Fatal("the snapshot's pipe could not be opened")
	}
	t.Cleanup(func() { _ = pipe.Close(); cancel(); <-done })

	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err == nil {
		_ = c.Close()
		t.Fatal("serve took a connection while it restored the board: a hook would wait in the kernel's queue, " +
			"to be decided after its client went ahead")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("a connection during the restore: %v, want it refused", err)
	}
	select {
	case <-listening:
		t.Fatal("serve said it listens before the board was restored")
	default:
	}

	if _, err := pipe.WriteString(`{"format":1}`); err != nil {
		t.Fatal(err)
	}
	_ = pipe.Close()
	select {
	case <-listening:
	case <-time.After(10 * time.Second):
		t.Fatalf("serve did not listen once the board was restored: %s", errb.String())
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz once restored: %d", resp.StatusCode)
	}
}
