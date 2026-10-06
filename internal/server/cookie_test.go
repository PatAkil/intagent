package server

import (
	"crypto/tls"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
)

// The session cookie is HttpOnly, SameSite=Strict and for the whole site,
// over plain HTTP and over TLS alike, and Secure exactly when the browser
// came over TLS, so that a plain-HTTP server on a trusted network still
// signs members in. Logging out sends the same cookie expired, which a
// browser then drops.
func TestSessionCookieAttributes(t *testing.T) {
	cert, pool := ecdsaCert(t)
	for _, encrypted := range []bool{false, true} {
		name := "plain HTTP"
		if encrypted {
			name = "TLS"
		}
		t.Run(name, func(t *testing.T) {
			ts := newTestServer(t, func(o *Options) {
				if encrypted {
					o.TLS = TLSConfig([]tls.Certificate{cert})
				}
			})
			base := "http://" + ts.serve(t)
			tr := &http.Transport{}
			if encrypted {
				base = strings.Replace(base, "http://", "https://", 1)
				tr.TLSClientConfig = &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
			}
			t.Cleanup(tr.CloseIdleConnections)
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: tr, Jar: jar,
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			check := func(what string, resp *http.Response, expired bool) {
				t.Helper()
				c := resp.Cookies()
				if len(c) != 1 || c[0].Name != cookieName || c[0].Path != "/" || !c[0].HttpOnly ||
					c[0].SameSite != http.SameSiteStrictMode || c[0].Secure != encrypted || (c[0].MaxAge < 0) != expired {
					t.Fatalf("%s cookie = %+v, want HttpOnly, SameSite=Strict, Path=/, Secure %t, expired %t", what, c, encrypted, expired)
				}
			}
			resp, err := client.PostForm(base+"/ui/login", url.Values{"token": {ts.tokens["alice"]}})
			if err != nil || resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("login: %v %v", err, resp)
			}
			check("login", resp, false)
			u, _ := url.Parse(base)
			if len(jar.Cookies(u)) != 1 {
				t.Fatalf("the browser kept %d cookies after signing in, want 1", len(jar.Cookies(u)))
			}
			resp, err = client.Get(base + "/v1/whoami")
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("whoami with the cookie: %v %v", err, resp)
			}
			resp, err = client.Post(base+"/ui/logout", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			check("logout", resp, true)
			if c := jar.Cookies(u); len(c) != 0 {
				t.Fatalf("the browser still holds %v after logging out", c)
			}
		})
	}
}
