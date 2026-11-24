package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/davidgeorgehope/ducktel/internal/feedback"
	"github.com/davidgeorgehope/ducktel/internal/httpauth"
	"github.com/davidgeorgehope/ducktel/internal/mcp"
	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/telemetry"
	"github.com/davidgeorgehope/ducktel/internal/webapi"
	"github.com/davidgeorgehope/ducktel/web"
)

func mcpCmd() *cobra.Command {
	var (
		httpMode            bool
		host                string
		port                int
		token               string
		basePath            string
		flushURL            string
		dashboard           bool
		dashURL             string
		feedbackURL         string
		feedbackToken       string
		webhookURL          string
		githubRepo          string
		githubToken         string
		githubLabel         string
		feedbackGlobalLimit int
	)

	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve generic telemetry queries over the Model Context Protocol",
		Long: `Serve ducktel's stored telemetry as MCP tools.

The tools are generic and domain-agnostic — they know about spans, attributes,
metrics and time, and nothing about what any attribute means:

  list_tenants()                          tenant ids with stored telemetry
  list_services(tenant)                   service names for one tenant
  list_metric_names(tenant)               metric names for one tenant
  list_attribute_keys(tenant)             attribute keys in a recent sample
  trace_lookup(trace_id, tenant)          one trace, summary by default
  span_search(filters, tenant)            spans matching attribute filters
  compare_spans(tenant)                   values that differ on slow spans
  log_search(tenant)                      log records matching filters
  slow_requests(tenant)                   slowest requests, with a short summary
  metric_query(name, aggregation, tenant) one metric aggregated over a range
  list_saved_queries(tenant)              saved MCP queries for one tenant
  save_query(tenant, name, tool, ...)     save a tool invocation after validation
  run_saved_query(tenant, name)           run a saved query; optional window override
  sql_query(sql, tenant)                  one read-only SELECT or WITH on spans, logs, and metrics
  submit_feedback(tenant, ...)            report a confusing, wrong, or missing tool
  flush_buffer()                          make just-received telemetry queryable now

Fifteen tools are always registered. flush_buffer is advertised only when
--flush-url is set. sql_query is always registered. submit_feedback is
always registered. save_query will not store it.

list_tenants takes no arguments and returns only tenant ids. Every other tool
except flush_buffer requires a tenant and hard-filters on the tenant.id
resource attribute, so a caller cannot widen its scope. flush_buffer is
advertised only when --flush-url is set. sql_query is always registered.
It reads only that tenant's spans, logs, and metrics. ducktel query is not
sandboxed. list_attribute_keys samples the last 60 minutes, at most 2000
rows per signal, not every key ever stored.

submit_feedback stores one log for that tenant under service
ducktel.agent_feedback. log_search and the logs dashboard show it. Nothing is
sent off the machine unless a sink is configured. A sink is off by default.
Turning one on sends the agent-written text off the machine. ducktel does not scrub that text.
The in-memory rate limit is 20 accepted reports per tenant
per rolling hour, and --feedback-global-limit (default 30) across all
tenants. A restart clears both windows. POST /feedback flushes
the log before it returns success. A flush error still keeps the
report_id, so a retry does not write a second log.

Point this process at the running receiver instead of starting a second
ducktel serve on the same data directory. A second serve exits because the
directory is locked, and it will not store the report:

  --feedback-url http://127.0.0.1:4318/feedback

With no --feedback-url, this process writes the data directory itself. Set at
most one sink. Setting both fails at startup:
  --feedback-webhook-url
  --feedback-github-repo with --feedback-github-token
  --feedback-github-label (default feedback) is applied to each issue and must already exist on the repo.

Saved queries are per tenant. save_query checks the tool name and arguments
before writing. run_saved_query can override since_minutes and until_minutes
for that run only without changing what is stored.

Two transports:
  stdio (default)   spawned as a subprocess; the process boundary is the trust
                    boundary, so no token is needed.
  --http            listens on the network for remote consumers. This is a
                    separate trust domain from OTLP ingest: the OTLP token
                    grants WRITE (held by senders), this token grants READ
                    (held by whoever consumes the data). Use a different value
                    for each so a compromised sender cannot read telemetry.

--flush-url points at the receiving process's POST /flush. It is called with
this server's own token, so the query side can request a flush without
holding the OTLP ingest token.

--feedback-url points at POST /feedback on that receiver. The bearer is
--feedback-token, else this server's token, else DUCKTEL_FEEDBACK_TOKEN, else
DUCKTEL_FLUSH_TOKEN. The route stores one feedback log. It does not accept
OTLP spans, metrics, or arbitrary logs.

--dashboard additionally serves a human-facing dashboard (static SPA at
/dashboard, JSON REST API at /api/*) on the same port, guarded by the same
--auth-token — this is a convenience view over the identical query core the
MCP tools use, not a second trust domain, so it reuses the one read token
rather than minting another secret. Browsers may send that token as HTTP
Basic (any username, password is the token); a 401 on /dashboard and /api
includes a Basic challenge so the browser will prompt. /mcp stays
bearer-only, so existing MCP clients are unchanged.

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

			if !cmd.Flags().Changed("dashboard-url") {
				if v := os.Getenv("DUCKTEL_DASHBOARD_URL"); v != "" {
					dashURL = v
				}
			}
			if !cmd.Flags().Changed("feedback-url") {
				if v := os.Getenv("DUCKTEL_FEEDBACK_URL"); v != "" {
					feedbackURL = v
				}
			}
			if !cmd.Flags().Changed("feedback-webhook-url") {
				if v := os.Getenv("DUCKTEL_FEEDBACK_WEBHOOK_URL"); v != "" {
					webhookURL = v
				}
			}
			if !cmd.Flags().Changed("feedback-github-repo") {
				if v := os.Getenv("DUCKTEL_FEEDBACK_GITHUB_REPO"); v != "" {
					githubRepo = v
				}
			}
			if !cmd.Flags().Changed("feedback-github-token") {
				if v := os.Getenv("DUCKTEL_FEEDBACK_GITHUB_TOKEN"); v != "" {
					githubToken = v
				}
			}
			if !cmd.Flags().Changed("feedback-github-label") {
				if v := os.Getenv("DUCKTEL_FEEDBACK_GITHUB_LABEL"); v != "" {
					githubLabel = v
				}
			}
			feedbackGlobalLimit, err := positiveIntFlag(cmd.Flags().Changed("feedback-global-limit"), feedbackGlobalLimit, "feedback-global-limit", "DUCKTEL_FEEDBACK_GLOBAL_LIMIT")
			if err != nil {
				return err
			}
			bearer := feedbackToken
			if bearer == "" {
				bearer = token
			}
			if bearer == "" {
				bearer = os.Getenv("DUCKTEL_FEEDBACK_TOKEN")
			}
			if bearer == "" {
				bearer = os.Getenv("DUCKTEL_FLUSH_TOKEN")
			}
			feedbackSvc, err := newFeedbackService(dataDir, feedbackURL, bearer, webhookURL, githubRepo, githubToken, githubLabel, feedbackGlobalLimit)
			if err != nil {
				return err
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
				s := mcp.NewServer(engine, dataDir, flushURL, token)
				s.SetDashboardURL(dashURL)
				s.SetFeedback(feedbackSvc)
				s.Register(srv)
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
	cmd.Flags().StringVar(&feedbackURL, "feedback-url", "", "POST feedback to the running receiver, e.g. http://127.0.0.1:4318/feedback. Unset writes this process's data directory (env: DUCKTEL_FEEDBACK_URL)")
	cmd.Flags().StringVar(&feedbackToken, "feedback-token", "", "Bearer token for --feedback-url. Falls back to the MCP auth token, then DUCKTEL_FEEDBACK_TOKEN, then DUCKTEL_FLUSH_TOKEN. Sent only when non-empty")
	cmd.Flags().StringVar(&webhookURL, "feedback-webhook-url", "", "POST each stored report here. Off unless set. Sends the agent-written text off the machine; ducktel does not scrub it (env: DUCKTEL_FEEDBACK_WEBHOOK_URL)")
	cmd.Flags().StringVar(&githubRepo, "feedback-github-repo", "", "GitHub owner/repo that receives one issue per stored report. Off unless set. Mutually exclusive with --feedback-webhook-url (env: DUCKTEL_FEEDBACK_GITHUB_REPO)")
	cmd.Flags().StringVar(&githubToken, "feedback-github-token", "", "Bearer token for --feedback-github-repo (env: DUCKTEL_FEEDBACK_GITHUB_TOKEN)")
	cmd.Flags().StringVar(&githubLabel, "feedback-github-label", "feedback", "Label on each GitHub issue created for a stored report. The label must already exist on the repo (env: DUCKTEL_FEEDBACK_GITHUB_LABEL)")
	cmd.Flags().IntVar(&feedbackGlobalLimit, "feedback-global-limit", 30, "Accepted feedback reports across all tenants per rolling hour (env: DUCKTEL_FEEDBACK_GLOBAL_LIMIT)")
	cmd.Flags().StringVar(&dashURL, "dashboard-url", "", "Public address of the dashboard, e.g. https://otel.example.com/dashboard/; query results then carry a dashboard_url that opens the same query. Discovery lists do not (env: DUCKTEL_DASHBOARD_URL)")
	cmd.Flags().BoolVar(&dashboard, "dashboard", false, "Also serve the dashboard SPA at /dashboard and its REST API at /api, guarded by the same --auth-token (Bearer or HTTP Basic)")

	return cmd
}

func newFeedbackService(dataDir, feedbackURL, bearer, webhookURL, githubRepo, githubToken, githubLabel string, globalPerHour int) (*feedback.Service, error) {
	opt := feedback.Options{
		Token:         bearer,
		WebhookURL:    webhookURL,
		GitHubRepo:    githubRepo,
		GitHubToken:   githubToken,
		GitHubLabel:   githubLabel,
		GlobalPerHour: globalPerHour,
	}
	if feedbackURL != "" {
		opt.Endpoint = feedbackURL
	} else {
		opt.DataDir = dataDir
	}
	return feedback.New(opt)
}

func positiveIntFlag(changed bool, value int, flagName, envName string) (int, error) {
	if !changed {
		if v := os.Getenv(envName); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("%s must be a positive integer", envName)
			}
			value = n
		}
	}
	if value <= 0 {
		return 0, fmt.Errorf("--%s must be greater than zero", flagName)
	}
	return value, nil
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
		// query logic, not a second trust domain. Browser() also accepts
		// HTTP Basic so a person can open /dashboard in a browser; /mcp
		// above stays Bearer-only.
		core := telemetry.NewCore(engine)
		flusher := telemetry.NewFlusher(flushURL, token)
		webapi.NewHandlers(core, flusher, token).Register(mux)
		mountDashboard(mux, token)
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

// mountDashboard serves the embedded SPA at /dashboard, behind the same
// read token as /api. A missing token is 401 with a Basic challenge so a
// browser prompts; curl -u 'ducktel:$TOKEN' and Authorization: Bearer both
// work.
func mountDashboard(mux *http.ServeMux, token string) {
	assets := httpauth.Browser(token, http.StripPrefix("/dashboard/", http.FileServerFS(webui.FS())))
	mux.Handle("GET /dashboard/", assets)
	mux.Handle("GET /dashboard", httpauth.Browser(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard/", http.StatusMovedPermanently)
	})))
}
