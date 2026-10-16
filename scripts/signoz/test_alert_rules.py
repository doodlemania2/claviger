"""Pins the shape, severity and channel of every SigNoz rule in alert_rules.py.

    python3 -m unittest discover -s scripts/signoz -v

Offline and stdlib-only on purpose: it runs in CI with no SigNoz key. Behaviour
against live data is checked separately with `alert_rules.py --probe`.
"""

import os
import re
import sys
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))
import alert_rules as ar  # noqa: E402

# name -> (severity, evalWindow, frequency, target, enabled). Changing a threshold
# or a window is a deliberate act: update this table in the same commit.
EXPECTED = {
    "PocketBase - Single-Writer Handover Failed": ("critical", "30m0s", "5m0s", 0, True),
    "PocketBase - Server Errors (5xx)": ("warning", "15m0s", "5m0s", 1, True),
    "PocketBase - Password Auth Failure Spike": ("warning", "15m0s", "5m0s", 49, True),
    "PocketBase - Scheduled Cleanup Failing Continuously": ("warning", "6h0m0s", "30m0s", 11, True),
    "PocketBase - Telemetry Silent (24h)": ("warning", "24h0m0s", "1h0m0s", 0, True),
    "PocketBase - Availability Probe Failing": ("critical", "5m0s", "1m0s", 0, False),
}


class RuleShape(unittest.TestCase):
    def test_every_rule_is_pinned_and_nothing_else_exists(self):
        got = {
            r.name: (r.severity, r.eval_window, r.frequency, r.target, r.enabled)
            for r in ar.RULES
        }
        self.assertEqual(got, EXPECTED)

    def test_names_are_unique_and_namespaced(self):
        names = [r.name for r in ar.RULES]
        self.assertEqual(len(names), len(set(names)))
        # The sibling STFoA-Church repo owns every "STFoA - ..." rule, and apply-by-name
        # would make the two appliers overwrite each other.
        for n in names:
            self.assertTrue(n.startswith("PocketBase - "), n)

    def test_severity_is_one_ntfy_understands(self):
        for r in ar.RULES:
            self.assertIn(r.severity, {"warning", "critical"}, r.name)

    def test_every_rule_has_a_runbook(self):
        for r in ar.RULES:
            self.assertGreater(len(r.intent), 20, r.name)
            self.assertIn("Runbook:", r.description, r.name)
            self.assertIn("(1)", r.description, r.name)

    def test_queries_are_evaluable_time_series(self):
        for r in ar.RULES:
            q = r.query
            with self.subTest(rule=r.name):
                self.assertTrue(q.startswith("SELECT"))
                self.assertIn("{{.start_timestamp_nano}}", q)
                self.assertIn("{{.end_timestamp_nano}}", q)
                # #4193: a constant `ts` bucket, grouped, or the rule logs "nil" forever.
                self.assertIn(ar.TS_BUCKET, q)
                self.assertTrue(q.rstrip().endswith("GROUP BY ts"))
                self.assertRegex(q, r"AS value\b")
                # #4783: if(...) is UInt8 and is skipped; it must be wrapped.
                for m in re.finditer(r"(?<![A-Za-z0-9_])if\(", q):
                    self.assertTrue(q[: m.start()].endswith("toInt32("), "bare if() in " + r.name)

    def test_log_rules_are_scoped_to_production_and_this_service(self):
        for r in ar.RULES:
            if "distributed_logs_v2" in r.query and "countIf" not in r.query:
                with self.subTest(rule=r.name):
                    self.assertIn(ar.PROD, r.query)
                    self.assertIn(ar.SERVICE, r.query)

    def test_silence_rule_scopes_inside_countif(self):
        # If the service filter moved to WHERE, the query would return no rows while the
        # service is dark, which is nil, which can never fire.
        q = next(r for r in ar.RULES if "Silent" in r.name).query
        where = q.split("WHERE", 1)[1]
        self.assertNotIn("service.name", where.split("GROUP BY")[0])
        self.assertIn(f"countIf({ar.SERVICE} AND {ar.PROD})", q)

    def test_availability_rule_stays_disabled_until_a_probe_exists(self):
        rule = next(r for r in ar.RULES if "Availability" in r.name)
        self.assertFalse(rule.enabled)
        self.assertTrue(ar.to_payload(rule)["disabled"])
        self.assertIn("auth.stfoafrisco.org", rule.query)


