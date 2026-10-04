package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
)

// A server that is busy says how long to wait, and the client passes it on.
func TestClientReadsRetryAfter(t *testing.T) {
	for header, want := range map[string]time.Duration{"2": 2 * time.Second, "": 0, "soon": 0, "-1": 0, "99999": time.Hour} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if header != "" {
				w.Header().Set("Retry-After", header)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"busy"}`))
		}))
		_, err := New(srv.URL, "t", time.Second).Check(context.Background(), board.CheckRequest{})
		srv.Close()
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusTooManyRequests || ae.Message != "busy" || ae.RetryAfter != want {
			t.Errorf("Retry-After %q: %v, want a wait of %s", header, err, want)
		}
	}
}
