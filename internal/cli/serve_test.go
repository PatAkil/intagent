package cli

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCert writes a self-signed certificate for 127.0.0.1 and its key, and
// returns their files and the certificate.
func writeCert(t *testing.T, dir, name string, key crypto.Signer) (string, string, *x509.Certificate) {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "intagent " + name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(dir, name+".pem"), filepath.Join(dir, name+".key")
	writeFile(t, certFile, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	writeFile(t, keyFile, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})))
	return certFile, keyFile, must(x509.ParseCertificate(der))
}

// An RSA certificate alone is served with a warning about what it costs;
// given an ECDSA one too, intagent's clients get that, and no warning.
func TestServeWithTwoCertificates(t *testing.T) {
	dir := t.TempDir()
	rkey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ekey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rcert, rkeyFile, rleaf := writeCert(t, dir, "rsa", rkey)
	ecert, ekeyFile, eleaf := writeCert(t, dir, "ecdsa", ekey)
	var setup safeBuffer
	app := &App{In: strings.NewReader(""), Out: &setup, Err: &setup, Version: "test", Dir: dir}
	team := filepath.Join(dir, "team.json")
	if code := app.Run(context.Background(), []string{"token", "add", "alice", "--config", team}); code != 0 {
		t.Fatalf("token add: %s", setup.String())
	}

	run := func() (*safeBuffer, *App) {
		errb := &safeBuffer{}
		return errb, &App{In: strings.NewReader(""), Out: errb, Err: errb, Version: "test", Dir: dir}
	}
	errb, app := run()
	if code := app.Run(context.Background(), []string{"serve", "--config", team, "--tls-cert", rcert, "--tls-key", rkeyFile,
		"--tls-cert", ecert}); code == 0 || !strings.Contains(errb.String(), "go together") {
		t.Fatalf("two certificates and one key: %d %s", code, errb.String())
	}

	errb, app = run()
	startServe(t, app, "--config", team, "--tls-cert", rcert, "--tls-key", rkeyFile)
	if !strings.Contains(errb.String(), "RSA key") {
		t.Fatalf("no warning about an RSA certificate: %s", errb.String())
	}

	errb, app = run()
	addr := startServe(t, app, "--config", team, "--tls-cert", rcert, "--tls-key", rkeyFile, "--tls-cert", ecert, "--tls-key", ekeyFile)
	if strings.Contains(errb.String(), "RSA key") {
		t.Fatalf("a warning about RSA with an ECDSA certificate too: %s", errb.String())
	}
	pool := x509.NewCertPool()
	pool.AddCert(rleaf)
	pool.AddCert(eleaf)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}, Timeout: 2 * time.Second}
	defer hc.CloseIdleConnections()
	resp, err := hc.Get("https://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if alg := resp.TLS.PeerCertificates[0].PublicKeyAlgorithm; alg != x509.ECDSA {
		t.Fatalf("the client got a %v certificate", alg)
	}
}
