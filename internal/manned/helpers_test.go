package manned

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	coresources "github.com/rootxkit/uspace-core/sources"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/sources"
)

// sink records every message published, by subject.
type sink struct {
	mu   sync.Mutex
	msgs []sent
	err  error
}

type sent struct {
	subject string
	data    []byte
}

func (s *sink) Publish(_ context.Context, subject string, m bus.Enveloped) error {
	if _, err := bus.Parse(subject); err != nil {
		return err
	}
	if err := m.Head().Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.msgs = append(s.msgs, sent{subject, b})
	return nil
}

// tracks are the man.v1 messages published.
func (s *sink) tracks() []Track {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Track
	for _, m := range s.msgs {
		if !strings.HasPrefix(m.subject, "man.v1.") {
			continue
		}
		var t Track
		_ = json.Unmarshal(m.data, &t)
		out = append(out, t)
	}
	return out
}

// statuses are the src.v1 messages published, newest last.
func (s *sink) statuses() []sources.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []sources.Status
	for _, m := range s.msgs {
		if !strings.HasPrefix(m.subject, "src.v1.") {
			continue
		}
		var st sources.Status
		_ = json.Unmarshal(m.data, &st)
		out = append(out, st)
	}
	return out
}

func (s *sink) raw(prefix string) [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][]byte
	for _, m := range s.msgs {
		if strings.HasPrefix(m.subject, prefix) {
			out = append(out, m.data)
		}
	}
	return out
}

// gate is a source switch board: a type or an instance off.
type gate struct {
	mu  sync.Mutex
	off map[string]bool
}

func (g *gate) set(key string, off bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.off == nil {
		g.off = map[string]bool{}
	}
	g.off[key] = off
}

func (g *gate) Query(t string, inst *string) coresources.Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.off[t] {
		w := coresources.WhyType
		return coresources.Decision{WhyDisabled: &w}
	}
	if inst != nil && g.off[t+"/"+*inst] {
		w := coresources.WhyInstance
		return coresources.Decision{WhyDisabled: &w}
	}
	return coresources.Decision{Enabled: true}
}

// own is an echo guard that knows one registration.
type own struct{ reg, flight string }

func (o own) EchoOf(_ string, callsign, registration *string) (string, bool) {
	for _, s := range []*string{registration, callsign} {
		if s != nil && NormRegistration(*s) == NormRegistration(o.reg) {
			return o.flight, true
		}
	}
	return "", false
}

type tokens struct {
	mu  sync.Mutex
	tok string
	err error
	got []string
}

func (t *tokens) Token(_ context.Context, base string, scopes ...string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.got = append(t.got, base+"|"+strings.Join(scopes, " "))
	return t.tok, t.err
}

var errSink = errors.New("bus down")

// within waits (a condition, never a fixed sleep) until cond holds.
func within(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %s: %s", d, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const schemaBase = "https://schemas.uspace.ge/"

// schemas compiles every schema under schemas/ (offline).
func schemas(t *testing.T) map[string]*jsonschema.Schema {
	t.Helper()
	root := filepath.Join("..", "..", "schemas")
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	var names []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "schema.json" {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(p))
		name := filepath.ToSlash(rel)
		names = append(names, name)
		return c.AddResource(schemaBase+name+".json", doc)
	})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*jsonschema.Schema{}
	for _, n := range names {
		s, err := c.Compile(schemaBase + n + ".json")
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		out[n] = s
	}
	return out
}

func validate(t *testing.T, ss map[string]*jsonschema.Schema, name string, raw []byte) {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := ss[name].Validate(inst); err != nil {
		t.Fatalf("%s does not validate: %v\n%s", name, err, raw)
	}
}
