---- MODULE Compact ----
\* What a reader sees while compaction has published a merge and not yet
\* deleted the sources.
\*
\* Code:
\*   Publish is os.Rename of the temp onto day.parquet or HH.parquet
\*   (writeMergedWith in compaction.go). The sources are removed only
\*   after that rename (compactDayDir, compactHourDir).
\*   older is a per-flush or hour file whose mtime is before the merge.
\*   newer is a file written after the merge. dropSuperseded in engine.go
\*   hides a source only when the merge mtime is strictly greater.
\*   HideOlder is FALSE when selectFiles returns every parquet name.
\*   HideNewer is TRUE when a newer neighbour of the merge is hidden too.
\*   That would drop a flush compaction kept on purpose
\*   (TestCompactDoesNotDeleteFlushLandingAfterMidnight).

EXTENDS Integers, FiniteSets, TLC

CONSTANTS HideOlder, HideNewer

Rows == {"r1", "r2"}

VARIABLES older, newer, merged

vars == <<older, newer, merged>>

Init ==
  /\ older = {}
  /\ newer = {}
  /\ merged = {}

WriteBefore(r) ==
  /\ merged = {}
  /\ r \notin older
  /\ older' = older \union {r}
  /\ UNCHANGED <<newer, merged>>

Publish ==
  /\ merged = {}
  /\ older # {}
  /\ merged' = older
  /\ UNCHANGED <<older, newer>>

WriteAfter(r) ==
  /\ merged # {}
  /\ r \notin older
  /\ r \notin merged
  /\ r \notin newer
  /\ newer' = newer \union {r}
  /\ UNCHANGED <<older, merged>>

DeleteOlder ==
  /\ merged # {}
  /\ older # {}
  /\ older' = {}
  /\ UNCHANGED <<newer, merged>>

\* How many copies of r a reader opens. A temp name is never in this count.
Visible(r) ==
  LET inMerged == r \in merged
      inNewer == r \in newer /\ ~HideNewer
      inOlder ==
        /\ r \in older
        /\ \/ merged = {}
           \/ r \notin merged
           \/ ~HideOlder
  IN (IF inMerged THEN 1 ELSE 0)
     + (IF inNewer THEN 1 ELSE 0)
     + (IF inOlder THEN 1 ELSE 0)

AtRest ==
  /\ merged # {}
  /\ older = {}
  /\ \A r \in Rows : r \in merged \/ r \in newer

Done ==
  /\ AtRest
  /\ UNCHANGED vars

Next ==
  \/ \E r \in Rows : WriteBefore(r)
  \/ Publish
  \/ \E r \in Rows : WriteAfter(r)
  \/ DeleteOlder
  \/ Done

Spec == Init /\ [][Next]_vars

\* A reader does not open two copies of one row.
NoDouble ==
  \A r \in Rows : Visible(r) <= 1

\* A row that is the only copy, or a row in the merge, is still opened.
\* A file newer than the merge stays visible.
NoLoss ==
  \A r \in Rows :
    /\ (r \in merged => Visible(r) >= 1)
    /\ (r \in newer => Visible(r) >= 1)
    /\ ((r \in older /\ merged = {}) => Visible(r) >= 1)

====
