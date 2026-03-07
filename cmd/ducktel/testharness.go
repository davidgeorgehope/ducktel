package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/davidgeorgehope/ducktel/internal/testharness"
)

func testHarnessCmd() *cobra.Command {
	var (
		endpoint    string
		traceRate   float64
		metricsMs   int
		errorRate   float64
		duration    time.Duration
		scenarioStr string
		configFile  string
	)

	cmd := &cobra.Command{
		Use:   "testdata",
		Short: "Generate synthetic OTLP telemetry for testing ducktel",
		Long: `Runs a synthetic telemetry generator that sends realistic traces, logs,
and metrics to ducktel's OTLP receiver. Simulates an e-commerce microservice
topology with configurable failure scenarios.

The default topology includes: api-gateway, product-service, order-service,
payment-service, and notification-service with database dependencies.

Scenarios inject failures into specific services:
  --scenario "payment-service:error_rate:0.5"     50% errors on payment
  --scenario "product-service:latency_spike:5.0"   5x latency on product

Inspired by github.com/davidgeorgehope/otel-demo-gen.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var cfg testharness.Config

			if configFile != "" {
				data, err := os.ReadFile(configFile)
				if err != nil {
					return fmt.Errorf("reading config: %w", err)
				}
				if err := json.Unmarshal(data, &cfg); err != nil {
					return fmt.Errorf("parsing config: %w", err)
				}
			} else {
				cfg = testharness.DefaultConfig()
			}

			// CLI overrides
			if endpoint != "" {
				cfg.Endpoint = endpoint
			}
			if cmd.Flags().Changed("trace-rate") {
				cfg.TraceRate = traceRate
			}
			if cmd.Flags().Changed("metrics-ms") {
				cfg.MetricsMs = metricsMs
			}
			if cmd.Flags().Changed("error-rate") {
				cfg.ErrorRate = errorRate
			}

			// Parse scenarios
			if scenarioStr != "" {
				for _, s := range strings.Split(scenarioStr, ",") {
					parts := strings.SplitN(strings.TrimSpace(s), ":", 3)
					if len(parts) != 3 {
						return fmt.Errorf("invalid scenario format %q — use service:type:value", s)
					}
					sc := testharness.Scenario{
						Name:          parts[0] + "-" + parts[1],
						TargetService: parts[0],
						Type:          parts[1],
					}
					var val float64
					if _, err := fmt.Sscanf(parts[2], "%f", &val); err != nil {
						return fmt.Errorf("invalid scenario value %q: %w", parts[2], err)
					}
					switch parts[1] {
					case "error_rate":
						sc.ErrorRate = val
					case "latency_spike":
						sc.LatencyMultiple = val
					default:
						return fmt.Errorf("unknown scenario type %q — use error_rate or latency_spike", parts[1])
					}
					cfg.Scenarios = append(cfg.Scenarios, sc)
				}
			}

			h := testharness.New(cfg)
			h.Start()

			if duration > 0 {
				log.Printf("[testdata] running for %s", duration)
				select {
				case <-time.After(duration):
				case <-signalChan():
				}
			} else {
				log.Println("[testdata] running until interrupted (Ctrl+C)")
				<-signalChan()
			}

			h.Stop()
			return nil
		},
	}

	cmd.Flags().StringVar(&endpoint, "endpoint", "http://localhost:4318", "OTLP HTTP endpoint")
	cmd.Flags().Float64Var(&traceRate, "trace-rate", 2, "Traces per second")
	cmd.Flags().IntVar(&metricsMs, "metrics-ms", 10000, "Metrics send interval in milliseconds")
	cmd.Flags().Float64Var(&errorRate, "error-rate", 0.05, "Baseline error rate (0.0–1.0)")
	cmd.Flags().DurationVar(&duration, "duration", 0, "Run duration (e.g. 30s, 5m). 0 = until Ctrl+C")
	cmd.Flags().StringVar(&scenarioStr, "scenario", "", "Failure scenarios: service:type:value (comma-separated)")
	cmd.Flags().StringVar(&configFile, "config", "", "JSON config file for custom topology")

	return cmd
}

func signalChan() <-chan os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	return ch
}
