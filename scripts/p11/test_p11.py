"""Unit tests for the P11 harness's decision logic. Run: /usr/bin/python3 -m unittest scripts/p11/test_p11.py"""

import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import p11  # noqa: E402

TH = p11.THRESHOLDS


def sample(wall, mono, up, boot=1000, wake=1):
    return {"wall": wall, "mono": mono, "up": up, "boot": boot, "sleep": 0, "wake": wake}


class ClassifyIntervalTest(unittest.TestCase):
    def test_normal_interval(self):
        iv = p11.classify_interval(sample(0, 0, 0), sample(300, 300, 300), 300, TH)
        self.assertEqual(iv["kind"], "normal")
        self.assertEqual(iv["unknownSec"], 0)

    def test_long_gap_explained_by_kernel_sleep(self):
        # 2h wall, monotonic counted 2h, uptime only 5 min: the machine slept.
        iv = p11.classify_interval(sample(0, 0, 0), sample(7200, 7200, 300, wake=2), 300, TH)
        self.assertEqual(iv["kind"], "sleep_gap")
        self.assertEqual(iv["sleptSec"], 6900)
        self.assertEqual(iv["unknownSec"], 0)

    def test_long_gap_without_sleep_evidence_is_unknown_not_sleep(self):
        iv = p11.classify_interval(sample(0, 0, 0), sample(7200, 7200, 7200), 300, TH)
        self.assertEqual(iv["kind"], "observed_clock_gap")
        self.assertGreater(iv["unknownSec"], 0)

    def test_wall_clock_jump_is_detected(self):
        iv = p11.classify_interval(sample(0, 0, 0), sample(3900, 300, 300), 300, TH)
        self.assertEqual(iv["kind"], "clock_jump")
        self.assertEqual(iv["clockJumpSec"], 3600)

    def test_backward_clock_jump_is_detected(self):
        iv = p11.classify_interval(sample(10000, 0, 0), sample(7000, 300, 300), 300, TH)
        self.assertEqual(iv["kind"], "clock_jump")
        self.assertLess(iv["clockJumpSec"], 0)

    def test_reboot_is_detected_by_boot_time(self):
        iv = p11.classify_interval(sample(0, 5000, 5000, boot=1), sample(900, 60, 60, boot=2), 300, TH)
        self.assertEqual(iv["kind"], "reboot")

    def test_first_sample(self):
        self.assertEqual(p11.classify_interval(None, sample(0, 0, 0), 300, TH)["kind"], "first")


class ParseGoTestTest(unittest.TestCase):
    def test_pass(self):
        r = p11.parse_go_test("=== RUN TestA\n--- PASS: TestA (0.1s)\nPASS\n")
        self.assertEqual((r["pass"], r["fail"], r["skip"], r["final"]), (1, 0, 0, "PASS"))

    def test_fail_and_skip_and_subtests(self):
        text = "--- FAIL: TestA (0.1s)\n    --- PASS: TestA/sub (0.0s)\n--- SKIP: TestB (0.0s)\nFAIL\n"
        r = p11.parse_go_test(text)
        self.assertEqual(r["failed"], ["TestA"])
        self.assertEqual(r["skipped"], ["TestB"])
        self.assertEqual(r["pass"], 1)
        self.assertEqual(r["final"], "FAIL")

    def test_panic(self):
        self.assertTrue(p11.parse_go_test("panic: boom\n")["panic"])


class DiffTerminalTest(unittest.TestCase):
    def test_terminal_run_reopened_is_a_violation(self):
        v, t, merged = p11.diff_terminal({"wf-1": ["completed", "t1"]}, {"wf-1": ["running", "t2"]})
        self.assertEqual(v, [{"runId": "wf-1", "was": "completed", "now": "running"}])

    def test_terminal_state_swapped_is_a_violation(self):
        v, _, _ = p11.diff_terminal({"wf-1": ["completed", "t1"]}, {"wf-1": ["failed", "t1"]})
        self.assertEqual(len(v), 1)

    def test_touched_row_is_not_a_violation(self):
        v, t, merged = p11.diff_terminal({"wf-1": ["cancelled", "t1"]}, {"wf-1": ["cancelled", "t2"]})
        self.assertEqual(v, [])
        self.assertEqual(t, [{"runId": "wf-1", "state": "cancelled"}])
        self.assertEqual(merged["wf-1"], ["cancelled", "t2"])

    def test_newly_terminal_runs_are_tracked(self):
        _, _, merged = p11.diff_terminal({}, {"wf-2": ["completed", "t"], "wf-3": ["running", "t"]})
        self.assertIn("wf-2", merged)
        self.assertNotIn("wf-3", merged)

    def test_missing_terminal_run_is_a_violation(self):
        v, _, _ = p11.diff_terminal({"wf-1": ["completed", "t1"]}, {})
        self.assertEqual(v[0]["now"], "missing")


