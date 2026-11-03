package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"orchestrator-mcp/internal/mcpserver"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("orchestrator-mcp-server", flag.ContinueOnError)
	flags.SetOutput(stderr)
	profile := flags.String("profile", "", "operator-selected JSON profile (required)")
	sourceSpec := flags.String("source-spec", "", "operator-selected fixed S2 binding JSON (optional)")
	routingPolicy := flags.String("routing-policy", "", "operator-selected fixed routing policy JSON (optional)")
	port := flags.Int("port", 8080, "127.0.0.1 TCP port; 0 selects a free local port")
	containerListen := flags.Bool("container-listen", false, "listen on 0.0.0.0 inside Docker only; publish host loopback only")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *profile == "" || *port < 0 || *port > 65535 {
		return errors.New("an explicit -profile and port 0..65535 are required; positional arguments are unsupported")
	}
	address := "127.0.0.1"
	if *containerListen {
		marker, err := os.Lstat("/.dockerenv")
		if err != nil || !marker.Mode().IsRegular() {
			return errors.New("-container-listen requires Docker; host publication must remain loopback-only")
		}
		address = "0.0.0.0"
	}
	handler, err := mcpserver.NewHandler(*profile, *sourceSpec, *routingPolicy)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(address, strconv.Itoa(*port)))
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 8 << 10}
	if _, err := fmt.Fprintf(stderr, "MCP endpoint: http://%s/mcp\n", listener.Addr()); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			server.Close()
			return err
		}
		err := <-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
