package server

import (
	"bufio"
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
)

// slowConns caps the server's socket send buffers, so an answer of a few
// megabytes cannot all sit in the kernel while the client reads nothing.
func slowConns(ctx context.Context, c net.Conn) context.Context {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(8 << 10)
	}
	return ctx
}

func TestAnswersNeedProgressNotSpeed(t *testing.T) {
	defer func(d time.Duration) { writeTimeout = d }(writeTimeout)
	writeTimeout = time.Second
	payload := map[string]string{"x": strings.Repeat("x", 2<<20)}
	returned := make(chan struct{}, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer func() { returned <- struct{}{} }()
		writeJSON(w, http.StatusOK, payload)
	}))
	srv.Config.ConnContext = slowConns
	srv.Start()
	defer srv.Close()
	dial := func(readBuffer int) net.Conn {
		c, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if readBuffer > 0 {
			_ = c.(*net.TCPConn).SetReadBuffer(readBuffer)
		}
		fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: test\r\n\r\n")
		return c
	}

	// A client that stops reading lets go of the handler.
	stuck := dial(8 << 10)
	defer func() { _ = stuck.Close() }()
	select {
	case <-returned:
	case <-time.After(15 * time.Second):
		t.Fatal("a client that reads nothing holds its handler")
	}

	// One that reads slowly but steadily gets the whole answer.
	slow := dial(0)
	defer func() { _ = slow.Close() }()
	resp, err := http.ReadResponse(bufio.NewReaderSize(&trickle{r: slow}, 16<<10), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || got["x"] != payload["x"] {
		t.Fatalf("slow client: %v, %d bytes", err, len(got["x"]))
	}
	<-returned
}

// trickle reads at most 16 KB every 5 ms: about 3 MB/s, a 64 KB piece in 20 ms.
type trickle struct{ r io.Reader }

func (t *trickle) Read(p []byte) (int, error) {
	time.Sleep(5 * time.Millisecond)
	return t.r.Read(p[:min(len(p), 16<<10)])
}
