#!/usr/bin/env python3
"""SigNoz alert rules for auth.stfoafrisco.org (service.name = stfoa-auth), as code.

Rules are authored HERE and applied BY NAME to the SigNoz API. Never create or
edit these rules in the SigNoz UI: the next --apply overwrites it, and --check
reports it as drift.

Every rule notifies the SigNoz channel `ntfy-stfoa` (webhook -> self-hosted ntfy,
topic `stfoa`). The script refuses to run if that channel does not exist: SigNoz
rejects a rule that names no channel, and only a SigNoz ADMIN can create one.
SigNoz cannot rename a channel; to change one, create the new channel, repoint
the rules, delete the old one.

    export SIGNOZ_API_KEY=$(kubectl -n signoz get secret signoz-query-api-key \\
        -o jsonpath='{.data.key}' | base64 -d)       # never print or commit it
    python3 scripts/signoz/alert_rules.py            # dry run: CREATE/UPDATE per rule
    python3 scripts/signoz/alert_rules.py --check    # read-only drift report, exit 1 on drift
    python3 scripts/signoz/alert_rules.py --probe    # run every query against live data
    python3 scripts/signoz/alert_rules.py --apply    # write (clean checkout of origin/master only)

A SUCCESSFUL APPLY PROVES NOTHING. SigNoz evaluates a rule through the
`time_series` request type, which silently yields "query result is nil" for a
query shaped wrong, and a rule in that state looks exactly like a quiet one. The
sibling STFoA-Church rules sat dead for a week that way (#4193, #4783). Three
shape rules, each pinned by test_alert_rules.py:

  1. Select a constant time bucket (`ts`) and GROUP BY it.
  2. The `value` column must be a signed integer or float. `if(...)` returns
     UInt8, which is skipped: wrap it in toInt32(...). count() is UInt64 and fine.
  3. A "this thing went silent" rule must keep an outer table that still has rows
     when the thing is dark, so scope the condition INSIDE countIf().

After any change run --probe and confirm each rule reports a series (or an
explained empty result), then check the SigNoz server log:
    kubectl -n signoz logs signoz-0 --since=15m | grep 'query result is nil'
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass

DEFAULT_CHANNEL = "ntfy-stfoa"
DEFAULT_SIGNOZ_URL = "https://signoz.cluster.thedoodleproject.net"
TRUNK = "master"

# --- query building blocks --------------------------------------------------

LOGS = "signoz_logs.distributed_logs_v2"
WINDOW = (
    "timestamp >= {{.start_timestamp_nano}} AND timestamp <= {{.end_timestamp_nano}}"
)
PROD = "resources_string['deployment.environment'] = 'production'"
SERVICE = "resources_string['service.name'] = 'stfoa-auth'"
# One constant bucket per evaluation keeps count() meaning "N across the whole
# window". A fixed INTERVAL would silently rewrite every threshold.
TS_BUCKET = "fromUnixTimestamp64Nano({{.start_timestamp_nano}}) AS ts"

METRICS_SAMPLES = "signoz_metrics.distributed_samples_v4"
METRICS_SERIES = "signoz_metrics.distributed_time_series_v4"
METRICS_WINDOW = (
    "unix_milli >= intDiv({{.start_timestamp_nano}}, 1000000) AND "
    "unix_milli <= intDiv({{.end_timestamp_nano}}, 1000000)"
)
PROBE_URL = "https://auth.stfoafrisco.org"

# The app runs OTEL_MIN_LEVEL=WARN, so only WARN and ERROR records reach SigNoz:
# request logs are visible only when their status is >= 400. Every rule below
# is written against that, never against INFO.


def count_rule_query(*predicates: str) -> str:
    where = "\n  AND ".join([WINDOW, PROD, SERVICE, *predicates])
    return f"SELECT\n  {TS_BUCKET},\n  count() AS value\nFROM {LOGS}\nWHERE {where}\nGROUP BY ts"


def probe_series() -> str:
    return (
        f"SELECT fingerprint FROM {METRICS_SERIES}\n"
        "    WHERE metric_name = 'httpcheck.status'\n"
        f"      AND JSONExtractString(labels, 'http.url') = '{PROBE_URL}'"
    )


@dataclass(frozen=True)
class Rule:
    name: str
    severity: str  # "warning" | "critical"
    eval_window: str
    frequency: str
    target: int  # fires when value > target (SigNoz op "1", Above)
    intent: str  # the comparison in plain words; becomes the alert summary
    description: str  # runbook text; becomes the alert body in ntfy
    query: str
    enabled: bool = True


RULES: tuple[Rule, ...] = (
    Rule(
        name="PocketBase - Single-Writer Handover Failed",
        severity="critical",
        eval_window="30m0s",
        frequency="5m0s",
        target=0,
        intent="any 'no handover after' or 'never acknowledged' WARN from entrypoint.sh in 30 minutes",
        description=(
            "A rollout of auth.stfoafrisco.org started the incoming replica without proof that "
            "the outgoing one released /pb_data, so two writers may have shared the SQLite volume. "
            "This is the corruption alarm of #35/#54: auxiliary.db was corrupted five times by this. "
            "Runbook: (1) read the record body: 'no handover after Ns' means the lock was still held; "
            "'never acknowledged' means no ack arrived (expected exactly once, on the deploy that "
            "introduced the ack, so any later one needs a look). (2) Read the durable log: "
            "az containerapp exec -g stfoa-auth -n ca-auth --command 'tail -n 40 /pb_data/.pb_handover.log'. "
            "(3) Check integrity: see 'Recovering a corrupt auxiliary.db' in DEPLOY.md. "
            "(4) Confirm only one replica is running: az containerapp replica list -g stfoa-auth -n ca-auth."
        ),
        query=count_rule_query(
            "attributes_string['source'] = 'single-writer-handover'",
            "(body LIKE '%no handover after%' OR body LIKE '%never acknowledged%')",
        ),
    ),
    Rule(
        name="PocketBase - Server Errors (5xx)",
        severity="warning",
        eval_window="15m0s",
        frequency="5m0s",
        target=1,
        intent="2 or more HTTP 5xx responses from auth.stfoafrisco.org in 15 minutes",
        description=(
            "auth.stfoafrisco.org is returning HTTP 5xx. Baseline over 30 days is at most 1 per "
            "15 minutes, so two or more is a trend, not a blip. Sign-in for the parish apps depends "
            "on this service. Runbook: (1) open SigNoz Logs, service.name=stfoa-auth, status >= 500, "
            "and read the 'url' and 'error' attributes. (2) The known cause is SQLite lock/NFS "
            "latency ('context deadline exceeded') at the top of the hour when system crons run: "
            "check whether the failures cluster at :00-:01. (3) If they follow a deploy, compare "
            "az containerapp revision list -g stfoa-auth -n ca-auth and roll back the revision. "
            "(4) Check https://auth.stfoafrisco.org/api/health."
        ),
        query=count_rule_query("attributes_number['status'] >= 500"),
    ),
    Rule(
        name="PocketBase - Password Auth Failure Spike",
        severity="warning",
        eval_window="15m0s",
        frequency="5m0s",
        target=49,
        intent="50 or more failed password-auth requests (400/401/403/429) in 15 minutes",
        description=(
            "Failed auth-with-password requests are far above normal (30-day peak is 17 per 15 "
            "minutes), which looks like credential stuffing or a client stuck in a retry loop. "
            "Runbook: (1) in SigNoz Logs (service.name=stfoa-auth) group the failing requests by "
            "'client.address' / 'userIP' and by collection in the URL (users vs _superusers). "
            "(2) One address: block it at the edge. Many addresses against _superusers: treat as an "
            "attack, rotate the superuser password (PB_ADMIN_PASSWORD secret, redeploy). "
            "(3) One address that is a parish app server: a deploy broke its credentials, "
            "fix the caller. (4) 429s mean the rate limiter is already shedding load."
        ),
        query=count_rule_query(
            "body LIKE 'POST /api/collections/%/auth-with-password'",
            "attributes_number['status'] IN (400, 401, 403, 429)",
        ),
    ),
    Rule(
        name="PocketBase - Scheduled Cleanup Failing Continuously",
        severity="warning",
        eval_window="6h0m0s",
        frequency="30m0s",
        target=11,
        intent="12 or more 'Failed to delete expired ...' WARNs in 6 hours (the hourly job failing every run)",
        description=(
            "PocketBase's hourly system cron that deletes expired MFA and OTP sessions has failed on "
            "every run for 6 hours (each run logs two records). Occasional failures are normal here: "
            "30-day peak is 6 per 6 hours, caused by 'context deadline exceeded' on SQLite over NFS. "
            "Continuous failure means the database is wedged or locked, so sign-in will degrade next. "
            "Runbook: (1) read the 'exception.message' attribute. (2) Check for two writers: see the "
            "single-writer handover rule and /pb_data/.pb_handover.log. (3) Check /pb_data free space "
            "and the NFS share in the Azure portal. (4) If the database is corrupt, follow "
            "'Recovering a corrupt auxiliary.db' in DEPLOY.md. Restore from the S3 backup only as a "
            "last resort; Litestream is disabled, there is no blob replica."
        ),
        query=count_rule_query("body LIKE 'Failed to delete expired%'"),
    ),
    Rule(
        name="PocketBase - Telemetry Silent (24h)",
        severity="warning",
        eval_window="24h0m0s",
        frequency="1h0m0s",
        target=0,
        intent="zero log records from stfoa-auth in production over 24 hours",
        description=(
            "SigNoz has received nothing from auth.stfoafrisco.org in 24 hours. With OTEL_MIN_LEVEL=WARN "
            "the service normally emits ~230 records a day (internet scanner 404s alone guarantee "
            "traffic), and the longest silence in 30 days was 15 hours, so 24 hours means the "
            "service is down or the exporter is dead. The exporter fails SILENTLY: a wrong endpoint "
            "or protocol exports nothing and reports no error. Runbook: (1) curl -s -o /dev/null -w "
            "'%{http_code}' https://auth.stfoafrisco.org/api/health. (2) az containerapp replica list "
            "-g stfoa-auth -n ca-auth, and az containerapp logs show -n ca-auth -g stfoa-auth --tail 50. "
            "(3) Check OTEL_EXPORTER_OTLP_ENDPOINT and OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf "
            "on the container app and that OTLP_AUTH_HEADER still holds a valid ingest token. "
            "LIMIT: if the collector itself dies this rule cannot fire either."
        ),
        # Scope the condition INSIDE countIf over the whole table: the outer table
        # always has rows from other services, so the query still emits a row
        # while stfoa-auth is dark. Restricting the WHERE would make it nil.
        query=(
            f"SELECT\n  {TS_BUCKET},\n"
            f"  toInt32(if(countIf({SERVICE} AND {PROD}) = 0, 1, 0)) AS value\n"
            f"FROM {LOGS}\nWHERE {WINDOW}\nGROUP BY ts"
        ),
    ),
    Rule(
        # DISABLED until the probe exists. The httpcheck receiver on the K3s collector probes
        # app.stfoafrisco.org only; auth.stfoafrisco.org has no synthetic check, so enabling
        # this today would fire immediately and permanently. Enable it (set enabled=True and
        # re-apply) after the K3s repo adds PROBE_URL to the collector's httpcheck targets.
        name="PocketBase - Availability Probe Failing",
        severity="critical",
        eval_window="5m0s",
        frequency="1m0s",
        target=0,
        intent="no 2xx httpcheck sample for auth.stfoafrisco.org in 5 minutes",
        description=(
            "The synthetic check of https://auth.stfoafrisco.org is not getting a 2xx. Every parish "
            "app that signs in through PocketBase is affected. Runbook: (1) curl -i "
            "https://auth.stfoafrisco.org/api/health. (2) az containerapp show -g stfoa-auth -n ca-auth "
            "--query properties.runningStatus and az containerapp replica list. (3) A rollout parks the "
            "outgoing replica on purpose; a 2xx gap of more than ~60 s after a deploy is a failed handover. "
            "(4) If the probe, not the site, is down, see the 'Availability Probe Stopped Reporting' rule "
            "in STFoA-Church."
        ),
        query=(
            f"SELECT\n  {TS_BUCKET},\n"
            "  toInt32(if(sumIf(value, fingerprint IN (\n"
            f"    {probe_series()}\n"
            "      AND JSONExtractString(labels, 'http.status_class') = '2xx')) = 0, 1, 0)) AS value\n"
            f"FROM {METRICS_SAMPLES}\nWHERE {METRICS_WINDOW}\n"
            "  AND metric_name = 'httpcheck.status'\nGROUP BY ts"
        ),
        enabled=False,
    ),
)


# --- payload ---------------------------------------------------------------


def channel_name() -> str:
    return os.environ.get("SIGNOZ_CHANNEL") or DEFAULT_CHANNEL


def to_payload(rule: Rule, channel: str | None = None) -> dict:
    return {
        "schemaVersion": "v2alpha1",
        "alert": rule.name,
        "alertType": "LOGS_BASED_ALERT",
        "ruleType": "threshold_rule",
        "disabled": not rule.enabled,
        "condition": {
            "selectedQueryName": "A",
            "compositeQuery": {
                "queries": [
                    {
                        "type": "clickhouse_sql",
                        "spec": {"name": "A", "disabled": False, "query": rule.query},
                    }
                ]
            },
            "thresholds": {
                "kind": "basic",
                "spec": [
                    {
                        "name": rule.severity,
                        "target": rule.target,
                        "matchType": "1",
                        "op": "1",
                        "channels": [channel or channel_name()],
                    }
                ],
            },
        },
        "evaluation": {
            "kind": "rolling",
            "spec": {"evalWindow": rule.eval_window, "frequency": rule.frequency},
        },
        "notificationSettings": {"groupBy": [], "renotify": {"enabled": False}},
        "labels": {
            "severity": rule.severity,
            "service": "stfoa-auth",
            "source": "pocketbase",
            "managed_by": "scripts/signoz/alert_rules.py",
        },
        "annotations": {"summary": rule.intent, "description": rule.description},
    }


def digest(payload: dict) -> dict:
    """The fields that matter for drift, from a payload we built OR a live rule."""
    cond = payload.get("condition", {})
    queries = cond.get("compositeQuery", {}).get("queries", [{}])
    threshold = (cond.get("thresholds", {}).get("spec") or [{}])[0]
    return {
        "disabled": bool(payload.get("disabled")),
        "query": " ".join(queries[0].get("spec", {}).get("query", "").split()),
        "target": threshold.get("target"),
        "severity": threshold.get("name"),
        "channels": sorted(threshold.get("channels") or []),
        "evaluation": payload.get("evaluation", {}).get("spec"),
        "description": (payload.get("annotations") or {}).get("description"),
    }


# --- SigNoz API ------------------------------------------------------------


class Api:
    def __init__(self, base: str, key: str):
        self.base, self.key = base.rstrip("/"), key

    def call(self, path: str, body: dict | None = None, method: str | None = None) -> dict:
        req = urllib.request.Request(
            self.base + path,
            data=None if body is None else json.dumps(body).encode(),
            method=method or ("POST" if body is not None else "GET"),
            headers={"SIGNOZ-API-KEY": self.key, "Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                text = resp.read().decode()
        except urllib.error.HTTPError as err:
            raise SystemExit(f"{path} -> HTTP {err.code}: {err.read().decode()[:300]}")
        try:
            return json.loads(text)
        except json.JSONDecodeError:
            # An unknown SigNoz path falls through to the SPA and answers 200 with HTML.
            raise SystemExit(f"{path} returned non-JSON; wrong path or base URL?")


def require_channel(api: Api, channel: str) -> None:
    names = [c["name"] for c in api.call("/api/v1/channels").get("data") or []]
    if channel not in names:
        raise SystemExit(
            f'Notification channel "{channel}" does not exist in SigNoz. SigNoz refuses a rule '
            "that names no channel, and only a SigNoz ADMIN (or the service-account key where "
            "allowed) can create one: Settings -> Alert Channels -> New -> Webhook, URL "
            "https://alert.thedoodleproject.net/stfoa?template=alertmanager plus the ntfy auth "
            "(see K3s docs/current-state.md, '2026-10-16: alerting moved from Mattermost to ntfy').\n"
            f"Channels visible to this key: {', '.join(sorted(names)) or '(none)'}"
        )


def live_rules(api: Api) -> dict[str, dict]:
    data = api.call("/api/v1/rules").get("data") or {}
    rules = data.get("rules", []) if isinstance(data, dict) else data
    return {r.get("alert") or r.get("name"): r for r in rules}


def refusal_reason() -> str | None:
    """--apply must come from the exact commit origin/master points at."""
    try:
        subprocess.run(["git", "fetch", "origin", TRUNK], check=True, capture_output=True)
        head = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
        trunk = subprocess.check_output(["git", "rev-parse", f"origin/{TRUNK}"], text=True).strip()
        if head != trunk:
            return f"HEAD ({head[:12]}) is not origin/{TRUNK} ({trunk[:12]}) after fetch"
        if subprocess.check_output(["git", "status", "--porcelain"], text=True).strip():
            return "the checkout has uncommitted changes"
    except (subprocess.CalledProcessError, OSError) as err:
        return f"could not verify the checkout against origin/{TRUNK}: {err}"
    return None


def probe(api: Api, rule: Rule, lookback_hours: float) -> str:
    """Run the rule's query exactly as the evaluator does (time_series)."""
    end = int(time.time() * 1000)
    body = {
        "start": end - int(lookback_hours * 3600 * 1000),
        "end": end,
        "requestType": "time_series",
        "compositeQuery": {
            "queries": [
                {"type": "clickhouse_sql", "spec": {"name": "A", "query": rule.query, "disabled": False}}
            ]
        },
    }
    resp = api.call("/api/v5/query_range", body)
    result = ((resp.get("data") or {}).get("data") or {}).get("results") or [{}]
    aggs = result[0].get("aggregations")
    values = [
        v.get("value")
        for agg in aggs or []
        for series in agg.get("series") or []
        for v in series.get("values") or []
    ]
    if aggs is None:
        return "aggregations: null -> NIL (quiet if the query matches nothing; BROKEN if it should match)"
    return f"series values over {lookback_hours:g}h: {values}"


