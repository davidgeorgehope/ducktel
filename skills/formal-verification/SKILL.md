---
name: ducktel-formal-verification
description: "Model-check ducktel flush commits, the compaction reader view, and tenant row filters. Use when changing Writer.Flush, writeParquet, commitTemp, compaction rename or delete, selectFiles, dropSuperseded, or tenantPredicate."
---

# Check the flush, the reader view, and the tenant filter

Four TLA+ specs cover the places a bug drops or duplicates telemetry. `make tla` runs TLC on them. Read every TLC result with the pstack `/statechart` skill. Its commands and paths change between releases, so copy none of them into this file or into `scripts/tla`.

## When a change needs a run

Run the check when the diff touches a covered function. A formatting-only diff does not.

- **Ingest.** `Writer.Add`, `Writer.Flush`, `requeue`, `writeParquet`, `commitTemp`, `nextPath` in `internal/writer/writer.go`. `handleFlush` and `handleTraces` in `internal/receiver/receiver.go`. `Flush` holds `flushMu` for the whole call.
- **Compact.** `writeMergedWith`, `compactDayDir`, `dropMergedHourSources`, `compactHourDir` in `internal/writer/compaction.go`. `selectFiles` and `dropSuperseded` in `internal/query/engine.go`.
- **DayMerge.** `dropMergedHourSources`. The spec is `specs/tla/DayMerge.tla`.
- **Tenant.** `tenantPredicate`, `SearchSpans`, `SearchLogs`, and `QueryMetric` in `internal/telemetry/core.go`.

`list_tenants` returns tenant ids. It is not a row query and it is not an action in the Tenant spec.

## Run

From the repo root:

```bash
make tla
```

That runs `scripts/tla/check.sh`. The script downloads the TLC 1.7.4 jar (`sha256 936a262061c914694dfd669a543be24573c45d5aa0ff20a8b96b23d01e050e88`) into `scripts/tla/.cache` and needs Java 21. The good configs must pass. These configs must fail on the named invariant:

- `IngestLost.cfg` violates `NoLostCommit`
- `IngestRequeue.cfg` violates `NoRequeueDup`
- `IngestFalseAck.cfg` violates `NoLostCommit`
- `CompactDouble.cfg` violates `NoDouble`
- `CompactLoss.cfg` violates `NoLoss`
- `DayMergeDouble.cfg` violates `NoDouble`
- `TenantSpans.cfg` violates `NoForeign`

A bug config that passes means the spec can no longer see that break. Fix the spec or the script. Keep the invariant.

## What each invariant says

- `NoLostCommit`. A batch whose `Flush` returned nil is in exactly one published file. OTLP HTTP 200 means the batch was accepted into the memory buffer (`Writer.Add`). A crash may drop that buffer. Durability is `Flush` returning nil. `POST /flush` returns 200 only after that.
- `NoRequeueDup`. `requeue` does not put a batch back into memory after its file was renamed into place.
- `NoDouble`. A reader does not open two copies of a row. `dropSuperseded` hides a source whose mtime is strictly older than `day.parquet` or its `HH.parquet`.
- `NoLoss`. A reader still opens a row that is in the merge, a source that is the only copy, or a file newer than the merge. Equal mtimes stay visible.
- `NoForeign`. `SearchSpans`, `SearchLogs`, and `QueryMetric` return only the tenant that was asked for. The predicate is `tenantPredicate`.

TLC checks deadlock on each run. Ingest and Compact end in a `Done` action that changes nothing. Tenant's queries stay enabled.

## Bounds

A pass covers the model at these bounds, not the whole process.

- Ingest has two batches and two flushes in one directory. `FlushSerial` is the `flushMu` on `Writer.Flush`. It leaves out `maxBuffered` drops, directory fsync after rename, and a flush that runs longer than `hourlyMargin` (2 hours) or the day margin.
- DayMerge is one crashed hour publish, then one `compactOldDays` pass. `DropLeftovers` is `dropMergedHourSources`.
- `ducktel query` and the other local CLI commands call `engine.Query` with the caller's SQL. They are not Core methods and they are not in the Tenant spec.
- Compact has two rows. It leaves out retention deletes. It also leaves out a flush that lands in a day `compactOldDays` has already merged. The day margin exists so that landing does not happen. `TestCompactDoesNotDeleteFlushLandingAfterMidnight` guards it in Go.
- Tenant has two rows and the three row-query actions. It does not parse SQL. A new query function is outside the spec until you add an action for it.

## Tell a bug from a modeling error

`/statechart` prints the shortest path to each broken invariant. Replay that path as steps a caller of this code can take. One spec step is one code step, and the spec header names the function.

When a step cannot happen in the code, the spec is wrong. Change the spec so the step matches the code. Keep the invariant.

When every step can happen, the code is wrong. The Go tests that replay the bugs these specs found are:

- `TestConcurrentFlushKeepsEverySpan` in `internal/writer/writer_test.go`
- `TestQuerySkipsRowsAlreadyInANewerMerge` and `TestQuerySkipsRowsAlreadyInANewerHourFile` in `internal/query/engine_test.go`
- `TestSelectFilesHidesSourceOlderThanMerge` and `TestSelectFilesKeepsSourceWhenMergeIsNotNewer` in `internal/query/selectfiles_test.go`

The bug configs are the broken variants of the code.

- `NameLock = FALSE` means `nextPath` and `os.Rename` are not in one critical section (`commitTemp`).
- `FlushSerial = FALSE` means a second `Flush` returns nil while the first call has not renamed.
- `DropLeftovers = FALSE` means `compactDayDir` merges flush files that already sit beside `HH.parquet`.
- `RequeuePublished = TRUE` means `requeue` runs after a successful rename.
- `HideOlder = FALSE` means `selectFiles` returns every `.parquet` name, including a source older than the merge.
- `HideNewer = TRUE` means a file that is not older than the merge is hidden.
- `Unfiltered = "spans"` means `SearchSpans` forgot `tenantPredicate`.

## When the covered code changes

1. Read the spec header and the diff.
2. Change the spec so each changed step still matches the code. Change an invariant only when the promise changed on purpose, and cite the test or the comment that says so.
3. Leave the good configs at the values that describe the code. `NameLock`, `FlushSerial`, `HideOlder`, and `DropLeftovers` stay `TRUE`. `RequeuePublished` and `HideNewer` stay `FALSE`. `Unfiltered` stays `"none"`.
4. Run `make tla`.
5. Run the Go tests named above.
6. Set the sync commit below to the commit you checked. Move it forward only after that run passed and the spec still describes the code.

Sync commit: `5b03382c4d038e8fa8ca1f4c210ab2a78089f128`

## Upkeep

A merge that edits covered code and skips this check leaves the spec describing code that may no longer exist. Use `/maintain-formal-verification-skill` to bring the specs back in line.
