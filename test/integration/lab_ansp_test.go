//go:build integration && lab

package integration

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // RFC 6238 TOTP is HMAC-SHA1: the ANSP's console MFA
	"crypto/x509"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/coordination"
	coordstore "github.com/rootxkit/uspace-ussp/internal/coordination/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// The S-M2 lab clause of WP-15 (docs/RUNBOOKS/WP-15.md): "the lab ANSP
// acknowledges the notice". This USSP's Notifier and Sender, on the real
// relational database, post a nonconformance notice to the real
// uspace-ansp api (the binary built from the commit api/clients/SOURCE
// pins, on its own migrated databases), whose watch supervisor then
// acknowledges it on the ANSP's console API; the Sender reads the
// acknowledgement back and stores ats_ack_ref. The token is a lab
// issuer's, minted here, scope ansp.coordination, aud the ANSP's host.
//
// Environment: LAB_ANSP_BIN (the uspace-ansp api binary),
// LAB_ANSP_RELATIONAL_DSN and LAB_ANSP_TIMESERIES_DSN (its databases,
// migrated with `api migrate relational timeseries`).

type labIssuer struct {
	iss *coreauth.Issuer
	url string
	sub string
}

// labInstance is the ANSP instance name of the lab run.
const labInstance = "lab"

func (l labIssuer) Token(_ context.Context, baseURL string, scopes ...string) (string, error) {
	u := strings.TrimPrefix(strings.TrimPrefix(baseURL, "http://"), "https://")
	host, _, err := net.SplitHostPort(strings.TrimRight(u, "/"))
	if err != nil {
		host = strings.TrimRight(u, "/")
	}
	return l.iss.Issue(l.sub, host, scopes, 5*time.Minute, time.Now())
}

func totp(secret string, at time.Time) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.TrimRight(secret, "=")))
	if err != nil {
		return "", err
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(at.Unix()/30))
	h := hmac.New(sha1.New, key)
	h.Write(msg[:])
	sum := h.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff) % 1_000_000
	return fmt.Sprintf("%06d", code), nil
}

