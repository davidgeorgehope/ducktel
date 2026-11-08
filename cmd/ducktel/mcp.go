package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/davidgeorgehope/ducktel/internal/httpauth"
	"github.com/davidgeorgehope/ducktel/internal/mcp"
	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/telemetry"
	"github.com/davidgeorgehope/ducktel/internal/webapi"
	"github.com/davidgeorgehope/ducktel/web"
)

func mcpCmd() *cobra.Command {
	var (
		httpMode  bool
		host      string
		port      int
		token     string
		basePath  string
		flushURL  string
		dashboard bool
	)

	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve generic telemetry queries over the Model Context Protocol",
		Long: `Serve ducktel's stored telemetry as MCP tools.

The tools are generic and domain-agnostic — they know about spans, attributes,
metrics and time, and nothing about what any attribute means:

  trace_lookup(trace_id)                all spans of one trace
  span_search(filters, time_range)      spans matching arbitrary attribute filters
  log_search(severity, search, ...)     log records matching body/severity/attribute filters
  metric_query(name, aggregation, ...)  metric aggregation over a range
  flush_buffer()                        make just-received telemetry queryable now

Every tool requires a tenant and hard-filters on the tenant.id resource
attribute, so a caller cannot widen its scope.

Two transports:
  stdio (default)   spawned as a subprocess; the process boundary is the trust
                    boundary, so no token is needed.
  --http            listens on the network for remote consumers. This is a
                    separate trust domain from OTLP ingest: the OTLP token
                    grants WRITE (held by senders), this token grants READ
                    (held by whoever consumes the data). Use a different value
                    for each so a compromised sender cannot read telemetry.

--flush-url points at the receiving process's POST /flush. It is called with
this server's own token, so the query side can request a flush without ever
holding write access.

--dashboard additionally serves a human-facing dashboard (static SPA at
/dashboard, JSON REST API at /api/*) on the same port, guarded by the same
--auth-token — this is a convenience view over the identical query core the
MCP tools use, not a second trust domain, so it reuses the one read token
rather than minting another secret.

Examples:
  ducktel mcp --data-dir /data
  ducktel mcp --http --host 0.0.0.0 --port 4319 --auth-token "$READ_TOKEN" \
    --flush-url http://localhost:4318/flush --dashboard`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Env wins only when the flag was not given, so an explicit flag
			// can always override the environment.
			if !cmd.Flags().Changed("auth-token") {
				if v := os.Getenv("DUCKTEL_MCP_TOKEN"); v != "" {
					token = v
				}
			}
			if !cmd.Flags().Changed("flush-url") {
				if v := os.Getenv("DUCKTEL_FLUSH_URL"); v != "" {
					flushURL = v
				}
			}

			engine, err := query.Open(dataDir)
			if err != nil {
				return fmt.Errorf("opening query engine: %w", err)
			}
			defer engine.Close()

			newServer := func() *sdkmcp.Server {
				srv := sdkmcp.NewServer(&sdkmcp.Implementation{
					Name:    "ducktel",
					Version: "0.1.0",
				}, nil)
				// The flush request is authorised with this server's own token.
				mcp.NewServer(engine, flushURL, token).Register(srv)
				return srv
			}

			if !httpMode {
				// stdio: a token adds nothing here, since anything able to
				// spawn this process can already read the --data-dir directly.
				if token != "" {
					log.Printf("warning: --auth-token has no effect on stdio transport")
				}
				if err := newServer().Run(context.Background(), &sdkmcp.StdioTransport{}); err != nil {
					return fmt.Errorf("mcp server: %w", err)
				}
				return nil
			}
			return serveMCPHTTP(newServer, engine, host, port, token, basePath, flushURL, dashboard)
		},
	}

	cmd.Flags().BoolVar(&httpMode, "http", false, "Serve over HTTP for remote consumers, requiring --auth-token")
	cmd.Flags().StringVar(&host, "host", "localhost", "Host/interface to bind in HTTP mode (use 0.0.0.0 for all interfaces)")
	cmd.Flags().IntVar(&port, "port", 4319, "Port to listen on in HTTP mode")
	cmd.Flags().StringVar(&token, "auth-token", "", "Require this bearer token in HTTP mode (env: DUCKTEL_MCP_TOKEN)")
	cmd.Flags().StringVar(&basePath, "base-path", "/mcp", "URL path to serve the MCP endpoint on in HTTP mode")
	cmd.Flags().StringVar(&flushURL, "flush-url", "", "Writer's POST /flush endpoint, e.g. http://localhost:4318/flush; enables the flush_buffer tool (env: DUCKTEL_FLUSH_URL)")
	cmd.Flags().BoolVar(&dashboard, "dashboard", false, "Also serve the dashboard SPA at /dashboard and its REST API at /api, guarded by the same --auth-token")

	return cmd
}

// serveMCPHTTP runs the MCP server over Streamable HTTP with a bearer-token
// check in front of it. When dashboard is set, it also mounts the dashboard
// SPA and its REST API on the same mux, same port, same token — see mcpCmd's
// --dashboard help text for why that's one trust domain, not two.
func serveMCPHTTP(
	newServer func() *sdkmcp.Server,
	engine *query.Engine,
	host string,
	port int,
	token string,
	basePath string,
	flushURL string,
	dashboard bool,
) error {
	if token == "" {
		// Refuse rather than silently expose everything: over the network this
		// endpoint reads every tenant, and an unauthenticated bind is almost
		// certainly a misconfiguration rather than an intent.
		return errors.New("--http requires --auth-token (or DUCKTEL_MCP_TOKEN): " +
			"an unauthenticated MCP endpoint exposes all tenant data")
	}

	handler := sdkmcp.NewStreamableHTTPHandler(
		func(*http.Request) *sdkmcp.Server { return newServer() }, nil)

	mux := http.NewServeMux()
	mux.Handle(basePath, httpauth.Bearer(token, handler))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	if dashboard {
		// Same Core the MCP tools query, same Flusher the flush_buffer tool
		// calls, same token — this is a second transport onto identical
		// query logic, not a second trust domain.
		core := telemetry.NewCore(engine)
		flusher := telemetry.NewFlusher(flushURL, token)
		webapi.NewHandlers(core, flusher, token).Register(mux)

		// Static assets are unauthenticated: they're app shell with no data
		// or secrets embedded in them. Only the /api/* calls the SPA makes
		// carry the token, checked above.
		assets := http.FileServerFS(webui.FS())
		mux.Handle("GET /dashboard/", http.StripPrefix("/dashboard/", assets))
		mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/dashboard/", http.StatusMovedPermanently)
		})
	}

	addr := fmt.Sprintf("%s:%d", host, port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("mcp: listening on %s%s (auth enabled, dashboard=%v)", addr, basePath, dashboard)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("mcp http server: %w", err)
		}
	case <-sigCh:
		log.Println("mcp: shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
	}
	return nil
}
