package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/transport/acp"
)

var flagServeACP bool

// serveCmd is `milk serve --acp` — the ACP v2 JSON-RPC stdio agent server
// (docs/machine-readable-output-design.md §5/§7). Distinct from the
// existing, unrelated serverCmd (`milk server start|stop|status`, which
// manages the local inference server process).
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run milk as a long-lived agent server",
	RunE:  runServe,
}

func init() {
	serveCmd.Flags().BoolVar(&flagServeACP, "acp", false, "Speak ACP v2 (JSON-RPC 2.0) over stdio")
}

func runServe(cmd *cobra.Command, args []string) error {
	if !flagServeACP {
		return fmt.Errorf("milk serve requires --acp (no other transport is implemented yet)")
	}

	cfg, err := config.LoadMerged()
	if err != nil {
		var recovered *config.ErrConfigRecovered
		if !errors.As(err, &recovered) {
			return fmt.Errorf("loading config: %w", err)
		}
		fmt.Fprintf(os.Stderr, "%s warning: %s\n", milkTag(), recovered.Error())
	}

	obsShutdown := initObs(cfg)
	defer obsShutdown(context.Background()) //nolint:errcheck

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var conn *acp.StdioConn
	server := newACPServer(cfg, connFunc(func() acp.Conn { return conn }))
	conn = acp.NewStdioConn(os.Stdout, server)

	err = conn.Serve(ctx, os.Stdin)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// connFunc lets acpServer hold a Conn before the StdioConn it wraps exists
// yet (acpServer needs a Conn at construction to hand to each session;
// StdioConn needs a Handler, i.e. the server, at construction) — a trivial
// indirection breaking that construction cycle.
type connFunc func() acp.Conn

func (f connFunc) Notify(method string, params any) error { return f().Notify(method, params) }
func (f connFunc) Request(ctx context.Context, method string, params any, result any) error {
	return f().Request(ctx, method, params, result)
}

var _ acp.Conn = connFunc(nil)