func postJSON(t *testing.T, url, bearer string, body any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// signIn signs a console account in (password, then TOTP; the enrolment
// comes with the first sign-in) and returns its session token.
func signIn(t *testing.T, base, user, pass string) string {
	t.Helper()
	code, body := postJSON(t, base+"/v1/auth/login", "", map[string]string{"username": user, "password": pass})
	var ch struct {
		MFAToken  string `json:"mfa_token"`
		Enrolment struct {
			Secret string `json:"secret"`
		} `json:"enrolment"`
	}
	if code != 200 || json.Unmarshal(body, &ch) != nil || ch.Enrolment.Secret == "" {
		t.Fatalf("login %s: %d %s", user, code, body)
	}
	otp, err := totp(ch.Enrolment.Secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	code, body = postJSON(t, base+"/v1/auth/mfa", "", map[string]string{"mfa_token": ch.MFAToken, "code": otp})
	var s struct {
		Token string `json:"token"`
	}
	if code != 200 || json.Unmarshal(body, &s) != nil || s.Token == "" {
		t.Fatalf("mfa %s: %d %s", user, code, body)
	}
	return s.Token
}

func writePEMKey(t *testing.T, dir, name string) (string, *rsa.PrivateKey) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, k
}

func TestLabANSPAcknowledgesTheNotice(t *testing.T) {
	bin := labEnv(t, "LAB_ANSP_BIN")
	relDSN, tsDSN := labEnv(t, "LAB_ANSP_RELATIONAL_DSN"), labEnv(t, "LAB_ANSP_TIMESERIES_DSN")
	ctx := context.Background()
	dir := t.TempDir()

	// The lab issuer: its JWKS served here, its tokens minted here.
	_, issKey := writePEMKey(t, dir, "issuer.pem")
	jwksSrv := httptest.NewServer(nil)
	t.Cleanup(jwksSrv.Close)
	iss, err := coreauth.NewIssuer(jwksSrv.URL, issKey, "lab-ansp-1")
	if err != nil {
		t.Fatal(err)
	}
	jwks, err := json.Marshal(iss.JWKS())
	if err != nil {
		t.Fatal(err)
	}
	jwksSrv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwks)
	})

	// The ANSP api on a free port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	base := "http://" + addr
	sessionKey, _ := writePEMKey(t, dir, "session.pem")
	var secret [32]byte
	_, _ = rand.Read(secret[:])
	secretsFile, adminPassFile := filepath.Join(dir, "secrets.key"), filepath.Join(dir, "admin.pass")
	adminPass := "lab-admin-" + hex.EncodeToString(secret[:4])
	if err := os.WriteFile(secretsFile, []byte(hex.EncodeToString(secret[:])), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(adminPassFile, []byte(adminPass), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "ANSP_PROCESS=api", "ANSP_INSTANCE="+labInstance, "ANSP_HTTP_ADDR="+addr, "ANSP_AUDIENCES="+addr+",127.0.0.1",
		"ANSP_PUBLIC_BASE_URL="+base, "ANSP_RELATIONAL_DSN="+relDSN, "ANSP_TIMESERIES_DSN="+tsDSN,
		"ANSP_TOKEN_ISSUERS="+jwksSrv.URL+"="+jwksSrv.URL+"/.well-known/jwks.json", "ANSP_MTLS_MODE=off",
		"ANSP_SESSION_KEY_FILE="+sessionKey, "ANSP_SECRETS_KEY_FILE="+secretsFile,
		"ANSP_BOOTSTRAP_ADMIN_USERNAME=labadmin", "ANSP_BOOTSTRAP_ADMIN_PASSWORD_FILE="+adminPassFile, "ANSP_LOG_LEVEL=info")
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("ANSP log:\n%s", logs.String())
		}
	})
	waitFor(t, 30*time.Second, "the ANSP api", func() bool {
		resp, err := http.Get(base + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == 200
	})

	// A watch supervisor at the ANSP.
	admin := signIn(t, base, "labadmin", adminPass)
	supPass := "lab-supervisor-" + hex.EncodeToString(secret[4:8])
	if code, body := postJSON(t, base+"/v1/users", admin, map[string]string{"username": "labsup", "password": supPass, "role": "watch_supervisor"}); code != 201 {
		t.Fatalf("create supervisor: %d %s", code, body)
	}
	supervisor := signIn(t, base, "labsup", supPass)

	// This USSP: a nonconforming transition of a seeded flight.
	client, err := coordination.NewClient(base, labIssuer{iss: iss, url: jwksSrv.URL, sub: "ussp-DEV01-01"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := coordstore.Store{S: appStore(t)}
	pol := policy.Defaults()
	pol.ATSAckPollS = 1
	rctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	yes := true
	n := &coordination.Notifier{Store: st, Airspaces: func() (coordination.Airspaces, bool) {
		return coordination.Airspaces{Version: "1", Controlled: map[string]*bool{"UA-LAB": &yes}}, true
	},
		SystemID: "DEV01", Counters: &core.Counters{}, Logger: quiet()}
	s := &coordination.Sender{Store: st, ANSP: client, Policy: func() policy.Values { return pol }, Counters: &core.Counters{}, Logger: quiet()}
	go n.Run(rctx, 200*time.Millisecond)
	go s.Run(rctx, 200*time.Millisecond)
	f := seedFlight(t, "activated", nil, time.Now().Add(-5*time.Minute))
	stateID := seedState(t, f.flightID, "nonconforming", "above_upper", time.Now(), 4.5, 25)
	recorded := time.Now()
	waitFor(t, 10*time.Second, "the ANSP's receipt", func() bool { n, _ := atsFacts(t, stateID); return n != nil })
	t.Logf("receipt stored %v after the conformance state was recorded", time.Since(recorded).Round(time.Millisecond))
	ref := coordination.Ref("DEV01", coordination.KindNonconformance, f.intentID, stateID)
	var ackID string
	if err := appPool(t).QueryRow(ctx, "SELECT ack_id FROM coordination_notices WHERE notice_ref = $1", ref).Scan(&ackID); err != nil {
		t.Fatal(err)
	}

	// The supervisor acknowledges it on the ANSP's console API.
	code, body := postJSON(t, base+"/v1/coordination/inbox/"+ackID+"/acknowledge", supervisor, map[string]string{"note": "lab run: aware"})
	if code != 200 {
		t.Fatalf("acknowledge: %d %s", code, body)
	}
	acked := time.Now()
	waitFor(t, 10*time.Second, "ats_ack_ref", func() bool { _, r := atsFacts(t, stateID); return r != nil && *r == ackID })
	t.Logf("ack_id %s; acknowledgement read back %v after the supervisor acknowledged", ackID, time.Since(acked).Round(time.Millisecond))
	var by string
	if err := appPool(t).QueryRow(ctx, "SELECT state || ' by ' || coalesce(acknowledged_by, '?') FROM coordination_notices WHERE notice_ref = $1", ref).Scan(&by); err != nil {
		t.Fatal(err)
	}
	t.Logf("notice %s: %s", ref, by)
	if by != "acknowledged by watch_supervisor" {
		t.Fatalf("notice %s", by)
	}
	// A repeat of the same notice is the first receipt (the ANSP's 200).
	var body0 []byte
	if err := appPool(t).QueryRow(ctx, "SELECT body FROM coordination_notices WHERE notice_ref = $1", ref).Scan(&body0); err != nil {
		t.Fatal(err)
	}
	r, err := client.Submit(ctx, body0)
	if err != nil || !r.Repeat || r.AckID != ackID {
		t.Fatalf("repeat: %+v %v", r, err)
	}
	t.Logf("a repeat of the notice answered 200 with ack_id %s", r.AckID)

	// An intent in controlled U-space airspace: intent_notice on its
	// activation and ended on its end, both received by the ANSP.
	g := seedFlight(t, "activated", []string{"UA-LAB"}, time.Now().Add(-5*time.Minute))
	for _, k := range []coordination.Kind{coordination.KindIntentNotice, coordination.KindEnded} {
		if k == coordination.KindEnded {
			endIntent(t, g.intentID)
		}
		ref := coordination.Ref("DEV01", k, g.intentID, 0)
		waitFor(t, 10*time.Second, string(k)+" received by the ANSP", func() bool {
			var state string
			err := appPool(t).QueryRow(ctx, "SELECT state FROM coordination_notices WHERE notice_ref = $1", ref).Scan(&state)
			return err == nil && state == "received"
		})
		t.Logf("%s received by the ANSP", k)
	}
}
