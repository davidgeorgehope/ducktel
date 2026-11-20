#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
cache=$root/scripts/tla/.cache
jar=$cache/tla2tools.jar
url=https://github.com/tlaplus/tlaplus/releases/download/v1.7.4/tla2tools.jar
sha=936a262061c914694dfd669a543be24573c45d5aa0ff20a8b96b23d01e050e88
specdir=$root/specs/tla

if ! command -v java >/dev/null 2>&1; then
  echo "java is required (Java 21 is what CI uses)" >&2
  exit 1
fi

mkdir -p "$cache"
if [ ! -f "$jar" ]; then
  tmp=$jar.partial
  curl -fsSL -o "$tmp" "$url"
  echo "$sha  $tmp" | sha256sum -c -
  mv "$tmp" "$jar"
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

run_tlc() {
  spec=$1
  cfg=$2
  out=$3
  # TLC writes its state directory in the working directory.
  (
    cd "$work"
    java -XX:+UseParallelGC -cp "$jar" tlc2.TLC \
      -workers 1 \
      -config "$specdir/$cfg" \
      "$specdir/$spec.tla"
  ) >"$out" 2>&1
}

plain() {
  sed 's/\x1b\[[0-9;]*m//g' "$1"
}

pass() {
  spec=$1
  cfg=$2
  out=$work/$cfg.out
  echo "pass $cfg"
  if ! run_tlc "$spec" "$cfg" "$out"; then
    plain "$out" >&2
    echo "expected $cfg to pass" >&2
    exit 1
  fi
}

fail() {
  spec=$1
  cfg=$2
  inv=$3
  out=$work/$cfg.out
  echo "fail $cfg on $inv"
  if run_tlc "$spec" "$cfg" "$out"; then
    plain "$out" >&2
    echo "expected $cfg to violate $inv" >&2
    exit 1
  fi
  if ! plain "$out" | grep -q "Invariant $inv is violated"; then
    plain "$out" >&2
    echo "expected $cfg to violate $inv" >&2
    exit 1
  fi
}

pass Ingest Ingest.cfg
fail Ingest IngestLost.cfg NoLostCommit
fail Ingest IngestRequeue.cfg NoRequeueDup
pass Compact Compact.cfg
fail Compact CompactDouble.cfg NoDouble
fail Compact CompactLoss.cfg NoLoss
pass Tenant Tenant.cfg
fail Tenant TenantSpans.cfg NoForeign

echo "tla checks passed"
