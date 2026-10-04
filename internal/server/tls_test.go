package server

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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
func testCert(t *testing.T, key crypto.Signer) (tls.Certificate, *x509.CertPool) {
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

func ecdsaCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testCert(t, key)
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
