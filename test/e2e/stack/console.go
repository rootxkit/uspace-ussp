package main

// The console's part of the browser-test stack (brief WP-18): staff
// accounts of each role made on the stack's database (an admin's TOTP
// secret answered once, for the test to compute its codes), each fake
// switched down and up again, and NATS cut and restored through a TCP
// proxy in front of it (every process reaches NATS through it), so the
// Playwright trace shows the console with every fake up and once with
// each down. Test-only; nothing here reaches an aircraft.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/accounts"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/dss/fakedss"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/ansp"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/authority"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/cisp"
)

// tcpProxy forwards every connection to target until Down, which closes
// every open connection and refuses new ones until Up: NATS as the
// processes see it when the server is gone.
type tcpProxy struct {
	ln     net.Listener
	target string
	logger *slog.Logger

	mu    sync.Mutex
	down  bool
	conns map[net.Conn]struct{}
}

// maxProxyConns bounds the connections the proxy holds (E-10): seven
// processes with a few connections each.
const maxProxyConns = 256

func newTCPProxy(target string, logger *slog.Logger) (*tcpProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &tcpProxy{ln: ln, target: target, logger: logger, conns: map[net.Conn]struct{}{}}
	go p.serve()
	return p, nil
}

func (p *tcpProxy) addr() string { return p.ln.Addr().String() }

func (p *tcpProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		refuse := p.down || len(p.conns) >= maxProxyConns
		if !refuse {
			p.conns[c] = struct{}{}
		}
		p.mu.Unlock()
		if refuse {
			_ = c.Close()
			continue
		}
		go p.pipe(c)
	}
}

func (p *tcpProxy) pipe(c net.Conn) {
	defer p.drop(c)
	up, err := net.DialTimeout("tcp", p.target, 2*time.Second)
	if err != nil {
		p.logger.Warn("nats proxy: target not reached", slog.String("error", err.Error()))
		return
	}
	p.mu.Lock()
	if p.down {
		p.mu.Unlock()
		_ = up.Close()
		return
	}
	p.conns[up] = struct{}{}
	p.mu.Unlock()
	defer p.drop(up)
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
}

func (p *tcpProxy) drop(c net.Conn) {
	_ = c.Close()
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
}

// set cuts (down) or restores the proxy.
func (p *tcpProxy) set(down bool) {
	p.mu.Lock()
	p.down = down
	var open []net.Conn
	if down {
		for c := range p.conns {
			open = append(open, c)
		}
	}
	p.mu.Unlock()
	for _, c := range open {
		_ = c.Close()
	}
}

func (p *tcpProxy) close() {
	_ = p.ln.Close()
	p.set(true)
}

// natsVia is natsURL with its host replaced by the proxy's (the
// credentials kept).
func natsVia(natsURL, proxy string) (string, error) {
	u, err := url.Parse(natsURL)
	if err != nil {
		return "", fmt.Errorf("USSP_TEST_NATS_URL: %w", err)
	}
	u.Host = proxy
	return u.String(), nil
}

// downs are the switches of each fake and of NATS.
type downs struct {
	cisp     *cisp.Fake
	registry *authority.Fake
	dss      *fakedss.DSS
	ansp     *ansp.Fake
	nats     *tcpProxy
}

// set switches one fake down or up.
func (d *downs) set(name string, down bool) error {
	switch name {
	case "cisp":
		if down {
			d.cisp.Down()
		} else {
			d.cisp.Up()
		}
	case "registry":
		if down {
			d.registry.Down()
		} else {
			d.registry.Up()
		}
	case "dss":
		d.dss.Down(down)
	case "ansp":
		if down {
			d.ansp.Down()
			d.ansp.CutStream()
		} else {
			d.ansp.Up()
			d.ansp.RestoreStream()
		}
	case "nats":
		d.nats.set(down)
	default:
		return fmt.Errorf("no fake named %q (cisp, registry, dss, ansp, nats)", name)
	}
	return nil
}

// fake is POST /fake {name, down}.
func (c *control) fake(_ context.Context, in map[string]any) (any, error) {
	down, ok := in["down"].(bool)
	if !ok {
		return nil, errors.New("down: want true or false")
	}
	if err := c.downs.set(str(in, "name"), down); err != nil {
		return nil, err
	}
	return map[string]any{"name": str(in, "name"), "down": down}, nil
}

// staff is POST /staff {username, password, role}: a console account on
// the stack's database (an admin with its TOTP secret, answered once).
func (c *control) staff(ctx context.Context, in map[string]any) (any, error) {
	out, err := c.accounts.CreateStaff(ctx, str(in, "username"), str(in, "password"), str(in, "role"), "e2e-stack")
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": out.ID, "totp_secret": out.TOTPSecret}, nil
}

// staffService is the accounts service of the control listener: the
// stack's database as api's role, the MFA key of the stack.
func staffService(ctx context.Context, pg, mfaKeyFile string) (*accounts.Service, func(), error) {
	st, err := store.Open(ctx, store.Config{RelURL: pg, RelRole: store.AppRole, MaxConns: 2, ApplicationName: "ussp-e2e-staff"})
	if err != nil {
		return nil, nil, err
	}
	hasher, err := auth.NewHasher()
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	sealer, err := accounts.LoadSealer(mfaKeyFile)
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	return &accounts.Service{Store: st, Hasher: hasher, MFA: sealer, Config: accounts.Config{TOTPIssuer: "USSP-E2E"}}, st.Close, nil
}