class ClassifyTmuxTest(unittest.TestCase):
    TOKENS = {"ao-session:s1:l1": {"id": "s1", "runtimeInstanceId": "$3"}}

    def test_proven(self):
        self.assertEqual(p11.classify_tmux_session("ao-session:s1:l1", "aoi-x", "$3", self.TOKENS, "aoi-x"), "proven")

    def test_recycled_incarnation_is_not_proven(self):
        self.assertEqual(p11.classify_tmux_session("ao-session:s1:l1", "aoi-x", "$9", self.TOKENS, "aoi-x"), "instance_mismatch")

    def test_ao_stamped_runtime_without_live_row_is_orphan_candidate(self):
        self.assertEqual(p11.classify_tmux_session("ao-session:s9:l9", "aoi-x", "$1", self.TOKENS, "aoi-x"), "orphan_candidate")

    def test_other_installation_is_not_ours(self):
        self.assertEqual(p11.classify_tmux_session("ao-session:s9:l9", "aoi-y", "$1", self.TOKENS, "aoi-x"), "foreign_installation")

    def test_unreadable_is_never_orphan(self):
        self.assertEqual(p11.classify_tmux_session(None, None, "$1", self.TOKENS, "aoi-x"), "unreadable")

    def test_unstamped(self):
        self.assertEqual(p11.classify_tmux_session("", "", "$1", self.TOKENS, "aoi-x"), "unstamped")


class MemoryTrendTest(unittest.TestCase):
    def samples(self, mb_per_hour, hours=10, base=200):
        start = 0
        return [(start + m * 60, "aod-1", start, int((base + mb_per_hour * m / 60.0) * 1024))
                for m in range(0, hours * 60, 5)]

    def test_flat_is_pass(self):
        self.assertEqual(p11.memory_trend(self.samples(0.5), TH)["verdict"], "PASS")

    def test_growth_is_unknown_never_pass(self):
        r = p11.memory_trend(self.samples(20), TH)
        self.assertEqual(r["verdict"], "UNKNOWN")
        self.assertGreater(r["slopeMBPerHour"], TH["rssSlopeMBPerHour"])

    def test_short_evidence_is_unknown(self):
        self.assertEqual(p11.memory_trend(self.samples(0, hours=3), TH)["verdict"], "UNKNOWN")

    def test_restart_drop_is_not_growth(self):
        a = self.samples(0.5, hours=5)
        b = [(w + 6 * 3600, "aod-2", 6 * 3600, r) for w, _, _, r in self.samples(0.5, hours=5)]
        r = p11.memory_trend(a + b, TH)
        self.assertLessEqual(abs(r["slopeMBPerHour"]), TH["rssSlopeMBPerHour"])


class DueItemsTest(unittest.TestCase):
    SCHED = [{"id": "cp-t0", "atHours": 0, "actions": ["checkpoint"]},
             {"id": "fx-1", "atHours": 2, "actions": ["fixtures"]},
             {"id": "fx-2", "atHours": 10, "actions": ["fixtures"]},
             {"id": "cp-t12", "atHours": 12, "actions": ["checkpoint", "backup"]}]

    def test_nothing_before_time(self):
        items, _ = p11.due_items(self.SCHED, 1, {"cp-t0": "x"})
        self.assertEqual(items, [])

    def test_overdue_fixtures_coalesce(self):
        items, coalesced = p11.due_items(self.SCHED, 11, {"cp-t0": "x"})
        self.assertEqual([i["id"] for i in items], ["fx-2"])
        self.assertEqual([i["id"] for i in coalesced], ["fx-1"])

    def test_checkpoints_never_coalesce(self):
        items, _ = p11.due_items(self.SCHED, 13, {})
        self.assertIn("cp-t0", [i["id"] for i in items])
        self.assertIn("cp-t12", [i["id"] for i in items])

    def test_no_clock_no_items(self):
        self.assertEqual(p11.due_items(self.SCHED, None, {}), ([], []))


class ParsersTest(unittest.TestCase):
    def test_vm_stat(self):
        text = ("Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free:  3921.\n"
                "Pages inactive: 205783.\nPages occupied by compressor: 100.\n")
        r = p11.parse_vm_stat(text)
        self.assertEqual(r["freeMB"], int(3921 * 16384 / 1048576))
        self.assertEqual(r["compressedMB"], int(100 * 16384 / 1048576))

    def test_swapusage(self):
        r = p11.parse_swapusage("total = 10240.00M  used = 9165.69M  free = 1074.31M  (encrypted)")
        self.assertEqual(r, {"swapTotalMB": 10240, "swapUsedMB": 9166})

    def test_sysctl_sec(self):
        self.assertEqual(p11.parse_sysctl_sec("{ sec = 1789410070, usec = 167225 } Mon Sep 14"), 1789410070)
        self.assertIsNone(p11.parse_sysctl_sec(""))

    def test_daemon_log_scan_counts_and_offsets(self):
        with tempfile.NamedTemporaryFile("w", delete=False) as f:
            f.write('time=x level=INFO msg="ok"\ntime=x level=ERROR msg="probe failed" err=secret\n'
                    "time=x level=WARN msg=slow\n")
        try:
            counts, top, off = p11.scan_daemon_log(f.name, 0)
            self.assertEqual(counts, {"ERROR": 1, "WARN": 1})
            self.assertIn("ERROR probe failed", top)
            self.assertNotIn("secret", json.dumps(top))
            counts2, _, off2 = p11.scan_daemon_log(f.name, off)
            self.assertEqual(counts2, {"ERROR": 0, "WARN": 0})
            os.truncate(f.name, 0)
            counts3, _, _ = p11.scan_daemon_log(f.name, off2)
            self.assertEqual(counts3, {"ERROR": 0, "WARN": 0})
        finally:
            os.unlink(f.name)


