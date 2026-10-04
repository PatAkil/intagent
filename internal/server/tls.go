package server

import (
	"crypto"
	"crypto/rsa"
	"crypto/tls"
	"slices"
)

// TLSConfig returns the configuration for serving HTTPS with certs. A client
// is given the first certificate it can use, so those with an ECDSA or
// Ed25519 key go before RSA ones: intagent's clients, every hook among them,
// then get a certificate whose signature costs the server a small fraction of
// an RSA one, and a browser that accepts only RSA still gets one.
func TLSConfig(certs []tls.Certificate) *tls.Config {
	certs = slices.Clone(certs)
	slices.SortStableFunc(certs, func(a, b tls.Certificate) int { return rank(a) - rank(b) })
	// h2 first: each open dashboard holds a stream, and HTTP/1.1 browsers
	// allow only six connections to a server.
	return &tls.Config{Certificates: certs, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}}
}

func rank(c tls.Certificate) int {
	if isRSA(c) {
		return 1
	}
	return 0
}

// RSAOnly reports whether every certificate in config has an RSA key, so
// that every connection costs the server an RSA signature.
func RSAOnly(config *tls.Config) bool {
	return config != nil && len(config.Certificates) > 0 && !slices.ContainsFunc(config.Certificates, func(c tls.Certificate) bool {
		return !isRSA(c)
	})
}

func isRSA(c tls.Certificate) bool {
	k, ok := c.PrivateKey.(crypto.Signer)
	if !ok {
		return false
	}
	_, ok = k.Public().(*rsa.PublicKey)
	return ok
}
