---- MODULE DayMerge ----
\* What compactOldDays does with an hour merge that died after rename.
\*
\* Code: dropMergedHourSources in compaction.go. compactHours only visits
\* today and yesterday. A crash after HH.parquet is renamed, followed by a
\* restart once that day is older than yesterday, reaches compactDayDir
\* with the hour file and its flush files both present. DropLeftovers is
\* FALSE when those flush files are merged into day.parquet.

EXTENDS Integers, TLC

CONSTANT DropLeftovers

VARIABLES hour, leftover, day

vars == <<hour, leftover, day>>

Init ==
  /\ hour = 0
  /\ leftover = 0
  /\ day = 0

\* The hour file holds the row, and the flush file still holds it.
CrashHour ==
  /\ hour = 0
  /\ hour' = 1
  /\ leftover' = 1
  /\ UNCHANGED day

DayMerge ==
  /\ hour = 1
  /\ day = 0
  /\ IF DropLeftovers
     THEN /\ day' = hour
          /\ leftover' = 0
     ELSE /\ day' = hour + leftover
          /\ UNCHANGED leftover
  /\ UNCHANGED hour

Done ==
  /\ day > 0
  /\ UNCHANGED vars

Next ==
  \/ CrashHour
  \/ DayMerge
  \/ Done

Spec == Init /\ [][Next]_vars

\* day.parquet holds each row once.
NoDouble == day <= 1

====
