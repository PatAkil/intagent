package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
)

// slowConns caps the server's socket send buffers, so an answer of a few
// megabytes cannot all sit in the kernel while the client reads nothing.
func slowConns(ctx context.Context, c net.Conn) context.Context {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(8 << 10)
	}
	return ctx
}

// Answers bound how long they may go without progress, not how long they
// take: a client that stops reading lets go of its handler, while one that
// reads slowly but steadily gets everything, though the whole answer takes
// several times writeTimeout. Each 64 KB piece reaches the slow client in
// 20 to 60 ms, a fifth of writeTimeout or less; the whole answer takes at
// least 3 MB at trickle's 3.2 MB/s, about 0.9 s. A writeJSON answer, the
// shared board answer, written by writeBoard, and a dashboard file, written
// by a handler of the dashboard's own, are checked.
func TestAnswersNeedProgressNotSpeed(t *testing.T) {
	defer func(d time.Duration) { writeTimeout = d }(writeTimeout)
	writeTimeout = 300 * time.Millisecond
	big := strings.Repeat("x", 3<<20)
	ts := newTestServer(t)
	ts.boards.wrapView(func(view func(string) board.View) func(string) board.View {
		return func(r string) board.View {
			v := view(r)
			v.Claims = []board.ClaimView{{ID: "c_1", Member: "alice", Task: big}}
			return v
		}
	})
	for _, c := range []struct {
		name, path string
		handler    http.Handler
	}{
		{"json", "/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"x": big})
		})},
		{"board", "/v1/board?repo=" + repo, ts.Handler()},
		{"dashboard", "/app.js", newTestServer(t, func(o *Options) {
			o.Dashboard = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `"`+big+`"`) // as http.FileServer writes a file
			})
		}).Handler()},
	} {
		t.Run(c.name, func(t *testing.T) {
			returned := make(chan struct{}, 2)
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { returned <- struct{}{} }()
				c.handler.ServeHTTP(w, r)
			}))
			srv.Config.ConnContext = slowConns
			srv.Start()
			defer srv.Close()
			// The client's receive buffer is small too, so the reader, not
			// the kernel, paces the writer.
			dial := func() net.Conn {
				conn, err := net.Dial("tcp", srv.Listener.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.(*net.TCPConn).SetReadBuffer(64 << 10)
				fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\n\r\n", c.path, ts.tokens["bob"])
				return conn
			}

			stuck := dial()
			defer func() { _ = stuck.Close() }()
			select {
			case <-returned:
			case <-time.After(15 * time.Second):
				t.Fatal("a client that reads nothing holds its handler")
			}

			slow := dial()
			defer func() { _ = slow.Close() }()
			start := time.Now()
			resp, err := http.ReadResponse(bufio.NewReaderSize(&trickle{r: slow}, 16<<10), nil)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != http.StatusOK || !json.Valid(body) || !bytes.Contains(body, []byte(big)) {
				t.Fatalf("slow client cut off after %v: %d, %v, %d bytes", time.Since(start), resp.StatusCode, err, len(body))
			}
			<-returned
		})
	}
}

// trickle reads at most 16 KB every 5 ms: about 3 MB/s, a 64 KB piece in 20 ms.
type trickle struct{ r io.Reader }

func (t *trickle) Read(p []byte) (int, error) {
	time.Sleep(5 * time.Millisecond)
	return t.r.Read(p[:min(len(p), 16<<10)])
}
