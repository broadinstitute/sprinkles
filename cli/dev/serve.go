package dev

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"cloud.google.com/go/firestore"
	"cloud.google.com/go/pubsub/v2"
	"github.com/urfave/cli"

	"github.com/broadinstitute/sprinkles/monitor"
)

// serveShutdownGrace is how long in-flight HTTP requests get to finish once
// shutdown starts.
const serveShutdownGrace = 10 * time.Second

// ServeCommand returns the top-level "serve" command, which runs the whole
// control plane — the monitor and the dashboard-backend (REST API + embedded
// UI) — in a single process. It lives in this package because it reuses the
// dashboard-backend and monitor wiring here, and is registered as a top-level
// command by cmd/sprinkles/main.go.
func ServeCommand() cli.Command {
	return cli.Command{
		Name:  "serve",
		Usage: "Run the control plane: the monitor and the dashboard-backend (REST API + UI) in one process",
		Flags: []cli.Flag{
			cli.StringFlag{Name: "project", Usage: "GCP project ID (required)"},
			cli.StringFlag{Name: "db", Value: defaultDB, Usage: "Firestore database"},
			cli.StringFlag{Name: "addr", Value: ":8080", Usage: "address for the dashboard-backend to listen on"},
			cli.StringFlag{Name: "prefix", Usage: "URL path prefix under which all routes are served, e.g. \"sprinkles\" serves everything under /sprinkles/... (default: none, serve at the root)"},
			cli.BoolFlag{Name: "no-monitor", Usage: "run only the dashboard-backend (REST API + UI), without the monitor"},
			cli.BoolFlag{Name: "verbose, v", Usage: "log a message at the start of every monitor poll"},
		},
		Action: runServe,
	}
}

// leaseRetryDelay is how long runMonitorWhenLeased waits after an unexpected
// error acquiring the lease before trying again.
const leaseRetryDelay = 30 * time.Second

// runMonitorWhenLeased runs the monitor, but only once it holds the monitor
// lease. If another monitor holds it, this waits (the dashboard keeps
// serving, and the monitor's error log says the monitor isn't running and
// why), and takes over if that monitor goes away. It returns when ctx ends or
// the monitor loop exits.
func runMonitorWhenLeased(ctx context.Context, m *monitor.Monitor) {
	for {
		err := m.AcquireLease(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		var held *monitor.LeaseHeldError
		if errors.As(err, &held) {
			// AcquireLease already reported who holds it; wait again.
			continue
		}
		m.ErrorLog().Add("monitor is not running: %v; retrying in %s", err, leaseRetryDelay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(leaseRetryDelay):
		}
	}
	m.RunMonitorLoop(ctx)
}

func runServe(c *cli.Context) error {
	project := c.String("project")
	if project == "" {
		return fmt.Errorf("--project is required")
	}
	db := c.String("db")
	addr := c.String("addr")
	prefix := normalizePrefix(c.String("prefix"))
	noMonitor := c.Bool("no-monitor")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("Connecting to project %s, database %s", project, db)
	fsClient, err := firestore.NewClientWithDatabase(ctx, project, db)
	if err != nil {
		return fmt.Errorf("creating firestore client: %w", err)
	}
	defer fsClient.Close()

	psClient, err := pubsub.NewClient(ctx, project)
	if err != nil {
		return fmt.Errorf("creating pubsub client: %w", err)
	}
	defer psClient.Close()

	// With --no-monitor there's no monitor in this process: its error log and
	// poller stats stay nil, which NewDashboardHandler handles.
	var (
		m           *monitor.Monitor
		errorLog    *monitor.ErrorLog
		pollerStats *monitor.PollerStats
	)
	if !noMonitor {
		// No --linger here: unlike "dev monitor", a server shouldn't shut itself
		// down when the queue goes quiet.
		var stopMonitor func()
		m, stopMonitor, err = newMonitor(ctx, project, db, psClient, c.Bool("verbose"), 0)
		if err != nil {
			return fmt.Errorf("starting monitor: %w", err)
		}
		defer stopMonitor()
		errorLog, pollerStats = m.ErrorLog(), m.PollerStats()
	}

	handler, err := NewDashboardHandler(ctx, project, fsClient, psClient, prefix, errorLog, pollerStats)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{Addr: addr, Handler: handler}

	// monitorDone stays nil with --no-monitor; a nil channel never becomes
	// ready, so the select below simply ignores it.
	var monitorDone chan struct{}
	if m != nil {
		monitorDone = make(chan struct{})
		go func() {
			defer close(monitorDone)
			runMonitorWhenLeased(ctx, m)
		}()
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("serve: dashboard-backend listening on %s", addr)
		err := httpSrv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serverErr <- err
	}()

	// Any one of these ending takes the whole process down: a signal, the
	// listener failing, or the monitor loop exiting (which it only does on a
	// fatal Pub/Sub error or a lost lease, since linger is off). Not getting
	// the monitor lease at startup does not: the dashboard stays up and the
	// monitor waits for the lease (see runMonitorWhenLeased).
	var runErr error
	select {
	case <-ctx.Done():
		log.Printf("serve: shutting down")
	case runErr = <-serverErr:
		if runErr != nil {
			runErr = fmt.Errorf("dashboard-backend: %w", runErr)
		}
	case <-monitorDone:
		log.Printf("serve: monitor loop exited; shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), serveShutdownGrace)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && runErr == nil {
		runErr = fmt.Errorf("shutting down dashboard-backend: %w", err)
	}

	// Cancel ctx (if a signal didn't already) so the monitor loop returns, and
	// wait for it before the deferred client Close calls run.
	stop()
	if monitorDone != nil {
		<-monitorDone
	}

	return runErr
}
