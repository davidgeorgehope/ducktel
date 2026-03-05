package main

import (
	"context"
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
		port          int
		flushInterval time.Duration
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the OTLP receiver",
		RunE: func(cmd *cobra.Command, args []string) error {
			w := writer.New(dataDir, flushInterval, 1000)
			w.Start()

			r := receiver.New(port, w)

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

			errCh := make(chan error, 1)
			go func() {
				errCh <- r.Start()
			}()

			select {
			case err := <-errCh:
				w.Stop()
				return err
			case <-sigCh:
				log.Println("Shutting down...")
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				r.Stop(ctx)
				w.Stop()
				log.Println("Stopped.")
				return nil
			}
		},
	}

	cmd.Flags().IntVar(&port, "port", 4318, "Port to listen on")
	cmd.Flags().DurationVar(&flushInterval, "flush-interval", 30*time.Second, "How often to flush buffered spans to disk")

	return cmd
}
