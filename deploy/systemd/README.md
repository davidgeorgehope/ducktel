# Running ducktel under systemd

These files run ducktel on one Linux host, bound to 127.0.0.1, with an
OpenTelemetry Collector feeding it host telemetry. They are what the
`hetzner-ash` deployment uses. No secrets live here.

| File | Installs to |
|---|---|
| `ducktel.slice` | `/etc/systemd/system/` |
| `ducktel-serve.service` | `/etc/systemd/system/` |
| `ducktel-mcp.service` | `/etc/systemd/system/` |
| `otelcol-config.yaml` | `/etc/otelcol-contrib/config.yaml` |
| `otelcol-contrib.override.conf` | `/etc/systemd/system/otelcol-contrib.service.d/ducktel.conf` |

## Layout

- `ducktel-serve` takes OTLP/HTTP on `127.0.0.1:4318` and writes Parquet to
  `/var/lib/ducktel`, keeping 14 days.
- `ducktel-mcp` serves MCP at `127.0.0.1:4319/mcp` and the dashboard at
  `/dashboard/`. `flush_buffer` and `submit_feedback` go to the receiver.
- `otelcol-contrib` (the upstream `.deb`) sends hostmetrics, docker stats,
  journald for the listed units, and the nginx log files to the receiver. It
  also accepts OTLP/HTTP on `127.0.0.1:4328` for local apps. Every resource
  gets `tenant.id=hetzner`, so MCP queries use `tenant: hetzner`.
- All three run in `ducktel.slice` (MemoryMax 2G), each with its own cap.
- Nothing listens off-host. `tailscale serve` exposes the two ports on the
  tailnet only:

```sh
tailscale serve --bg --https=8446 http://127.0.0.1:4319   # MCP + dashboard
tailscale serve --bg --https=8447 http://127.0.0.1:4318   # OTLP ingest
```

## Secrets

`/etc/ducktel/ducktel.env` (root, 0600):

```sh
DUCKTEL_AUTH_TOKEN=...    # OTLP senders
DUCKTEL_MCP_TOKEN=...     # MCP readers
DUCKTEL_FLUSH_TOKEN=...   # same value as the MCP token: POST /flush, POST /feedback
# GitHub feedback sink, off until a fine-grained token (Issues: write on
# this repo only) exists. Uncomment both, then restart ducktel-mcp.
#DUCKTEL_FEEDBACK_GITHUB_REPO=davidgeorgehope/ducktel
#DUCKTEL_FEEDBACK_GITHUB_TOKEN=
```

`/etc/ducktel/collector.env` (root, 0600) holds only `DUCKTEL_AUTH_TOKEN`.

## Install

```sh
useradd --system --home /var/lib/ducktel --shell /usr/sbin/nologin ducktel
install -m 0755 ducktel /usr/local/bin/ducktel
install -m 0644 ducktel.slice ducktel-serve.service ducktel-mcp.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now ducktel-serve ducktel-mcp
```

Build the binary where glibc is no newer than the host's, for example
`docker run --rm -v "$PWD":/src -w /src -e CGO_ENABLED=1 golang:1.26-bookworm go build -o ducktel ./cmd/ducktel`.

For a short burst of traces, run the built-in generator against the receiver:

```sh
set -a; . /etc/ducktel/ducktel.env; set +a
systemd-run --unit=ducktel-testdata --slice=ducktel.slice -E DUCKTEL_AUTH_TOKEN \
  /usr/local/bin/ducktel testdata --tenant hetzner --duration 90s
```
