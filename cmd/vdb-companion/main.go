// Command vdb-companion is the vdb daemon: a per-host gRPC server (vdb.v1)
// orchestrating the user's installed vda/vega CLIs and the on-device
// automation toolkit. Requires the Vega SDK on the host; never bundles it.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/nitishagar/vdb/internal/companion"
)

func main() {
	bind := flag.String("bind", companion.BindAddr(), "listen address (env VDB_BIND)")
	flag.Parse()

	d, err := companion.NewDaemon(*bind, companion.Config{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stderr, "vdb-companion listening on %s\n", d.Addr())
	if err := d.Serve(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
