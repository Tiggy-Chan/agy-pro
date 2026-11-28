package seamless

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// EphemeralCA holds an in-memory per-session root CA and issues cached leaf certificates
// strictly for intercepted loopback proxy connections (never touching the OS trust store).
type EphemeralCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte

	mu    sync.Mutex
	cache map[string]*tls.Certificate
}

func NewEphemeralCA() (*EphemeralCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "agy-pro Ephemeral Local CA",
			Organization: []string{"agy-pro"},
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(48 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return &EphemeralCA{
		cert:    cert,
		key:     key,
		certPEM: pemBytes,
		cache:   make(map[string]*tls.Certificate),
	}, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, err
	}
	if n.Sign() <= 0 {
		n = big.NewInt(1)
	}
	return n, nil
}

// CertPool returns an x509.CertPool trusting this ephemeral CA (useful for tests).
func (ca *EphemeralCA) CertPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.certPEM)
	return pool
}

// WriteBundle writes a temporary PEM bundle combining system root CAs + this ephemeral CA
// so the child process trusts both normal public HTTPS hosts and the local loopback proxy.
func (ca *EphemeralCA) WriteBundle(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if sysPEM := readSystemCABundle(); len(sysPEM) > 0 {
		buf.Write(sysPEM)
		if !bytes.HasSuffix(sysPEM, []byte("\n")) {
			buf.WriteByte('\n')
		}
	}
	buf.Write(ca.certPEM)

	dst := filepath.Join(dir, "ca-bundle.pem")
	if err := os.WriteFile(dst, buf.Bytes(), 0o600); err != nil {
		return "", err
	}
	return dst, nil
}

func readSystemCABundle() []byte {
	candidates := []string{}
	if envFile := strings.TrimSpace(os.Getenv("SSL_CERT_FILE")); envFile != "" {
		candidates = append(candidates, envFile)
	}
	candidates = append(candidates,
		"/etc/ssl/cert.pem",                  // macOS, Alpine, OpenBSD
		"/etc/ssl/certs/ca-certificates.crt", // Debian, Ubuntu, Gentoo, Arch
		"/etc/pki/tls/certs/ca-bundle.crt",   // RHEL, Fedora, CentOS
		"/etc/ssl/ca-bundle.pem",             // openSUSE
		"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	)
	for _, p := range candidates {
		if data, err := os.ReadFile(p); err == nil && len(data) > 0 {
			return data
		}
	}
	return nil
}

// targetHostProvider allows GetCertificate to fall back to the CONNECT host when SNI is empty
// (e.g. IP-literal targets where RFC 6066 forbids sending SNI).
type targetHostProvider interface {
	TargetHost() string
}

// GetCertificate dynamically mints (and caches) a leaf certificate for the requested SNI host.
func (ca *EphemeralCA) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := strings.ToLower(strings.TrimSpace(hello.ServerName))
	if host == "" {
		if tp, ok := hello.Conn.(targetHostProvider); ok {
			host = strings.ToLower(strings.TrimSpace(tp.TargetHost()))
		}
	}
	if host == "" {
		host = "localhost"
	}
	return ca.LeafForHost(host)
}

// LeafForHost mints or returns a cached leaf certificate valid for host.
func (ca *EphemeralCA) LeafForHost(host string) (*tls.Certificate, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" {
		host = "localhost"
	}

	ca.mu.Lock()
	if c, ok := ca.cache[host]; ok {
		ca.mu.Unlock()
		return c, nil
	}
	ca.mu.Unlock()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: host,
		},
		NotBefore:   now.Add(-5 * time.Minute),
		NotAfter:    now.Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("sign leaf cert for %s: %w", host, err)
	}
	tlsCert := &tls.Certificate{
		Certificate: [][]byte{der, ca.cert.Raw},
		PrivateKey:  key,
	}

	ca.mu.Lock()
	ca.cache[host] = tlsCert
	ca.mu.Unlock()
	return tlsCert, nil
}
