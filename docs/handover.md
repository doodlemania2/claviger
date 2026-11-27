# Single-writer handover

SQLite allows one writer, but container platforms (Azure Container Apps, Kubernetes, ECS) start the incoming replica before they stop the outgoing one. Two processes writing one database on shared storage corrupt it (#35). Claviger carries a handover protocol for this. It exists in two forms that speak the same protocol on the same files, so a rollout can mix them:

- the shell form in `entrypoint.sh` (the default today), and
- the in-binary form in `tools/handover` (#72), which runs inside `claviger serve`.

Status: the binary form is **opt-in and default off**. Plain `serve` behaves exactly as before. The shell form stays the default until the binary form has run in production; deleting the shell block from `entrypoint.sh` is the follow-up that closes #72.

## Turning it on

Either of:

```
claviger serve --handover
CLAVIGER_HANDOVER=binary          # same effect, for any `serve`
```

With the container image, set `CLAVIGER_HANDOVER=binary`: `entrypoint.sh` then skips its own handover and passes `--handover`. Two things stay in the shell form on purpose:

- With `LITESTREAM_REPLICA_URL` set, `CLAVIGER_HANDOVER=binary` is ignored (with a WARN) and the shell handover runs. Litestream's restore and replicate steps live in the entrypoint and must come after the handover.
- The superuser bootstrap (`PB_ADMIN_EMAIL`) cannot run before the binary owns the volume, so in binary mode it runs once `/api/health` answers (needs `wget` or `curl`; without both it is skipped with a WARN).

## The protocol

Files in the data directory, identical to the shell form:

| File | Meaning |
|---|---|
| `.pb_singlewriter.lock` | `flock`, held for the life of the serving process |
| `.pb_handover` | incoming to outgoing: "please let go", carrying the requester's id |
| `.pb_handover_ack` | outgoing to incoming: "I let go", carrying the id it let go for |
| `.pb_owner` | the id of the replica that owns the volume |
| `.pb_released` | ids that handed over and must never serve again (last 50) |
| `.pb_handover.log` | durable event log (last 500 lines): `<utc time> <id> <LEVEL> <message>` |

1. **Incoming** (before any database opens: the check runs in an `OnBootstrap` hook ahead of `data.db`): writes its id to `.pb_handover`, then polls every second, never blocking, for the lock **and** its own id in the ack. The ack is the proof, not the lock: on NFS a `flock` release can be missed for tens of seconds (#54).
2. **Outgoing** (a watcher started when the server starts): sees a request that is not its own and not stale, shuts the HTTP server down, closes the databases (`ClearBootstrap`, which checkpoints the WAL), drops the lock, records itself in `.pb_released`, **then** acks, then parks.
3. **Parking** keeps the process alive without serving, until the platform sends SIGTERM and it exits 0. Exiting would look like a crash and the platform would restart it into a race for the volume. Health probes fail while parked; if liveness restarts the container anyway, it finds its id in `.pb_released` and parks again before opening a database.
4. A request is stale (ignored with a WARN) when it carries this replica's own id, an id in `.pb_released`, or an older azd revision epoch.

It **fails open**. If the wait expires the incoming replica starts anyway with a WARN (`no handover after Ns` when the lock is still held, `never acknowledged` when the lock is free but no ack came). A brief overlap is recoverable; an app that can no longer be deployed is not. Never replace the wait with a blocking startup lock: readiness gates the drain, so a blocked incoming replica deadlocks the rollout (tried and reverted 2026-09-06).

A fresh volume (no lock file) and a replica restarting as the recorded owner do not wait for an ack.

## Configuration

The same variables `entrypoint.sh` reads:

| Variable | Default | Meaning |
|---|---|---|
| `PB_HANDOVER_TIMEOUT` | `60` | seconds the incoming replica waits; `0` disables the whole mechanism |
| `PB_HANDOVER_POLL` | `2` | seconds between the outgoing watcher's looks at `.pb_handover` (a Go duration such as `500ms` also works) |
| `PB_INSTANCE_ID` | hostname | this replica's id (the pod name is stable across a container restart) |
| `CLAVIGER_DB_URL` + `CLAVIGER_MULTI_REPLICA=true` | | several replicas on PostgreSQL overlap by design, so the handover is skipped |

## Events

Every event is appended to `.pb_handover.log`, written to stderr, and logged through `app.Logger()`, so it reaches whatever sink is configured (the SQLite `_logs`, and OTLP when `OTEL_EXPORTER_OTLP_ENDPOINT` is set) with attributes `source=single-writer-handover` and `instance`. Events raised before the logger exists are buffered and flushed once bootstrap finishes. The last event of an outgoing replica (`released to ...`) is written after its databases are closed, so it reaches OTLP and the log file but not `_logs`. Message texts match the shell form, so the existing alerts on `no handover after` and `never acknowledged` keep working.

## Tests

`go test ./tools/handover/ -race` ports every scenario of `scripts/test_entrypoint_handover.sh` (new to new handover, the restarted-after-park replica, slow release, owner restart, kill switch, missing lock support, uncreatable lock file, fail-open timeout, own-id request, multi-replica on PostgreSQL, single replica on PostgreSQL) with two in-process replicas on one temp dir and an overlap ledger, plus two real apps handing a database over through the hooks. The shell script has T13 and T14 for the entrypoint switch.

Not ported: the pre-#54 transition against an old entrypoint file (T3's script form, which needs an old `entrypoint.sh` as input). Its behaviour (no ack, then ignore the retired replica) is covered by `TestNoAckFromOldReplicaFailsOpenThenIgnoresStaleRequest`.
