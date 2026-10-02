package proc

import (
	"context"
	"errors"
	"testing"

	coreauth "github.com/rootxkit/uspace-core/auth"
)

func TestRefuseAllRefusesEveryToken(t *testing.T) {
	_, err := RefuseAll{}.Verify(context.Background(), "x")
	var te *coreauth.TokenError
	if !errors.As(err, &te) || te.Counter != coreauth.CounterRejectedAudience {
		t.Fatalf("%v", err)
	}
}
