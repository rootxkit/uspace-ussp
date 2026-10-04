package httpx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writePair writes a self-signed certificate and its key as PEM files.
func writePair(t *testing.T, dir, name string) (certFile, keyFile string, cert *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ = x509.ParseCertificate(der)
	kd, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, cert
}

func TestMTLSClientRefusesByName(t *testing.T) {
	dir := t.TempDir()
	cert, key, _ := writePair(t, dir, "client")
	garbage := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		cfg  MTLSConfig
		want string
	}{
		{MTLSConfig{Mode: MTLSRequired}, "USSP_MTLS_CERT_FILE"},
		{MTLSConfig{Mode: MTLSRequired, CertFile: cert}, "USSP_MTLS_KEY_FILE"},
		{MTLSConfig{Mode: MTLSRequired, CertFile: garbage, KeyFile: key}, "USSP_MTLS_CERT_FILE"},
		{MTLSConfig{Mode: MTLSOff, CAFile: filepath.Join(dir, "missing.pem")}, "USSP_MTLS_CA_FILE"},
		{MTLSConfig{Mode: MTLSOff, CAFile: garbage}, "USSP_MTLS_CA_FILE"},
		{MTLSConfig{Mode: "sometimes"}, "USSP_MTLS_MODE"},
	} {
		if _, err := MTLSClient(c.cfg, time.Second); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want an error naming %s", c.cfg, err, c.want)
		}
	}
}

// E-01 pair: a server that requires a client certificate takes the
// required client and refuses the off client.
func TestMTLSClientPresentsItsCertificate(t *testing.T) {
	dir := t.TempDir()
	cert, key, clientCert := writePair(t, dir, "client")
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	pool := x509.NewCertPool()
	pool.AddCert(clientCert)
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()
	ca := filepath.Join(dir, "server-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	required, err := MTLSClient(MTLSConfig{Mode: MTLSRequired, CertFile: cert, KeyFile: key, CAFile: ca}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := required.Get(srv.URL)
	if err != nil {
		t.Fatalf("required: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("required: %d", resp.StatusCode)
	}
	off, err := MTLSClient(MTLSConfig{Mode: MTLSOff, CAFile: ca}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := off.Get(srv.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatal("off: the server took a client without a certificate")
	}
}
