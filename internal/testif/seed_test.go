package testif

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func seedFixture(t *testing.T) Fixture {
	t.Helper()
	fx, err := LoadFixture(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	return fx
}

func readState(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return strings.TrimSpace(string(b))
}

// An active operator is registered, signed in, given a client with the
// fixture's scopes and its pool bound, and the staff session is
// written; a second seed on the same state reuses the password and the
// client (no second registration, no second client).
func TestSeedAnActiveOperator(t *testing.T) {
	f := newFakeUSSP(t)
	dir := t.TempDir()
	pw := writeFile(t, "staff", "staff-password-1")
	cfg := SeedConfig{USSP: &USSP{Base: f.srv.URL, HTTP: f.srv.Client()}, Fixture: seedFixture(t), StateDir: dir,
		ConsoleUser: "staff.support", ConsolePasswordFile: pw}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := Seed(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	if res.OperatorStatus != "active" || res.ClientID != "op-test" || len(res.Bound) != len(cfg.Fixture.Serials) || !res.Console {
		t.Fatalf("%+v", res)
	}
	if readState(t, dir, SeedClientSecret) != "fresh-secret" || readState(t, dir, SeedPortalSession) != "portal-session" ||
		readState(t, dir, SeedConsole) != "console-session" {
		t.Error("state files")
	}
	for _, name := range []string{SeedAdminPassword, SeedClientSecret, SeedPortalSession, SeedConsole} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil || (st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/') {
			t.Errorf("%s: %v %v (want 0600)", name, err, st)
		}
	}
	var onDisk SeedResult
	if err := json.Unmarshal([]byte(readState(t, dir, SeedResultFile)), &onDisk); err != nil || onDisk.ClientID != "op-test" {
		t.Errorf("seed.json %+v %v", onDisk, err)
	}
	again, err := Seed(ctx, cfg, logger)
	if err != nil || again.ClientID != "op-test" || f.clientsMade != 1 || !slices.Contains(again.Notes, "the client of an earlier seed is reused") {
		t.Fatalf("second seed %+v %v, clients made %d", again, err, f.clientsMade)
	}
}

// An operator the registry has not validated gets no client: Seed says
// so in its result and writes the placeholder client, so that the run
// goes on and records every call's refusal (the twin of
// TestSeedAnActiveOperator).
func TestSeedAPendingOperator(t *testing.T) {
	f := newFakeUSSP(t)
	f.operatorStatus = "pending_validation"
	dir := t.TempDir()
	res, err := Seed(context.Background(), SeedConfig{USSP: &USSP{Base: f.srv.URL, HTTP: f.srv.Client()}, Fixture: seedFixture(t), StateDir: dir},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if res.ClientID != ClientUnavailable || len(res.Bound) != 0 || res.Validation != "unknown" || f.clientsMade != 0 {
		t.Fatalf("%+v", res)
	}
	if !strings.Contains(strings.Join(res.Notes, " "), "pending_validation, not active") {
		t.Errorf("notes %v", res.Notes)
	}
	if readState(t, dir, SeedClientID) != ClientUnavailable {
		t.Error("no placeholder client")
	}
}
