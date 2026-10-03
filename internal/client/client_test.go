package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
)

func TestClientSpeaksTheAPI(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer ia_tok" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unknown token"}`))
			return
		}
		if r.Method == http.MethodPost && r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s without a JSON content type", r.URL.Path)
		}
		switch r.URL.Path {
		case "/v1/whoami":
			_ = json.NewEncoder(w).Encode(board.Whoami{Member: "alice", Version: "v1", Demo: true})
		case "/v1/intents/release":
			_, _ = w.Write([]byte(`{"released":3}`))
		case "/v1/check":
			_, _ = w.Write([]byte(`{"conflicts":[{"path":"a.go","member":"bob"}],"text":"bob"}`))
		case "/v1/board":
			if r.URL.Query().Get("format") == "text" {
				_, _ = w.Write([]byte("the board as text"))
				return
			}
			_, _ = w.Write([]byte(`{"repo":"r"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	c := New(srv.URL+"/", "ia_tok", time.Second)

	if who, err := c.Whoami(ctx); err != nil || who.Member != "alice" || !who.Demo {
		t.Fatalf("Whoami = %+v, %v", who, err)
	}
	if n, err := c.Release(ctx, board.ReleaseRequest{}); err != nil || n != 3 {
		t.Fatalf("Release = %d, %v", n, err)
	}
	if res, err := c.Check(ctx, board.CheckRequest{}); err != nil || len(res.Conflicts) != 1 || res.Text != "bob" {
		t.Fatalf("Check = %+v, %v", res, err)
	}
	if text, err := c.BoardText(ctx, "github.com/acme/mono"); err != nil || text != "the board as text" {
		t.Fatalf("BoardText = %q, %v", text, err)
	}
	if v, err := c.Board(ctx, "a b"); err != nil || v.Repo != "r" {
		t.Fatalf("Board = %+v, %v", v, err)
	}
	if _, err := c.Repos(ctx); err == nil {
		t.Fatal("a 404 was not an error")
	} else if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "Not Found") {
		t.Fatalf("404 error = %v", err)
	}
	if !strings.Contains(strings.Join(seen, "\n"), "GET /v1/board?repo=a+b") {
		t.Fatalf("the repository was not escaped: %v", seen)
	}

	_, err := New(srv.URL, "ia_rotated", time.Second).Whoami(ctx)
	var ae *APIError
	if !IsUnauthorized(err) || !errors.As(err, &ae) || ae.Message != "unknown token" {
		t.Fatalf("a rejected token: %v", err)
	}
	if IsUnauthorized(errors.New("connection refused")) {
		t.Fatal("any error counted as unauthorised")
	}
}

func TestClientBoundsTheAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := []byte(strings.Repeat(" ", 1<<20))
		for range maxResponse>>20 + 1 {
			_, _ = w.Write(chunk)
		}
	}))
	defer srv.Close()
	_, err := New(srv.URL, "t", 10*time.Second).Whoami(context.Background())
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("an oversized answer: %v", err)
	}
}

func TestClientTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	start := time.Now()
	if _, err := New(srv.URL, "t", 100*time.Millisecond).Whoami(context.Background()); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("a hung server: %v after %s", err, time.Since(start))
	}
}