# --- CLI -------------------------------------------------------------------


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    mode = ap.add_mutually_exclusive_group()
    mode.add_argument("--apply", action="store_true", help="create/update rules by name")
    mode.add_argument("--check", action="store_true", help="read-only drift report; exit 1 on drift")
    mode.add_argument("--probe", action="store_true", help="run every query against live data")
    ap.add_argument("--lookback-hours", type=float, default=None, help="--probe window (default: each rule's own window)")
    args = ap.parse_args(argv)

    key = os.environ.get("SIGNOZ_API_KEY")
    if not key:
        print("SIGNOZ_API_KEY is required (see the module docstring for the kubectl one-liner).", file=sys.stderr)
        return 2
    api = Api(os.environ.get("SIGNOZ_URL", DEFAULT_SIGNOZ_URL), key)
    channel = channel_name()

    if args.apply:
        reason = refusal_reason()
        if reason:
            print(f"--apply refused: {reason}. Apply only from a clean, fetched origin/{TRUNK}.", file=sys.stderr)
            return 1

    require_channel(api, channel)
    live = live_rules(api)

    if args.probe:
        for rule in RULES:
            hours = args.lookback_hours or _window_hours(rule.eval_window)
            state = "" if rule.enabled else " [disabled rule]"
            print(f"{rule.name}{state}\n    {probe(api, rule, hours)}")
        return 0

    if args.check:
        drift = 0
        for rule in RULES:
            current = live.get(rule.name)
            if current is None:
                print(f"MISSING  {rule.name}")
                drift += 1
                continue
            want, have = digest(to_payload(rule, channel)), digest(current)
            changed = [k for k in want if want[k] != have[k]]
            if changed:
                print(f"DRIFT    {rule.name}: {', '.join(changed)}")
                drift += 1
        print("No drift." if not drift else f"\nDRIFT in {drift} of {len(RULES)} rule(s). Run --apply from {TRUNK}.")
        return 1 if drift else 0

    created = updated = 0
    for rule in RULES:
        current = live.get(rule.name)
        verb = "UPDATE" if current else "CREATE"
        if args.apply:
            payload = to_payload(rule, channel)
            if current:
                api.call(f"/api/v1/rules/{current['id']}", payload, method="PUT")
                updated += 1
            else:
                api.call("/api/v1/rules", payload)
                created += 1
        print(f"{verb:<6} {rule.name}{'' if rule.enabled else '  (disabled)'}")
    print(f"\nDone. {created} created, {updated} updated, channel {channel!r}." if args.apply
          else f"\nDRY RUN: {len(RULES)} rule(s), channel {channel!r}. Pass --apply to write.")
    return 0


def _window_hours(window: str) -> float:
    """'30m0s' / '6h0m0s' / '24h0m0s' -> hours."""
    total, num = 0.0, ""
    for ch in window:
        if ch.isdigit():
            num += ch
        else:
            total += int(num or 0) * {"h": 3600, "m": 60, "s": 1}[ch]
            num = ""
    return total / 3600


if __name__ == "__main__":
    sys.exit(main())
