package companion

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"

	vdbv1 "github.com/nitishagar/vdb/internal/gen/vdb/v1"
	"github.com/nitishagar/vdb/internal/vda"
	"github.com/nitishagar/vdb/internal/vegacli"
)

// EnvVarBind overrides the companion listen address (default 127.0.0.1:10882).
const EnvVarBind = "VDB_BIND"

// DefaultBind mirrors idb's companion port convention (10882).
const DefaultBind = "127.0.0.1:10882"

// BindAddr resolves the configured listen address.
func BindAddr() string {
	if v := os.Getenv(EnvVarBind); v != "" {
		return v
	}
	return DefaultBind
}

// DefaultMaxMessageBytes is the explicit gRPC message ceiling (invariant
// 14: limits are explicit and documented; screenshots are ~11 MB on
// hardware, so 64 MiB leaves headroom without being unbounded).
const DefaultMaxMessageBytes = 64 << 20

// Daemon owns the listener, server, and shutdown ordering.
type Daemon struct {
	Bind     string
	Registry *Registry
	Service  *Service
	Server   *grpc.Server
	listener net.Listener
}

// NewDaemon resolves adapters, builds the registry and service, and binds
// the listener. Callers must Close.
func NewDaemon(bind string, cfg Config) (*Daemon, error) {
	vdaBin, err := vda.ResolveVDA()
	if err != nil {
		return nil, fmt.Errorf("companion: %w", err)
	}
	vegaBin, err := vegacli.ResolveVega()
	if err != nil {
		return nil, fmt.Errorf("companion: %w", err)
	}
	reg := NewRegistry(vda.New(vdaBin), vegacli.New(vegaBin))
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		return nil, fmt.Errorf("companion: listen %s: %w", bind, err)
	}
	srv := grpc.NewServer(
		grpc.MaxSendMsgSize(DefaultMaxMessageBytes),
		grpc.MaxRecvMsgSize(DefaultMaxMessageBytes),
	)
	svc := NewService(reg, cfg)
	vdbv1.RegisterCompanionServer(srv, svc)
	return &Daemon{Bind: bind, Registry: reg, Service: svc, Server: srv, listener: ln}, nil
}

// Addr is the bound listener address (useful when binding :0).
func (d *Daemon) Addr() string { return d.listener.Addr().String() }

// Serve runs until ctx is canceled, then shuts down gracefully: stop
// accepting → let in-flight RPCs finish → release every session forward.
func (d *Daemon) Serve(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() { errCh <- d.Server.Serve(d.listener) }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		stopped := make(chan struct{})
		go func() {
			d.Server.GracefulStop()
			close(stopped)
		}()
		// GracefulStop can block on stuck streams; hard-stop after a bound.
		timer := time.NewTimer(gracefulStopGrace)
		defer timer.Stop()
		select {
		case <-stopped:
		case <-timer.C:
			d.Server.Stop()
		}
		closeCtx, cancel := context.WithTimeout(context.Background(), stopGrace)
		defer cancel()
		d.Registry.Close(closeCtx)
		return nil
	}
}

// stopGrace bounds session cleanup at shutdown.
const stopGrace = 5 * time.Second

// gracefulStopGrace bounds GracefulStop before a hard stop takes over
// (stuck streams must not wedge shutdown).
const gracefulStopGrace = 10 * time.Second
