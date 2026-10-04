//go:build unix

package cli

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// serve listens before it restores the board, so a hook that connects
// while a large board is restored waits to be answered rather than be
// refused. Here the restore waits on a pipe, until the test has connected.
func TestServeListensWhileItRestores(t *testing.T) {
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
	restored := make(chan struct{})
	release := func() {
		select {
		case <-restored:
			return
		default:
			close(restored)
		}
		// Opening the pipe waits for the server to open it to read.
		if f, err := os.OpenFile(snapshot, os.O_WRONLY, 0); err == nil {
			_, _ = f.WriteString(`{"format":1}`)
			_ = f.Close()
		}
	}
	t.Cleanup(release)

	var errb safeBuffer
	app := &App{In: strings.NewReader(""), Out: &errb, Err: &errb, Version: "test", Dir: dir}
	listening := make(chan string, 1)
	app.Listening = func(addr string) { listening <- addr }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- app.Run(ctx, []string{"serve", "--config", team, "--data", data, "--addr", "127.0.0.1:0"})
	}()
	t.Cleanup(func() { release(); cancel(); <-done })
	var addr string
	select {
	case addr = <-listening:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not listen while it restored the board")
	}
	answered := make(chan int, 1)
	go func() {
		resp, err := (&http.Client{Timeout: 20 * time.Second}).Get("http://" + addr + "/healthz")
		if err != nil {
			answered <- 0
			return
		}
		_ = resp.Body.Close()
		answered <- resp.StatusCode
	}()
	select {
	case code := <-answered:
		t.Fatalf("answered %d before the board was restored", code)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	if code := <-answered; code != http.StatusOK {
		t.Fatalf("a request made during the restore: %d, want 200 once it was done", code)
	}
}
