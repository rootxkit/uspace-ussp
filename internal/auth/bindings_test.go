package auth

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// E-01, 06 T3: a serial bound to client A is allowed for A and refused
// for B, whatever its case; unbinding (an empty set) refuses it for A.
func TestBindingsAllowTheBoundClientOnly(t *testing.T) {
	m := NewMemoryBindings()
	mustNoErr(t, m.ProjectClientBindings(context.Background(), "client-a", []string{"1581F5FKD229400A", "1581F5FKD229400A", "TEST0001"}))
	if !m.Allowed("client-a", "1581f5fkd229400a") || !m.Allowed("client-a", " TEST0001 ") {
		t.Fatal("the bound client is refused")
	}
	if m.Allowed("client-b", "1581F5FKD229400A") {
		t.Fatal("another client is allowed a serial bound to client A")
	}
	if got := m.Folds("client-a"); !slices.Equal(got, []string{"1581F5FKD229400A", "TEST0001"}) {
		t.Fatalf("folds %v", got)
	}
	mustNoErr(t, m.ProjectClientBindings(context.Background(), "client-a", nil))
	if m.Allowed("client-a", "TEST0001") || len(m.Folds("client-a")) != 0 {
		t.Fatal("an unbound serial is still allowed")
	}
	m.Fail = errors.New("kv down")
	if err := m.ProjectClientBindings(context.Background(), "client-a", []string{"X"}); err == nil {
		t.Fatal("a failing projection succeeded")
	}
}
