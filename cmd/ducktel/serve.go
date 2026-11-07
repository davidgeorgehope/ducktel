package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/receiver"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// goMemLimitFraction is the share of the container memory limit given to the
// Go heap as a soft limit (see the GOMEMLIMIT note in serveCmd).
const goMemLimitFraction = 70

func serveCmd() *cobra.Command {
	var (
		host          string
		port          int
		flushInterval time.Duration
		bufferSize    int
		retention     string
		token         string
		flushToken    string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the OTLP receiver",
		Long: `Start the OTLP receiver.

--retention deletes whole date-partition directories older than the given
window (accepts Go durations like 720h, or a bare day count like 30d). Empty
(the default) disables it, so upgrading an existing deployment never starts
deleting data without an explicit opt-in.

--buffer-size is how many records of a signal may accumulate before a flush
is forced ahead of the --flush-interval timer; the smaller of the two wins.
Defaults to 1000, which keeps ingest→queryable latency low on a busy
instance; raise it to batch more per file at the cost of a longer worst-case
delay.

Authentication is disabled unless a token is configured. Set it with
--auth-token or DUCKTEL_AUTH_TOKEN; senders must then present it as
"Authorization: Bearer <token>", which every OTel exporter supports via
OTEL_EXPORTER_OTLP_HEADERS. /health stays unauthenticated for probes.

TLS is not terminated here — front the receiver with a proxy or ingress if the
token would cross an untrusted network.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Env wins only when the flag was not given, so an explicit flag
			// can always override the environment.
			if !cmd.Flags().Changed("auth-token") {
				if v := os.Getenv("DUCKTEL_AUTH_TOKEN"); v != "" {
					token = v
				}
			}
			// /flush is authorised by its own token, separate from the ingest
			// token, so the query side can request a flush without holding
			// write access.
			if !cmd.Flags().Changed("flush-token") {
				if v := os.Getenv("DUCKTEL_FLUSH_TOKEN"); v != "" {
					flushToken = v
				}
			}
			// Fall back to the MCP token: a query process configured with
			// only DUCKTEL_MCP_TOKEN can then request flushes from this one.
			if flushToken == "" {
				if v := os.Getenv("DUCKTEL_MCP_TOKEN"); v != "" {
					flushToken = v
				}
			}

			retentionDur, err := writer.ParseRetention(retention)
			if err != nil {
				return err
			}
			if bufferSize <= 0 {
				return fmt.Errorf("--buffer-size must be greater than zero, got %d", bufferSize)
			}

			// Go does not size its heap to the container: without a limit
			// the GC lets the heap grow to about twice the live set, which
			// compaction rewrites pushed past a 512Mi container limit (the
			// live set itself stays near 250MiB). An explicit GOMEMLIMIT in
			// the environment still wins.
			if os.Getenv("GOMEMLIMIT") == "" {
				if limit := query.CgroupMemoryLimit(); limit > 0 {
					debug.SetMemoryLimit(limit / 100 * goMemLimitFraction)
					log.Printf("go memory limit: %d MiB (%d%% of cgroup limit)", limit/100*goMemLimitFraction>>20, goMemLimitFraction)
				}
			}

			// One writer per data directory: refuse to start rather than
			// race another serve over the same files. Deferred, so it is
			// released only after the final flush in w.Stop below.
			release, err := writer.Lock(dataDir)
			if err != nil {
				return err
			}
			defer release()

			w := writer.New(dataDir, flushInterval, bufferSize)
			w.OnError(func(err error) {
				log.Printf("writer error: %v", err)
			})
			if retentionDur > 0 {
				w.SetRetention(retentionDur)
			}
			w.Start()

			authState := "disabled"
			if token != "" {
				authState = "enabled"
			}
			// Log the effective configuration once at startup: without it the
			// log cannot be used to tell what a running instance was told to do.
			retentionState := "disabled (keep forever)"
			if retentionDur > 0 {
				retentionState = retentionDur.String()
			}
			log.Printf("starting: data-dir=%s flush-interval=%s buffer-size=%d retention=%s auth=%s",
				dataDir, flushInterval, bufferSize, retentionState, authState)

			r := receiver.New(host, port, w, token, flushToken, w.Flush)

			sigCh := make(chan os.Signal, 1)
			// SIGHUP requests an immediate flush without shutting down, so
			// buffered telemetry can be made queryable on demand instead of
			// waiting out the flush interval. Intercepting it also stops Go's
			// default behaviour of terminating on SIGHUP.
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

			errCh := make(chan error, 1)
			go func() {
				errCh <- r.Start()
			}()

			for {
				select {
				case err := <-errCh:
					if err := w.Stop(); err != nil {
						log.Printf("final flush failed: %v", err)
					}
					return err
				case sig := <-sigCh:
					if sig == syscall.SIGHUP {
						// Flush in place: the receiver keeps serving and the
						// writer keeps its buffer, so nothing is dropped.
						log.Printf("received %s: flushing buffered telemetry", sig)
						if err := w.Flush(); err != nil {
							log.Printf("flush on %s failed: %v", sig, err)
						}
						continue
					}
					log.Println("Shutting down...")
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					r.Stop(ctx)
					if err := w.Stop(); err != nil {
						log.Printf("final flush failed: %v", err)
					}
					log.Println("Stopped.")
					return nil
				}
			}
		},
	}

	cmd.Flags().StringVar(&host, "host", "localhost", "Host/interface to bind (use 0.0.0.0 for all interfaces)")
	cmd.Flags().IntVar(&port, "port", 4318, "Port to listen on")
	cmd.Flags().DurationVar(&flushInterval, "flush-interval", 30*time.Second, "How often to flush buffered spans to disk")
	cmd.Flags().IntVar(&bufferSize, "buffer-size", 1000, "Records per signal that force a flush ahead of --flush-interval")
	cmd.Flags().StringVar(&retention, "retention", "", "Delete date partitions older than this window (e.g. 720h or 30d); empty keeps data forever")
	cmd.Flags().StringVar(&token, "auth-token", "", "Require this bearer token on OTLP endpoints (env: DUCKTEL_AUTH_TOKEN)")
	cmd.Flags().StringVar(&flushToken, "flush-token", "", "Require this bearer token on POST /flush (env: DUCKTEL_FLUSH_TOKEN, or DUCKTEL_MCP_TOKEN)")

	return cmd
}
