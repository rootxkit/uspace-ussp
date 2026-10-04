package httpx

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
	"time"

	"github.com/rootxkit/uspace-core/core"
)

// The modes of USSP_MTLS_MODE (cross-plan M25).
const (
	MTLSRequired = "required"
	MTLSOff      = "off"
)

// MTLSConfig is the client side of mTLS towards the ANSP: the mode and
// the PEM files of USSP_MTLS_CERT_FILE, USSP_MTLS_KEY_FILE and
// USSP_MTLS_CA_FILE.
type MTLSConfig struct {
	Mode     string
	CertFile string
	KeyFile  string
	CAFile   string
}

// MTLSClient is an HTTP client for a call USSP_MTLS_MODE governs, with
// timeout. required presents the client certificate (both files must
// load, else the error names the variable); off presents none (the lab
// and staging; the caller logs it at error level). A CA file, when set,
// replaces the system roots in either mode. TLS 1.2 is the floor.
func MTLSClient(c MTLSConfig, timeout time.Duration) (*http.Client, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch c.Mode {
	case MTLSRequired:
		if c.CertFile == "" {
			return nil, core.Fieldf("USSP_MTLS_CERT_FILE", "required with USSP_MTLS_MODE=required")
		}
		if c.KeyFile == "" {
			return nil, core.Fieldf("USSP_MTLS_KEY_FILE", "required with USSP_MTLS_MODE=required")
		}
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, core.Fieldf("USSP_MTLS_CERT_FILE", "the certificate and USSP_MTLS_KEY_FILE do not load as a PEM key pair")
		}
		cfg.Certificates = []tls.Certificate{cert}
	case MTLSOff:
	default:
		return nil, core.Fieldf("USSP_MTLS_MODE", "must be required or off, got %q", c.Mode)
	}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, core.Fieldf("USSP_MTLS_CA_FILE", "cannot be read")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, core.Fieldf("USSP_MTLS_CA_FILE", "holds no PEM certificate")
		}
		cfg.RootCAs = pool
	}
	tr, _ := http.DefaultTransport.(*http.Transport)
	t := tr.Clone()
	t.TLSClientConfig = cfg
	return &http.Client{Timeout: timeout, Transport: t}, nil
}
