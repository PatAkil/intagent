package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patakil/intagent/internal/board"
)

// busyBoard fills a server's board through its hooks: 300 claims of 30 files
// each in the busy repository, and 700 more sessions spread over 29 others.
func busyBoard(b *testing.B, s *Server, now time.Time) {
	b.Helper()
	hook := func(member string, w board.Where, session string, paths ...string) {
		ev := board.HookEvent{Kind: board.KindPostEdit, Member: member, Agent: board.AgentClaudeCode, SessionID: session, Where: w, Tool: "Edit"}
		for _, p := range paths {
			ev.Paths = append(ev.Paths, board.PathRef{Path: p, Area: p[:len(p)-len("/f00.go")]})
		}
		if _, err := s.board.Hook(now, ev); err != nil {
			b.Fatal(err)
		}
	}
	for i := range 300 {
		var paths []string
		for j := range 30 {
			paths = append(paths, fmt.Sprintf("services/svc%02d/internal/pkg%d/f%02d.go", (i+j)%40, j%5, j))
		}
		w := board.Where{Repo: repo, Host: fmt.Sprintf("host%d", i), Worktree: fmt.Sprintf("/work/wt%d", i), Branch: fmt.Sprintf("feat/%d", i)}
		hook("alice", w, fmt.Sprintf("s%d", i), paths...)
	}
	for i := range 700 {
		w := board.Where{Repo: fmt.Sprintf("github.com/acme/r%02d", i%29), Host: fmt.Sprintf("h%d", i), Worktree: "/w", Branch: "main"}
		hook("bob", w, fmt.Sprintf("o%d", i), "lib/x/f00.go")
	}
}

// BenchmarkBoardReads measures what 100 open dashboards cost the hooks: each
// dashboard reloads the busy repository's board every 300 ms, as app.js did
// under steady activity, while an agent's pre_edit is timed. "per-request"
// builds a view for every request, as handleBoard did; "shared" is today's.
//
//	go test ./internal/server -run '^$' -bench BoardReads -benchtime 300x -cpu 2
func BenchmarkBoardReads(b *testing.B) {
	for _, mode := range []string{"per-request", "shared"} {
		b.Run(mode, func(b *testing.B) {
			now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
			tok, hash, _ := NewToken()
			_, other, _ := NewToken()
			s, err := New(Options{Members: []Member{{Name: "alice", TokenSHA256: hash}, {Name: "bob", TokenSHA256: other}}, Board: board.DefaultConfig(), Now: func() time.Time { return now }})
			if err != nil {
				b.Fatal(err)
			}
			busyBoard(b, s, now)
			var views atomic.Int64
			s.boards.wrapView(func(view func(string) board.View) func(string) board.View {
				return func(r string) board.View { views.Add(1); return view(r) }
			})
			h := s.Handler()
			if mode == "per-request" {
				api := h
				h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/v1/board" {
						api.ServeHTTP(w, r)
						return
					}
					s.read(func(w http.ResponseWriter, r *http.Request) {
						writeJSON(w, http.StatusOK, s.boards.view(board.RepoID(r.URL.Query().Get("repo"))))
					}).ServeHTTP(w, r)
				})
			}
			hs := httptest.NewServer(h)
			defer hs.Close()

			ctx, stop := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			var egress atomic.Int64
			client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 200}}
			for range 100 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for ctx.Err() == nil {
						req, _ := http.NewRequestWithContext(ctx, http.MethodGet, hs.URL+"/v1/board?repo="+repo, nil)
						req.Header.Set("Authorization", "Bearer "+tok)
						req.Header.Set("Accept-Encoding", "gzip")
						if resp, err := client.Do(req); err == nil {
							n, _ := io.Copy(io.Discard, resp.Body)
							egress.Add(n)
							_ = resp.Body.Close()
						}
						select {
						case <-ctx.Done():
						case <-time.After(300 * time.Millisecond):
						}
					}
				}()
			}
			time.Sleep(time.Second) // let the dashboards fall into their rhythm
			views.Store(0)
			egress.Store(0)
			ev := board.HookEvent{Kind: board.KindPreEdit, Agent: board.AgentClaudeCode, SessionID: "s7", Tool: "Edit",
				Where: board.Where{Repo: repo, Host: "host7", Worktree: "/work/wt7"}, Paths: []board.PathRef{{Path: "services/svc07/internal/pkg0/f00.go"}}}
			body, _ := json.Marshal(ev)
			lat := make([]time.Duration, 0, b.N)
			start := time.Now()
			b.ResetTimer()
			for range b.N {
				req, _ := http.NewRequest(http.MethodPost, hs.URL+"/v1/hook", bytes.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+tok)
				t0 := time.Now()
				resp, err := client.Do(req)
				if err != nil {
					b.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				lat = append(lat, time.Since(t0))
				time.Sleep(10 * time.Millisecond)
			}
			b.StopTimer()
			elapsed := time.Since(start).Seconds()
			stop()
			wg.Wait()
			slices.Sort(lat)
			b.ReportMetric(float64(lat[len(lat)/2].Microseconds())/1000, "p50-ms")
			b.ReportMetric(float64(lat[len(lat)*99/100].Microseconds())/1000, "p99-ms")
			b.ReportMetric(float64(views.Load())/elapsed, "views/s")
			b.ReportMetric(float64(egress.Load())/elapsed/1e6, "MB/s")
		})
	}
}