class Channel(unittest.TestCase):
    def test_default_channel_is_ntfy_stfoa(self):
        with mock.patch.dict(os.environ, {}, clear=False):
            os.environ.pop("SIGNOZ_CHANNEL", None)
            self.assertEqual(ar.channel_name(), "ntfy-stfoa")
            for r in ar.RULES:
                spec = ar.to_payload(r)["condition"]["thresholds"]["spec"]
                self.assertEqual([s["channels"] for s in spec], [["ntfy-stfoa"]], r.name)

    def test_channel_can_be_overridden_for_a_one_off_run(self):
        with mock.patch.dict(os.environ, {"SIGNOZ_CHANNEL": "ntfy-elsewhere"}):
            payload = ar.to_payload(ar.RULES[0])
            self.assertEqual(payload["condition"]["thresholds"]["spec"][0]["channels"], ["ntfy-elsewhere"])

    def test_missing_channel_fails_loudly(self):
        api = mock.Mock()
        api.call.return_value = {"data": [{"name": "ntfy-tdpdf"}]}
        with self.assertRaises(SystemExit) as ctx:
            ar.require_channel(api, "ntfy-stfoa")
        self.assertIn("does not exist", str(ctx.exception))
        self.assertIn("ntfy-tdpdf", str(ctx.exception))

    def test_present_channel_passes(self):
        api = mock.Mock()
        api.call.return_value = {"data": [{"name": "ntfy-stfoa"}]}
        ar.require_channel(api, "ntfy-stfoa")


class NoRetiredWiring(unittest.TestCase):
    def test_no_mattermost_or_slack_in_rules(self):
        blob = repr(ar.RULES).lower() + repr([ar.to_payload(r) for r in ar.RULES]).lower()
        for word in ("mattermost", "slack", "work.apps", "hooks.slack"):
            self.assertNotIn(word, blob)

    def test_no_token_shaped_strings(self):
        blob = repr([ar.to_payload(r) for r in ar.RULES])
        self.assertNotRegex(blob, r"Bearer\s+\S{8,}")
        self.assertNotRegex(blob, r"[?&]auth=")


class Digest(unittest.TestCase):
    def test_digest_ignores_whitespace_and_matches_own_payload(self):
        for r in ar.RULES:
            p = ar.to_payload(r)
            self.assertEqual(ar.digest(p), ar.digest(p))
        p = ar.to_payload(ar.RULES[0])
        q = ar.to_payload(ar.RULES[0])
        q["condition"]["compositeQuery"]["queries"][0]["spec"]["query"] += "\n\n   "
        self.assertEqual(ar.digest(p), ar.digest(q))

    def test_digest_detects_channel_and_threshold_drift(self):
        base = ar.to_payload(ar.RULES[1])
        moved = ar.to_payload(ar.RULES[1], "ntfy-tdpdf")
        self.assertNotEqual(ar.digest(base), ar.digest(moved))
        edited = ar.to_payload(ar.RULES[1])
        edited["condition"]["thresholds"]["spec"][0]["target"] = 99
        self.assertNotEqual(ar.digest(base), ar.digest(edited))


class WindowParsing(unittest.TestCase):
    def test_window_hours(self):
        self.assertAlmostEqual(ar._window_hours("30m0s"), 0.5)
        self.assertAlmostEqual(ar._window_hours("6h0m0s"), 6)
        self.assertAlmostEqual(ar._window_hours("24h0m0s"), 24)


if __name__ == "__main__":
    unittest.main()
