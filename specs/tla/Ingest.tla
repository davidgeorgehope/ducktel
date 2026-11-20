---- MODULE Ingest ----
\* Flush commit for one signal directory.
\*
\* Code:
\*   Accept is Writer.Add (writer.go). OTLP 200 is sent after Add returns
\*   (receiver.go handleTraces) and is not a durability ack. A crash may
\*   drop mem. The durability ack is Flush returning nil, including
\*   POST /flush (receiver.go handleFlush).
\*   StartFlush takes the buffer under w.mu and drops the lock before the
\*   write (Writer.Flush). FlushSerial is the flushMu around that whole
\*   call. Without it, a second Flush sees an empty buffer and returns nil
\*   while the first call has not renamed yet (FalseAck).
\*   CommitLocked is commitTemp: nextPath and os.Rename under the per-
\*   directory lock (writer.go). NameLock is FALSE when those two calls
\*   are not in the same critical section.
\*   FailWrite is the requeue path. It runs only after the write returns
\*   an error, which is before rename.
\*   Crash drops the buffer and unrenamed temp files. selectFiles skips
\*   names that start with "." (engine.go), so a temp is not in files.

EXTENDS Integers, FiniteSets, TLC

CONSTANTS NameLock, RequeuePublished, FlushSerial

Batches == {"b1", "b2"}
Slots == {1, 2}
Names == {"n0", "n1"}

VARIABLES mem, phase, batch, name, files, ok

vars == <<mem, phase, batch, name, files, ok>>

None == "none"
Idle == "idle"
Writing == "writing"
Named == "named"
DonePh == "done"

Init ==
  /\ mem = {}
  /\ phase = [s \in Slots |-> Idle]
  /\ batch = [s \in Slots |-> None]
  /\ name = [s \in Slots |-> None]
  /\ files = [n \in Names |-> None]
  /\ ok = {}

Placed(b) ==
  \/ b \in mem
  \/ b \in ok
  \/ \E s \in Slots : batch[s] = b

Accept(b) ==
  /\ b \in Batches
  /\ ~Placed(b)
  /\ mem' = mem \union {b}
  /\ UNCHANGED <<phase, batch, name, files, ok>>

StartFlush(s) ==
  /\ phase[s] = Idle
  /\ \E b \in mem :
       /\ mem' = mem \ {b}
       /\ phase' = [phase EXCEPT ![s] = Writing]
       /\ batch' = [batch EXCEPT ![s] = b]
       /\ UNCHANGED <<name, files, ok>>

CommitLocked(s) ==
  /\ NameLock
  /\ phase[s] = Writing
  /\ \E n \in Names :
       /\ files[n] = None
       /\ \A t \in Slots : name[t] # n
       /\ files' = [files EXCEPT ![n] = batch[s]]
       /\ name' = [name EXCEPT ![s] = n]
       /\ phase' = [phase EXCEPT ![s] = DonePh]
       /\ ok' = ok \union {batch[s]}
       /\ UNCHANGED <<mem, batch>>

\* nextPath only stats files that already exist. A sibling still between
\* stat and rename is invisible, so both can take the same name.
Pick(s) ==
  /\ ~NameLock
  /\ phase[s] = Writing
  /\ \E n \in Names :
       /\ files[n] = None
       /\ name' = [name EXCEPT ![s] = n]
       /\ phase' = [phase EXCEPT ![s] = Named]
       /\ UNCHANGED <<mem, batch, files, ok>>

\* os.Rename replaces an existing file and both Flush calls return nil.
Rename(s) ==
  /\ ~NameLock
  /\ phase[s] = Named
  /\ files' = [files EXCEPT ![name[s]] = batch[s]]
  /\ phase' = [phase EXCEPT ![s] = DonePh]
  /\ ok' = ok \union {batch[s]}
  /\ UNCHANGED <<mem, batch, name>>

FailWrite(s) ==
  /\ phase[s] = Writing
  /\ mem' = mem \union {batch[s]}
  /\ phase' = [phase EXCEPT ![s] = Idle]
  /\ batch' = [batch EXCEPT ![s] = None]
  /\ name' = [name EXCEPT ![s] = None]
  /\ UNCHANGED <<files, ok>>

\* POST /flush returns 200 for a snapshot another Flush still holds.
FalseAck(s) ==
  /\ ~FlushSerial
  /\ phase[s] \in {Writing, Named}
  /\ batch[s] \in Batches
  /\ batch[s] \notin ok
  /\ ok' = ok \union {batch[s]}
  /\ UNCHANGED <<mem, phase, batch, name, files>>

RequeueDone(s) ==
  /\ RequeuePublished
  /\ phase[s] = DonePh
  /\ mem' = mem \union {batch[s]}
  /\ UNCHANGED <<phase, batch, name, files, ok>>

Crash ==
  /\ mem' = {}
  /\ phase' = [s \in Slots |-> IF phase[s] = DonePh THEN DonePh ELSE Idle]
  /\ batch' = [s \in Slots |-> IF phase[s] = DonePh THEN batch[s] ELSE None]
  /\ name' = [s \in Slots |-> IF phase[s] = DonePh THEN name[s] ELSE None]
  /\ UNCHANGED <<files, ok>>

AtRest ==
  /\ mem = {}
  /\ \A s \in Slots : phase[s] \in {Idle, DonePh}
  /\ \A b \in Batches : Placed(b)

Done ==
  /\ AtRest
  /\ UNCHANGED vars

Next ==
  \/ \E b \in Batches : Accept(b)
  \/ \E s \in Slots : StartFlush(s)
  \/ \E s \in Slots : CommitLocked(s)
  \/ \E s \in Slots : Pick(s)
  \/ \E s \in Slots : Rename(s)
  \/ \E s \in Slots : FailWrite(s)
  \/ \E s \in Slots : RequeueDone(s)
  \/ \E s \in Slots : FalseAck(s)
  \/ Crash
  \/ Done

Spec == Init /\ [][Next]_vars

\* A batch whose Flush returned nil is stored under exactly one name.
NoLostCommit ==
  \A b \in ok : Cardinality({n \in Names : files[n] = b}) = 1

\* requeue must not run after the file is published.
NoRequeueDup ==
  \A b \in mem : \A n \in Names : files[n] # b

====
