package server

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"
)

// testCert makes a self-signed certificate for 127.0.0.1 with key, and a
// pool that trusts it.
func testCert(t testing.TB, key crypto.Signer) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "intagent test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

func ecdsaCert(t testing.TB) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testCert(t, key)
}

func rsaCert(t testing.TB) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return testCert(t, key)
}

// Given an RSA and an ECDSA certificate, in either order, intagent's clients
// get the ECDSA one, and a client that takes only RSA still gets the other.
func TestTLSPrefersECDSA(t *testing.T) {
	rc, rpool := rsaCert(t)
	ec, _ := ecdsaCert(t)
	both := x509.NewCertPool()
	both.AddCert(rc.Leaf)
	both.AddCert(ec.Leaf)
	for _, certs := range [][]tls.Certificate{{rc, ec}, {ec, rc}} {
		config := TLSConfig(certs)
		if RSAOnly(config) {
			t.Fatal("RSAOnly with an ECDSA certificate")
		}
		ts := newTestServer(t, func(o *Options) { o.TLS = config })
		addr := ts.serve(t)
		get := func(tc *tls.Config) x509.PublicKeyAlgorithm {
			t.Helper()
			hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tc, ForceAttemptHTTP2: true}}
			defer hc.CloseIdleConnections()
			resp, err := hc.Get("https://" + addr + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			return resp.TLS.PeerCertificates[0].PublicKeyAlgorithm
		}
		if alg := get(&tls.Config{RootCAs: both, MinVersion: tls.VersionTLS12}); alg != x509.ECDSA {
			t.Errorf("a Go client got a %v certificate", alg)
		}
		rsaOnly := &tls.Config{RootCAs: rpool, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12,
			CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}}
		if alg := get(rsaOnly); alg != x509.RSA {
			t.Errorf("a client that takes only RSA got a %v certificate", alg)
		}
	}
	if !RSAOnly(TLSConfig([]tls.Certificate{rc})) || RSAOnly(TLSConfig([]tls.Certificate{ec})) || RSAOnly(nil) {
		t.Error("RSAOnly is wrong")
	}
}

// A TLS handshake must finish soon after the connection is accepted; once
// it has, the connection lives as long as any other.
func TestTLSHandshakeDeadline(t *testing.T) {
	cert, pool := ecdsaCert(t)
	ts := newTestServer(t, func(o *Options) {
		o.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}}
	})
	ts.handshakeTimeout = 100 * time.Millisecond
	addr := ts.serve(t)

	silent, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = silent.Close() }()
	if !closedWithin(silent, 5*time.Second) {
		t.Fatal("a connection that never started its handshake was still open after 5 s")
	}

	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: true}}
	defer hc.CloseIdleConnections()
	for i := range 2 {
		resp, err := hc.Get("https://" + addr + "/healthz")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.ProtoMajor != 2 {
			t.Fatalf("request %d: %d over HTTP/%d", i, resp.StatusCode, resp.ProtoMajor)
		}
		// The second request reuses the connection, well past the deadline.
		time.Sleep(5 * ts.handshakeTimeout)
	}
}

// BenchmarkTLSHandshake measures a full handshake, both ends in this process,
// with the configuration TLSConfig gives each kind of certificate.
func BenchmarkTLSHandshake(b *testing.B) {
	for _, c := range []struct {
		name string
		make func(testing.TB) (tls.Certificate, *x509.CertPool)
	}{{"rsa2048", rsaCert}, {"ecdsa-p256", ecdsaCert}} {
		cert, pool := c.make(b)
		server, client := TLSConfig([]tls.Certificate{cert}), &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
		b.Run(c.name, func(b *testing.B) {
			for b.Loop() {
				sc, cc := net.Pipe()
				done := make(chan error, 1)
				go func() { done <- tls.Server(sc, server).Handshake() }()
				if err := tls.Client(cc, client).Handshake(); err != nil {
					b.Fatal(err)
				}
				if err := <-done; err != nil {
					b.Fatal(err)
				}
				_, _ = sc.Close(), cc.Close()
			}
		})
	}
}