class CommandLineTest(unittest.TestCase):
    def test_launchd_program_arguments_parse_to_the_monitor_loop(self):
        argv = p11.monitor_program_args("/x/p11.py", "run-24h-abc")
        self.assertEqual(argv[:2], ["/usr/bin/python3", "/x/p11.py"])
        args = p11.build_parser().parse_args(argv[2:])
        self.assertIs(args.fn, p11.cmd_run)
        self.assertEqual(args.run, "run-24h-abc")

    def test_operator_commands_parse(self):
        parser = p11.build_parser()
        self.assertTrue(parser.parse_args(["daemon-crash", "--confirm", "--restart"]).confirm)
        self.assertIs(parser.parse_args(["checkpoint", "--label", "x"]).fn, p11.cmd_checkpoint)
        self.assertEqual(parser.parse_args(["event", "electron_close_reopen", "--result", "pass"]).result, "pass")
        args = parser.parse_args(["maintenance", "--reason", "electron", "--minutes", "20"])
        self.assertIs(args.fn, p11.cmd_maintenance)
        self.assertEqual((args.reason, args.minutes, args.close), ("electron", 20, False))


class EvaluateTest(unittest.TestCase):
    """The report must never turn missing evidence into PASS."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.old_root = p11.ROOT
        p11.ROOT = self.tmp
        rid = "run-24h-test"
        os.makedirs(os.path.join(self.tmp, "runs", rid))
        man = {"runId": rid, "stage": "24h", "targetHours": 24, "heartbeatSeconds": 300,
               "schedule": p11.STAGES["24h"]["schedule"], "manualEvents": [], "required": p11.STAGES["24h"]["required"],
               "thresholds": TH, "ao": {}, "limitations": [], "platform": {}, "eccSha": "x"}
        p11.write_json(os.path.join(self.tmp, "runs", rid, "manifest.json"), man)
        self.run = p11.Run(rid)

    def tearDown(self):
        p11.ROOT = self.old_root

    def test_not_started(self):
        self.assertEqual(p11.evaluate(self.run)["verdict"], "NOT_STARTED")

    def test_finished_without_evidence_is_unknown_not_pass(self):
        p11.write_json(self.run.p("t0.json"), {"at": "x", "wall": 0, "expectedEnd": "y"})
        p11.write_json(self.run.p("stopped.json"), {"finalizedWall": 25 * 3600})
        ev = p11.evaluate(self.run, now=25 * 3600)
        self.assertEqual(ev["criteria"]["duration"]["status"], "PASS")
        self.assertEqual(ev["verdict"], "UNKNOWN")
        for key in ("daemonRestart", "controlledCrash", "sleepWake", "scratchRestore", "backups", "memoryTrend"):
            self.assertNotEqual(ev["criteria"][key]["status"], "PASS", key)

    def test_short_stage_is_fail_when_stopped(self):
        p11.write_json(self.run.p("t0.json"), {"at": "x", "wall": 0, "expectedEnd": "y"})
        p11.write_json(self.run.p("stopped.json"), {"finalizedWall": 3600})
        self.assertEqual(p11.evaluate(self.run, now=3600)["criteria"]["duration"]["status"], "FAIL")

    def test_critical_incident_is_no_go(self):
        p11.write_json(self.run.p("t0.json"), {"at": "x", "wall": 0, "expectedEnd": "y"})
        p11.open_incident(self.run, "duplicate_worker", "critical", "two owners")
        ev = p11.evaluate(self.run, now=3600)
        self.assertEqual(ev["goNoGo"], "NO-GO")
        self.assertEqual(ev["criteria"]["noCriticalIncident"]["status"], "FAIL")

    def test_clock_jump_makes_duration_unknown(self):
        p11.write_json(self.run.p("t0.json"), {"at": "x", "wall": 0, "expectedEnd": "y"})
        self.run.event("clock_jump", "attention", jumpSec=86400)
        self.assertEqual(p11.evaluate(self.run, now=90000)["criteria"]["duration"]["status"], "UNKNOWN")

    def test_invalid_backup_fails(self):
        p11.write_json(self.run.p("t0.json"), {"at": "x", "wall": 0, "expectedEnd": "y"})
        self.run.event("backup_verified", "fail", status="INVALID", path="/x")
        self.assertEqual(p11.evaluate(self.run, now=3600)["criteria"]["backups"]["status"], "FAIL")

    def test_evidence_files_are_private(self):
        rel = self.run.evidence("checkpoints/x.json", {"a": 1})
        self.assertEqual(os.stat(self.run.p(rel)).st_mode & 0o777, 0o600)
        self.assertEqual(os.stat(self.run.p("checkpoints")).st_mode & 0o777, 0o700)


if __name__ == "__main__":
    unittest.main()
