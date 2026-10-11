# 0018: Built-in observability (metrics, health, alerts)

Status: Accepted (v1: metrics and health endpoints; alerts and webhook are
planned, see "Not yet")

## Context
Operators need to see whether the control plane has a leader, whether
workloads are ready, and whether nodes are healthy, without installing a
separate monitoring stack. Shoal targets small machines, so the observability
code must cost little in memory and CPU.

## Decision
- **Metrics in Prometheus text format (0.0.4)** from a small in-tree package,
  `internal/metrics`. It provides counters, gauges, histograms and scrape-time
  collectors. Series per family are capped (`DefaultMaxSeries` = 200); samples
  beyond the cap are dropped and counted in
  `shoal_metrics_dropped_series_total`. Label values are never per-request
  IDs.
- **No client library.** `github.com/prometheus/client_golang` is not in
  `go.mod`. It would add a sizeable dependency tree (protobuf models, procfs,
  and more) for a few dozen series. The in-tree writer is about 300 lines and
  is checked by a strict parser in the tests.
- **Listeners are loopback by default**: the server serves on
  `127.0.0.1:9464` and the agent on `127.0.0.1:9465` (`--metrics-listen`;
  empty disables). Each serves `/metrics`, `/healthz` (process up) and
  `/readyz`. Scrapers on other hosts need the flag set explicitly.
- **Readiness.** Server: the store answers and, with raft, a leader is
  known. Agent: connected to the server.
- **Server series:** `shoal_api_requests_total{method,code}`,
  `shoal_api_request_duration_seconds{method}` (a unary interceptor, outermost
  so rejected calls count), `shoal_objects{kind}` (counted at scrape time),
  and with raft `shoal_raft_is_leader`, `shoal_raft_commit_index`,
  `shoal_raft_applied_index`, `shoal_raft_last_log_index`.
- **Agent series:** `shoal_agent_connected`, `shoal_replicas{state}`
  (desired, running, ready), `shoal_replica_restarts`,
  `shoal_node_memory_bytes{kind}` (Linux, from /proc/meminfo) and
  `shoal_node_disk_bytes{kind}` (the state directory's file system).
- Values that come from live state are read at scrape time, so no background
  goroutine keeps them up to date.

## Not yet
The alert evaluator (NodeNotReady, WorkloadDegraded, CertExpiringSoon,
CertError, BackupStale, DiskAlmostFull), the `shoal alerts` command, the
`--alert-webhook` option, controller reconcile and certificate and backup
metrics, and edge request metrics are not part of v1. They are planned on this
design, with alert state stored as `Alert` status records.

## Consequences
A scrape costs one allocation pass over the series, which the benchmark
`BenchmarkScrape` measures. Object counts call the store's List per kind on
each scrape, which is cheap at the scale Shoal targets, but a store with
thousands of objects would make scrapes more expensive. A scrape of an
unhealthy store is bounded by a 2 second timeout.
