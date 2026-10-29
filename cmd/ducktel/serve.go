package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/davidgeorgehope/ducktel/internal/receiver"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

func serveCmd() *cobra.Command {
	var (
		host          string
		port          int
		flushInterval time.Duration
		bufferSize    int
		retention     string
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
delay.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			retentionDur, err := writer.ParseRetention(retention)
			if err != nil {
				return err
			}
			if bufferSize <= 0 {
				return fmt.Errorf("--buffer-size must be greater than zero, got %d", bufferSize)
			}

			w := writer.New(dataDir, flushInterval, bufferSize)
			w.OnError(func(err error) {
				log.Printf("writer error: %v", err)
			})
			if retentionDur > 0 {
				w.SetRetention(retentionDur)
			}
			w.Start()

			// Log the effective configuration once at startup: without it the
			// log cannot be used to tell what a running instance was told to do.
			retentionState := "disabled (keep forever)"
			if retentionDur > 0 {
				retentionState = retentionDur.String()
			}
			log.Printf("starting: data-dir=%s flush-interval=%s buffer-size=%d retention=%s",
				dataDir, flushInterval, bufferSize, retentionState)

			r := receiver.New(host, port, w)

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

			errCh := make(chan error, 1)
			go func() {
				errCh <- r.Start()
			}()

			select {
			case err := <-errCh:
				if err := w.Stop(); err != nil {
					log.Printf("final flush failed: %v", err)
				}
				return err
			case <-sigCh:
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
		},
	}

	cmd.Flags().StringVar(&host, "host", "localhost", "Host/interface to bind (use 0.0.0.0 for all interfaces)")
	cmd.Flags().IntVar(&port, "port", 4318, "Port to listen on")
	cmd.Flags().DurationVar(&flushInterval, "flush-interval", 30*time.Second, "How often to flush buffered spans to disk")
	cmd.Flags().IntVar(&bufferSize, "buffer-size", 1000, "Records per signal that force a flush ahead of --flush-interval")
	cmd.Flags().StringVar(&retention, "retention", "", "Delete date partitions older than this window (e.g. 720h or 30d); empty keeps data forever")

	return cmd
}
