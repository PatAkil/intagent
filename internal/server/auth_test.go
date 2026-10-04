package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/patakil/intagent/internal/board"
)

// manyMembers returns a server with n members and the token of the last.
func manyMembers(tb testing.TB, n int) (*Server, string) {
	tb.Helper()
	var members []Member
	var last string
	for i := range n {
		tok, hash, err := NewToken()
		if err != nil {
			tb.Fatal(err)
		}
		members = append(members, Member{Name: fmt.Sprintf("m%d", i), TokenSHA256: hash})
		last = tok
	}
	s, err := New(Options{Members: members, Board: board.DefaultConfig()})
	if err != nil {
		tb.Fatal(err)
	}
	return s, last
}

func cookieOf(tb testing.TB, s *Server, token string) string {
	tb.Helper()
	who, ok := s.authenticate(token)
	if !ok {
		tb.Fatal("token not known")
	}
	for _, m := range *s.members.Load() {
		if m.name == who.name {
			return s.session(m)
		}
	}
	tb.Fatal("member not listed")
	return ""
}

// Checking a dashboard cookie costs the same few compares whoever sends it,
// and no hashing: anyone who can reach the server can send one.
func TestSessionCookieCheckDoesNotHash(t *testing.T) {
	s, tok := manyMembers(t, 200)
	good := cookieOf(t, s, tok)
	bogus := strings.Repeat("ab", sha256.Size)
	for _, c := range []string{good, bogus} {
		if n := testing.AllocsPerRun(100, func() { s.sessionMember(c) }); n != 0 {
			t.Errorf("checking a cookie allocated %.0f times at 200 members", n)
		}
	}
	if who, ok := s.sessionMember(good); !ok || who.name != "m199" {
		t.Fatalf("good cookie = %q, %v", who.name, ok)
	}
	if _, ok := s.sessionMember(bogus); ok {
		t.Fatal("a made-up cookie was accepted")
	}
}

// A cookie is a MAC under the server's own key. Computed with no key, from
// what the team file holds, it must not sign anyone in: not when the server
// starts, and not after the team file is reloaded.
func TestSessionCookieNotFromTeamFile(t *testing.T) {
	ts := newTestServer(t)
	forged := func(member string) string {
		mac := hmac.New(sha256.New, nil)
		mac.Write([]byte("intagent dashboard session\x00"))
		h, _ := hex.DecodeString(HashToken(ts.tokens[member]))
		mac.Write(h)
		return hex.EncodeToString(mac.Sum(nil))
	}
	whoami := func(cookie string) int {
		req, _ := http.NewRequest(http.MethodGet, ts.url+"/v1/whoami", nil)
		req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := whoami(forged("alice")); code != http.StatusUnauthorized {
		t.Fatalf("a cookie forged from team.json after New: %d", code)
	}
	issued := ts.login(t, "alice", nil)
	if err := ts.SetMembers([]Member{
		{Name: "alice", TokenSHA256: HashToken(ts.tokens["alice"])}, {Name: "bob", TokenSHA256: HashToken(ts.tokens["bob"])},
	}); err != nil {
		t.Fatal(err)
	}
	if code := whoami(forged("alice")); code != http.StatusUnauthorized {
		t.Fatalf("a cookie forged from team.json after a reload: %d", code)
	}
	if code := whoami(issued); code != http.StatusOK {
		t.Fatalf("a cookie issued before the reload: %d", code)
	}
}

// A bearer value longer than any token is rejected before it is hashed.
func TestOversizedTokenIsNotHashed(t *testing.T) {
	ts := newTestServer(t, func(o *Options) { o.PublicRead = true })
	huge := "ia_" + strings.Repeat("x", 1<<20)
	if n := testing.AllocsPerRun(10, func() { ts.authenticate(huge) }); n != 0 {
		t.Errorf("a 1 MB token allocated %.0f times", n)
	}
	for _, path := range []string{"/v1/whoami", "/v1/hook"} {
		method := http.MethodGet
		if path == "/v1/hook" {
			method = http.MethodPost
		}
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+strings.Repeat("x", maxToken+1))
		rec := httptest.NewRecorder()
		ts.Handler().ServeHTTP(rec, req)
		// On a board open to all, an oversized token is an unknown token, not
		// no token: its owner must find out.
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with an oversized token: %d", path, rec.Code)
		}
	}
	if _, ok := ts.authenticate(ts.tokens["alice"]); !ok {
		t.Fatal("a real token was rejected")
	}
}

func BenchmarkSessionCookie(b *testing.B) {
	for _, n := range []int{2, 200, 1000} {
		s, tok := manyMembers(b, n)
		c := cookieOf(b, s, tok)
		b.Run(fmt.Sprintf("members=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				s.sessionMember(c)
			}
		})
	}
}

func BenchmarkOversizedToken(b *testing.B) {
	s, _ := manyMembers(b, 200)
	huge := "ia_" + strings.Repeat("x", 1<<20)
	b.ReportAllocs()
	for b.Loop() {
		s.authenticate(huge)
	}
}
