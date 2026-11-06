# ducktel — OTLP receiver + Parquet storage + DuckDB query, one process.
#
# Multi-stage: full Go toolchain to build, distroless for runtime.
#
# Why not Alpine/scratch: go-duckdb ships a vendored libduckdb.a that is
# compiled against glibc and references glibc-only fortified symbols
# (__vsnprintf_chk, __memcpy_chk). musl has no such symbols, so the link fails
# outright — verified, not assumed. A static/scratch build is blocked for the
# same reason (libstdc++/libm/libresolv would need static counterparts).
#
# distroless/cc is the smallest base that satisfies this: glibc + libstdc++,
# no shell, no package manager, ~25 MB.
#
# Build:
#   podman build -t ducktel:local -f Containerfile .
#
# Run (memory cap bounds the process so a leak cannot take the host down):
#   podman run --rm -p 4318:4318 --memory=512m --memory-swap=512m \
#     -v ducktel-data:/data ducktel:local

# ---------- build ----------
FROM docker.io/library/golang:1.25-bookworm AS build

WORKDIR /src

# Deps layer cached separately so source edits don't refetch modules.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# go-duckdb vendors a prebuilt libduckdb.a for linux/amd64, so no download
# step is needed — just CGo and a C/C++ toolchain, present in golang:*.
ENV CGO_ENABLED=1
RUN go build -trimpath -ldflags="-s -w" -o /out/ducktel ./cmd/ducktel

# ---------- runtime ----------
# cc = glibc + libstdc++, needed by the CGo/DuckDB objects. No shell.
FROM gcr.io/distroless/cc-debian12:nonroot

COPY --from=build /out/ducktel /usr/local/bin/ducktel

# Data lives on a volume so Parquet survives container replacement.
VOLUME /data

EXPOSE 4318

# Exec form: PID 1 is the binary, so SIGTERM reaches it and the graceful
# shutdown path (receiver drain + final flush) runs. No shell wrapper.
#
# Binding 0.0.0.0 is correct in a container; the host controls exposure via
# port publishing, and k8s via Service/NetworkPolicy. Host/port are flags today
# — env-var parsing is the natural next step for k8s configmaps.
ENTRYPOINT ["/usr/local/bin/ducktel"]
CMD ["serve", "--host", "0.0.0.0", "--port", "4318", "--data-dir", "/data"]
