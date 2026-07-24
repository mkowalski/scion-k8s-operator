// Command registrar is the AS-side sigs registration service. It runs next
// to an open-source SCION control service (outside the cluster) and lets
// the scion-k8s-operator register per-node SIG entries in the AS
// topology.json, then reloads the control service.
//
// The SCION control service (v0.15.0) reloads its topology on SIGHUP
// (control/cmd/control/main.go wires app.SIGHUPChannel into
// topology.NewLoader; private/topology/reload.go re-reads the file on that
// channel), so the default reload command sends SIGHUP via systemd and
// causes no control-plane downtime.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mkowalski/scion-k8s-operator/internal/registrar"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("registrar failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	var (
		topology = flag.String("topology", "/etc/scion/topology.json", "path to the AS topology.json")
		prefix   = flag.String("prefix", "k8s-", "name prefix identifying operator-managed sigs entries")
		listen   = flag.String("listen", ":8642", "listen address")
		// SIGHUP triggers a topology reload in the control service
		// (scion v0.15.0 private/topology/reload.go); no restart needed.
		reloadCmd = flag.String("reload-cmd", "systemctl kill -s HUP scion-control",
			"command run after patching the topology; split on spaces "+
				"(no shell quoting supported)")
	)
	flag.Parse()

	token := os.Getenv("REGISTRAR_TOKEN")
	if token == "" {
		return errors.New("REGISTRAR_TOKEN must be set (fail-closed: no anonymous access)")
	}

	argv := strings.Fields(*reloadCmd)
	if len(argv) == 0 {
		return errors.New("--reload-cmd must not be empty")
	}

	srv := &registrar.Server{
		TopologyFile: *topology,
		Prefix:       *prefix,
		Token:        token,
		Log:          log,
		Reload: func() error {
			out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s: %w (output: %s)", *reloadCmd, err, out)
			}
			return nil
		},
	}

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("registrar listening", "addr", *listen, "topology", *topology, "prefix", *prefix)
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(sctx); err != nil {
		return err
	}
	log.Info("shutdown complete")
	return nil
}
