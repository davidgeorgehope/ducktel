---- MODULE Tenant ----
\* Row queries keep a tenant's rows out of another tenant's result.
\*
\* Code:
\*   tenantPredicate in internal/telemetry/core.go is
\*   json_extract_string(resource_attributes, '$."tenant.id"') = ?
\*   SearchSpans, SearchLogs, and QueryMetric each append it.
\*   The tenant argument is a bound parameter, not pasted into the SQL.
\*   list_tenants returns tenant ids and is not a row query. It is not
\*   an action here.
\*   Unfiltered names the one signal whose action forgot the predicate.
\*   "none" means every row query filters.

EXTENDS FiniteSets, TLC, Sequences

CONSTANT Unfiltered

Tenants == {"acme", "globex"}
Signals == {"spans", "logs", "metrics"}

r1 == <<"s1", "acme">>
r2 == <<"s2", "globex">>

VARIABLES asked, returned

vars == <<asked, returned>>

stored == {r1, r2}

Init ==
  /\ asked = "none"
  /\ returned = {}

Matches(signal, t, row) ==
  IF signal = Unfiltered THEN TRUE ELSE row[2] = t

Query(signal, t) ==
  /\ t \in Tenants
  /\ asked' = <<signal, t>>
  /\ returned' = {row \in stored : Matches(signal, t, row)}

SearchSpans(t) == Query("spans", t)
SearchLogs(t) == Query("logs", t)
QueryMetric(t) == Query("metrics", t)

Next ==
  \/ \E t \in Tenants : SearchSpans(t)
  \/ \E t \in Tenants : SearchLogs(t)
  \/ \E t \in Tenants : QueryMetric(t)

Spec == Init /\ [][Next]_vars

\* A row result contains only the tenant that was asked for.
NoForeign ==
  \/ asked = "none"
  \/ \A row \in returned : row[2] = asked[2]

====
