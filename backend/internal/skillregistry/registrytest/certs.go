// Package registrytest stands a real private-registry server for tests.
//
// # Why a real HTTPS server and not a stub Provider
//
// The controls this phase adds are transport controls: TLS verification, the
// origin check, the redirect refusal, the address policy, the size ceilings,
// the credential header. A fake Provider satisfies the interface and exercises
// none of them. So the fixture is an actual net/http server behind an actual
// TLS certificate, and the tests point AO's real client at it.
//
// # Why the certificates are generated here
//
// A test that disabled certificate verification would prove the client works
// with verification off, which is the one configuration AO does not have. So
// the fixture mints its own CA at run time, hands the client that CA through
// the test-only HTTPSOptions.RootCAs seam, and keeps a SECOND, untrusted CA
// around so "a certificate AO was not given" is a case that can be tested
// rather than described.
//
// Nothing here is a production key, nothing is committed, and the system trust
// store is never touched.
package registrytest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"
)

// CA is a throwaway certificate authority for one test.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	// Pool is what a client trusts to reach servers this CA signed.
	Pool *x509.CertPool
}

// NewCA mints a CA valid for the life of the test.
func NewCA(t *testing.T) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ao registry test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &CA{cert: cert, key: key, Pool: pool}
}

// Issue signs a server certificate for one DNS name.
//
// The certificate carries the NAME and the loopback address, because AO's
// client dials the resolved address and verifies against the configured name --
// which is exactly the pinning the fixture has to reproduce.
func (ca *CA) Issue(t *testing.T, dnsName string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{dnsName},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create server certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// LoopbackResolver answers every lookup with 127.0.0.1.
//
// It is how a test gives the fixture a real DNS name without touching DNS or
// /etc/hosts. The registry is configured with a fully-qualified name, AO's
// address policy still runs against the address it resolves to, and the test
// permits 127.0.0.0/8 through the registry's own network policy -- which is the
// same control an on-premises registry at 10.x uses.
type LoopbackResolver struct{}

// LookupIP implements skillregistry.Resolver.
func (LoopbackResolver) LookupIP(_ context.Context, _ string) ([]net.IP, error) {
	return []net.IP{net.ParseIP("127.0.0.1")}, nil
}

// FixedResolver answers every lookup with the addresses it was given. It is how
// the DNS-rebinding test makes a permitted name resolve somewhere it must not.
type FixedResolver struct{ IPs []net.IP }

// LookupIP implements skillregistry.Resolver.
func (r FixedResolver) LookupIP(_ context.Context, _ string) ([]net.IP, error) {
	return r.IPs, nil
}
