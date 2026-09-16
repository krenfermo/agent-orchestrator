#!/usr/bin/env python3
"""P11 reliability soak harness for Agent Orchestrator (AO).

It OBSERVES and RECORDS. It never repairs AO: it does not restart a worker,
delete a tmux session or a run-file, restore or fix the database, or bring the
daemon back on its own. The only actions it takes are planned and recorded:
graceful daemon stop/start (canonical `ao stop`, frozen `ao daemon`), a crash of
a daemon whose identity it has PROVEN and that an operator confirmed, online
backups through P10, a scratch restore that never touches the live data dir, and
the isolated P9/P10 E2E fixtures, which run on their own scratch data dirs.

Stdlib only, so macOS /usr/bin/python3 under launchd needs nothing else.
See docs/p11-reliability-soak.md.
"""

import argparse
import fcntl
import hashlib
import json
import math
import os
import plistlib
import pwd
import re
import shlex
import shutil
import signal
import sqlite3
import subprocess
import sys
import time
import urllib.parse
import urllib.request
import uuid
from datetime import datetime, timezone

HARNESS_VERSION = "p11-harness/2"
HOME = pwd.getpwuid(os.getuid()).pw_dir
ROOT = os.environ.get("P11_ROOT") or os.path.join(HOME, ".ao", "soak", "p11")
LAUNCHD_LABEL = "com.aoagents.p11-soak"
TERMINAL = ("completed", "failed", "cancelled")
ACTIVE_WORK = ("pending", "running", "waiting")
GROWTH_TABLES = ("change_log", "workflow_runs", "workflow_steps", "workflow_outbox",
                 "workflow_attempts", "workflow_dispatch_checkpoints", "sessions", "review_run")

# Frozen at `init` into the manifest; changing them after T0 is moving the goalposts.
THRESHOLDS = {
    "heartbeatGapFactor": 2.5,       # a gap longer than this many intervals is explained or UNKNOWN
    "sleepEvidenceSec": 30,          # CLOCK_MONOTONIC - CLOCK_UPTIME_RAW above this = kernel-proven sleep
    "sleepWakeMinSec": 600,          # a sleep/wake cycle counts toward acceptance only if >= 10 min asleep
    "clockJumpSec": 120,             # |wall delta - monotonic delta| above this = wall clock changed
    "unknownWindowMaxMin": 30,       # any single unexplained window longer than this => duration UNKNOWN
    "daemonDownHeartbeats": 3,       # consecutive down heartbeats outside a maintenance window => incident
    "maintenanceWindowMin": 30,      # a planned stop/crash window closes on start or after this long
    "rssWarmupMin": 60,              # RSS samples in the first hour of each daemon instance are warm-up
    "rssSlopeMBPerHour": 8.0,        # pooled within-instance RSS slope above this => memory UNKNOWN (review)
    "rssGrowthFactor": 2.0,          # last post-warm-up hourly median above this x first => UNKNOWN (review)
    "rssMinSpanHours": 6,            # less post-warm-up evidence than this => memory UNKNOWN
    "diskFreeMinGB": 10,             # below this free space at any sample => disk attention
    "daemonLogRotateMB": 256,        # copy-truncate the daemon log above this size
    "orphanPersistScans": 2,         # an AO-owned orphan must persist this many scans to be an incident
    "restartGuardDeferHours": 3,     # a scheduled restart waits this long for active work to finish
}

STAGES = {
    "selftest": {
        "targetHours": 0.75, "heartbeatSeconds": 60,
        "schedule": [
            {"id": "cp-t0", "atHours": 0, "actions": ["checkpoint"]},
            {"id": "fx-1", "atHours": 0.08, "actions": ["fixtures"]},
            {"id": "cp-mid", "atHours": 0.3, "actions": ["checkpoint", "backup"]},
            {"id": "rs-1", "atHours": 0.45, "actions": ["daemon_restart"]},
            {"id": "cp-end", "atHours": 0.7, "actions": ["checkpoint"]},
        ],
        "manual": [],
        "required": {"heartbeats": 20, "checkpoints": 3, "daemonRestart": 1, "daemonCrash": 1,
                     "monitorRestart": 1, "onlineBackups": 1, "fixtureRuns": 1},
    },
    "24h": {
        "targetHours": 24, "heartbeatSeconds": 300,
        "schedule": [
            {"id": "cp-t0", "atHours": 0, "actions": ["checkpoint"]},
            {"id": "fx-1", "atHours": 2, "actions": ["fixtures"]},
            {"id": "rs-1", "atHours": 5, "actions": ["daemon_restart"]},
            {"id": "cp-t6", "atHours": 6, "actions": ["checkpoint"]},
            {"id": "fx-2", "atHours": 10, "actions": ["fixtures"]},
            {"id": "cp-t12", "atHours": 12, "actions": ["checkpoint", "backup"]},
            {"id": "cp-t18", "atHours": 18, "actions": ["checkpoint"]},
            {"id": "fx-3", "atHours": 20, "actions": ["fixtures"]},
            {"id": "cp-t24", "atHours": 24, "actions": ["checkpoint", "backup"]},
        ],
        "manual": [
            {"id": "sleep-1", "window": "T+4h..T+22h (a normal night counts)",
             "do": "let the Mac sleep (lid closed or idle sleep) for at least 20 minutes, then wake it",
             "detected": "automatically (kern.waketime + CLOCK_MONOTONIC vs CLOCK_UPTIME_RAW)"},
            {"id": "crash-1", "window": "T+8h..T+16h",
             "do": "p11 daemon-crash --confirm --restart"},
            {"id": "finalize", "window": ">= T+24h, after cp-t24 has run",
             "do": "p11 finalize"},
        ],
        "required": {"daemonRestart": 1, "daemonCrash": 1, "sleepWake": 1, "onlineBackups": 1,
                     "fixturePasses": 1, "electron": 0},
    },
    "48h": {
        "targetHours": 48, "heartbeatSeconds": 300,
        "schedule": [
            {"id": "cp-t0", "atHours": 0, "actions": ["checkpoint"]},
            {"id": "fx-1", "atHours": 3, "actions": ["fixtures"]},
            {"id": "rs-1", "atHours": 7, "actions": ["daemon_restart"]},
            {"id": "fx-2", "atHours": 11, "actions": ["fixtures"]},
            {"id": "cp-t12", "atHours": 12, "actions": ["checkpoint"]},
            {"id": "rs-2", "atHours": 19, "actions": ["daemon_restart"]},
            {"id": "bk-active", "atHours": 19.05, "actions": ["checkpoint", "backup"]},
            {"id": "fx-3", "atHours": 21, "actions": ["fixtures"]},
            {"id": "cp-t24", "atHours": 24, "actions": ["checkpoint", "backup"]},
            {"id": "rs-3", "atHours": 31, "actions": ["daemon_restart"]},
            {"id": "fx-4", "atHours": 33, "actions": ["fixtures"]},
            {"id": "cp-t36", "atHours": 36, "actions": ["checkpoint"]},
            {"id": "fx-5", "atHours": 43, "actions": ["fixtures"]},
            {"id": "cp-t48", "atHours": 48, "actions": ["checkpoint", "backup"]},
        ],
        "manual": [
            {"id": "sleep-1", "window": "first night", "do": "let the Mac sleep >= 20 min"},
            {"id": "sleep-2", "window": "second night", "do": "let the Mac sleep >= 20 min"},
            {"id": "electron-1", "window": "T+14h..T+30h",
             "do": "p11 electron-event (stops the soak daemon, opens/uses/quits the dev app twice on the frozen binary, restarts the soak daemon, records electron_close_reopen)"},
            {"id": "crash-1", "window": "T+26h..T+40h", "do": "p11 daemon-crash --confirm --restart"},
            {"id": "finalize", "window": ">= T+48h", "do": "p11 finalize"},
        ],
        "required": {"daemonRestart": 2, "daemonCrash": 1, "sleepWake": 2, "onlineBackups": 3,
                     "fixturePasses": 2, "electron": 1},
    },
    "72h": {
        "targetHours": 72, "heartbeatSeconds": 300,
        "schedule": (
            [{"id": "cp-t%d" % h, "atHours": h, "actions": ["checkpoint"] + (["backup"] if h in (24, 48, 72) else [])}
             for h in (0, 12, 24, 36, 48, 60, 72)]
            + [{"id": "fx-%d" % i, "atHours": h, "actions": ["fixtures"]} for i, h in enumerate((4, 14, 26, 38, 50, 62), 1)]
            + [{"id": "rs-1", "atHours": 10, "actions": ["daemon_restart"]},
               {"id": "rs-2", "atHours": 40, "actions": ["daemon_restart"]},
               {"id": "bk-active", "atHours": 40.05, "actions": ["checkpoint", "backup"]}]
        ),
        "manual": [
            {"id": "nights", "window": "all three nights", "do": "normal day/night use; let the Mac sleep"},
            {"id": "reboot-1", "window": "any time, if not done in 48h",
             "do": "reboot the Mac; after login run p11 daemon-start and p11 checkpoint --label post-reboot"},
            {"id": "electron-1", "window": "any day", "do": "p11 electron-event"},
            {"id": "crash-1", "window": "T+20h..T+60h", "do": "p11 daemon-crash --confirm --restart"},
            {"id": "finalize", "window": ">= T+72h", "do": "p11 finalize"},
        ],
        "required": {"daemonRestart": 2, "daemonCrash": 1, "sleepWake": 3, "onlineBackups": 4,
                     "fixturePasses": 3, "electron": 1, "rebootAcrossP11": 1},
    },
}


# ----------------------------------------------------------------------------- basics

def iso(ts=None):
    return datetime.fromtimestamp(time.time() if ts is None else ts, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def stamp(ts=None):
    return datetime.fromtimestamp(time.time() if ts is None else ts, timezone.utc).strftime("%Y%m%dT%H%M%SZ")


def ensure_dir(path):
    os.makedirs(path, mode=0o700, exist_ok=True)
    os.chmod(path, 0o700)
    return path


def open_private(path, flags):
    ensure_dir(os.path.dirname(path))
    return os.open(path, flags, 0o600)


def write_json(path, obj):
    tmp = "%s.tmp-%d" % (path, os.getpid())
    with os.fdopen(open_private(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC), "w") as f:
        json.dump(obj, f, indent=2, sort_keys=True, default=str)
        f.write("\n")
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)


def read_json(path, default=None):
    try:
        with open(path) as f:
            return json.load(f)
    except (OSError, ValueError):
        return default


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def run_cmd(argv, timeout=60, env=None, cwd=None, log_path=None):
    """Run argv; return (rc, stdout, stderr, seconds). Never raises for the child's failure."""
    start = time.monotonic()
    try:
        if log_path:
            with os.fdopen(open_private(log_path, os.O_WRONLY | os.O_CREAT | os.O_APPEND), "ab") as out:
                p = subprocess.run(argv, stdin=subprocess.DEVNULL, stdout=out, stderr=subprocess.STDOUT,
                                   env=env, cwd=cwd, timeout=timeout)
            return p.returncode, "", "", time.monotonic() - start
        p = subprocess.run(argv, stdin=subprocess.DEVNULL, capture_output=True, text=True,
                           env=env, cwd=cwd, timeout=timeout)
        return p.returncode, p.stdout, p.stderr, time.monotonic() - start
    except subprocess.TimeoutExpired:
        return 124, "", "timeout after %ss" % timeout, time.monotonic() - start
    except OSError as e:
        return 127, "", str(e), time.monotonic() - start


class FileLock:
    def __init__(self, path, blocking=True):
        self.path, self.blocking, self.fd = path, blocking, None

    def acquire(self):
        self.fd = open_private(self.path, os.O_WRONLY | os.O_CREAT)
        try:
            fcntl.flock(self.fd, fcntl.LOCK_EX | (0 if self.blocking else fcntl.LOCK_NB))
            return True
        except BlockingIOError:
            os.close(self.fd)
            self.fd = None
            return False

    def release(self):
        if self.fd is not None:
            fcntl.flock(self.fd, fcntl.LOCK_UN)
            os.close(self.fd)
            self.fd = None

    def __enter__(self):
        self.acquire()
        return self

    def __exit__(self, *exc):
        self.release()


# ----------------------------------------------------------------------------- run context

class Run:
    def __init__(self, run_id):
        self.id = run_id
        self.dir = os.path.join(ROOT, "runs", run_id)
        self.man = read_json(self.p("manifest.json"))
        if not self.man:
            raise SystemExit("p11: unknown run %s" % run_id)

    def p(self, *parts):
        return os.path.join(self.dir, *parts)

    def event(self, etype, result="ok", reason="", **meta):
        rec = {"ts": iso(), "type": etype, "result": result}
        if reason:
            rec["reason"] = reason
        rec.update({k: v for k, v in meta.items() if v is not None})
        line = (json.dumps(rec, separators=(",", ":"), sort_keys=True, default=str) + "\n").encode()
        with FileLock(self.p(".events.lock")):
            fd = open_private(self.p("events.jsonl"), os.O_WRONLY | os.O_CREAT | os.O_APPEND)
            try:
                os.write(fd, line)
                os.fsync(fd)
            finally:
                os.close(fd)
        return rec

    def events(self, etype=None):
        out = []
        try:
            with open(self.p("events.jsonl")) as f:
                for line in f:
                    try:
                        rec = json.loads(line)
                    except ValueError:
                        continue
                    if etype is None or rec.get("type") == etype:
                        out.append(rec)
        except OSError:
            pass
        return out

    def evidence(self, rel, obj):
        path = self.p(rel)
        write_json(path, obj)
        self.hash_file(rel)
        return rel

    def hash_file(self, rel):
        rec = {"ts": iso(), "path": rel, "sha256": sha256_file(self.p(rel))}
        with FileLock(self.p(".events.lock")):
            fd = open_private(self.p("hashes.jsonl"), os.O_WRONLY | os.O_CREAT | os.O_APPEND)
            try:
                os.write(fd, (json.dumps(rec, sort_keys=True) + "\n").encode())
            finally:
                os.close(fd)

    def state(self):
        return read_json(self.p("state.json"), {}) or {}

    def update_state(self, fn):
        with FileLock(self.p(".state.lock")):
            st = self.state()
            ret = fn(st)
            write_json(self.p("state.json"), st)
            return ret

    def t0(self):
        return read_json(self.p("t0.json"))

    def elapsed_hours(self, now=None):
        t0 = self.t0()
        if not t0:
            return None
        return round(((now or time.time()) - t0["wall"]) / 3600.0, 4)


def current_run(args):
    run_id = getattr(args, "run", None)
    if not run_id:
        try:
            with open(os.path.join(ROOT, "current")) as f:
                run_id = f.read().strip()
        except OSError:
            raise SystemExit("p11: no current run (use --run or p11 init)") from None
    return Run(run_id)


def die(msg, code=1):
    print("p11: " + msg, file=sys.stderr)
    raise SystemExit(code)


# ----------------------------------------------------------------------------- environment

_ENV_CACHE = {}


def base_env(man):
    """The daemon's base environment, resolved the way the desktop app does it: the
    user's login shell run from a launchd-like minimal env. Independent of whether
    the harness was invoked from a terminal, Claude, or launchd. Values are never
    recorded."""
    if "env" in _ENV_CACHE:
        return dict(_ENV_CACHE["env"][0]), _ENV_CACHE["env"][1]
    user = pwd.getpwuid(os.getuid()).pw_name
    shell = man.get("loginShell") or "/bin/zsh"
    minimal = {"HOME": HOME, "USER": user, "LOGNAME": user, "SHELL": shell, "TERM": "dumb",
               "PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "LANG": "en_US.UTF-8"}
    env, source = None, "login-shell"
    marker = b"__P11_ENV_START__\0"
    try:
        p = subprocess.run([shell, "-ilc", "printf '__P11_ENV_START__\\0'; /usr/bin/env -0"],
                           stdin=subprocess.DEVNULL, capture_output=True, env=minimal, cwd=HOME, timeout=30)
        i = p.stdout.find(marker)
        if p.returncode == 0 and i >= 0:
            env = {}
            for item in p.stdout[i + len(marker):].split(b"\0"):
                if b"=" in item:
                    k, v = item.split(b"=", 1)
                    env[k.decode("utf-8", "replace")] = v.decode("utf-8", "replace")
    except (OSError, subprocess.TimeoutExpired):
        env = None
    if not env:
        env, source = dict(minimal), "static-floor"
    for k in list(env):
        if k.startswith("AO_") or k in ("TMUX", "TMUX_PANE", "P11_ROOT"):
            del env[k]
    parts = [x for x in env.get("PATH", "").split(":") if x]
    for extra in man.get("pathFloor", "").split(":"):
        if extra and extra not in parts:
            parts.append(extra)
    env["PATH"] = ":".join(parts)
    _ENV_CACHE["env"] = (env, source)
    return dict(env), source


def ao_env(man, **overrides):
    env, _ = base_env(man)
    a = man["ao"]
    env.update({"AO_DATA_DIR": a["dataDir"], "AO_RUN_FILE": a["runFile"], "AO_PORT": str(a["port"])})
    env.update(a.get("extraEnv", {}))
    env.update({k: str(v) for k, v in overrides.items()})
    return env


def ao_cmd(man, args, timeout=60, **env_over):
    return run_cmd([man["ao"]["binary"]] + list(args), timeout=timeout, env=ao_env(man, **env_over), cwd=HOME)


# ----------------------------------------------------------------------------- observations

_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def healthz(port):
    try:
        with _OPENER.open("http://127.0.0.1:%d/healthz" % port, timeout=5) as r:
            return json.loads(r.read().decode())
    except Exception as e:  # noqa: BLE001 -- any failure is an observation, not a crash
        return {"error": type(e).__name__}


def daemon_status(man):
    rc, out, err, dur = ao_cmd(man, ["status", "--json"], timeout=45)
    try:
        st = json.loads(out)
    except ValueError:
        st = {"state": "error", "error": (err or out)[-300:]}
    st["_rc"], st["_sec"] = rc, round(dur, 2)
    return st


def read_installation_id(man):
    try:
        with open(os.path.join(man["ao"]["dataDir"], "installation_id")) as f:
            return f.read().strip()
    except OSError:
        return None


def daemon_identity(man, st):
    ident = {k: st.get(k) for k in ("state", "pid", "port", "instanceId", "installationId", "dataDir", "uptime", "health")}
    problems = []
    if st.get("state") == "ready":
        hz = healthz(man["ao"]["port"])
        ident["healthz"] = hz.get("status") or hz.get("error")
        ident["executablePath"] = hz.get("executablePath")
        if hz.get("pid") != st.get("pid"):
            problems.append("healthz_pid_mismatch")
        if hz.get("instanceId") != st.get("instanceId"):
            problems.append("healthz_instance_mismatch")
        if os.path.realpath(hz.get("dataDir") or "/nonexistent") != os.path.realpath(man["ao"]["dataDir"]):
            problems.append("data_dir_mismatch")
        if os.path.realpath(hz.get("executablePath") or "/nonexistent") != os.path.realpath(man["ao"]["binary"]):
            problems.append("binary_under_test_not_serving")
        if st.get("port") != man["ao"]["port"]:
            problems.append("port_mismatch")
        inst = read_installation_id(man)
        if inst and st.get("installationId") != inst:
            problems.append("installation_mismatch")
    ident["problems"] = problems
    return ident


def pid_alive(pid):
    """True while pid exists and has not exited. kill(pid, 0) also succeeds on a zombie: a process
    that exited (every descriptor, and its flock, released) whose parent has not reaped it yet --
    e.g. a daemon this monitor spawned and someone else stopped. A zombie is not alive. When ps
    cannot answer, the process is presumed alive (never report a pid gone without proof)."""
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    rc, out, err, _ = run_cmd(["ps", "-o", "stat=", "-p", str(pid)], 10)
    stat = out.strip()
    if rc == 1 and not stat and not err.strip():
        return False  # vanished (or reaped by run_cmd's own child bookkeeping) between the two checks
    return not stat.startswith("Z")


def port_listeners(port):
    rc, out, _, _ = run_cmd(["lsof", "-nP", "-iTCP:%d" % port, "-sTCP:LISTEN", "-t"], 15)
    return sorted({int(x) for x in out.split() if x.isdigit()})


def process_table():
    rc, out, _, _ = run_cmd(["ps", "-axo", "pid=,ppid=,rss=,comm="], 20)
    rows = []
    for line in out.splitlines():
        parts = line.split(None, 3)
        if len(parts) == 4 and parts[0].isdigit() and parts[1].isdigit() and parts[2].isdigit():
            rows.append((int(parts[0]), int(parts[1]), int(parts[2]), parts[3]))
    return rows


def ps_one(pid):
    rc, out, _, _ = run_cmd(["ps", "-o", "rss=,%cpu=,etime=", "-p", str(pid)], 10)
    parts = out.split()
    if rc != 0 or len(parts) < 3:
        return None
    try:
        return {"rssKB": int(parts[0]), "cpu": float(parts[1]), "etime": parts[2]}
    except ValueError:
        return None


def descendants(rows, root):
    children = {}
    for pid, ppid, rss, _comm in rows:
        children.setdefault(ppid, []).append((pid, rss))
    seen, stack, total = set(), [root], 0
    while stack:
        cur = stack.pop()
        for pid, rss in children.get(cur, []):
            if pid not in seen:
                seen.add(pid)
                total += rss
                stack.append(pid)
    return len(seen), total


def parse_vm_stat(text):
    page = 16384
    m = re.search(r"page size of (\d+) bytes", text)
    if m:
        page = int(m.group(1))
    vals = {}
    for line in text.splitlines():
        m = re.match(r'^"?([^:"]+)"?:\s+(\d+)\.?\s*$', line.strip())
        if m:
            vals[m.group(1).strip()] = int(m.group(2))

    def mb(key):
        return int(vals.get(key, 0) * page / 1048576)

    return {"freeMB": mb("Pages free"), "inactiveMB": mb("Pages inactive"), "activeMB": mb("Pages active"),
            "wiredMB": mb("Pages wired down"), "compressedMB": mb("Pages occupied by compressor")}


def parse_swapusage(text):
    def val(name):
        m = re.search(name + r" = ([\d.]+)([MG])", text)
        if not m:
            return None
        return round(float(m.group(1)) * (1024 if m.group(2) == "G" else 1))
    return {"swapTotalMB": val("total"), "swapUsedMB": val("used")}


def parse_sysctl_sec(text):
    m = re.search(r"sec = (\d+)", text or "")
    return int(m.group(1)) if m else None


def sysctl(name):
    rc, out, _, _ = run_cmd(["sysctl", "-n", name], 10)
    return out.strip() if rc == 0 else ""


def system_stats():
    s = parse_vm_stat(run_cmd(["vm_stat"], 10)[1])
    s.update(parse_swapusage(sysctl("vm.swapusage")))
    lvl = sysctl("kern.memorystatus_level")
    s["memLevel"] = int(lvl) if lvl.isdigit() else None
    fs = os.statvfs(HOME)
    s["diskFreeGB"] = round(fs.f_bavail * fs.f_frsize / 1e9, 1)
    s["load1"] = round(os.getloadavg()[0], 2)
    return s


def clock_sample():
    return {"wall": time.time(), "mono": time.clock_gettime(time.CLOCK_MONOTONIC),
            "up": time.clock_gettime(time.CLOCK_UPTIME_RAW),
            "boot": parse_sysctl_sec(sysctl("kern.boottime")),
            "sleep": parse_sysctl_sec(sysctl("kern.sleeptime")),
            "wake": parse_sysctl_sec(sysctl("kern.waketime"))}


def classify_interval(prev, cur, interval_sec, th):
    """Explain the time between two samples. Sleep is claimed only on kernel evidence:
    CLOCK_MONOTONIC keeps counting while asleep and CLOCK_UPTIME_RAW does not."""
    out = {"kind": "first", "gapSec": None, "sleptSec": 0, "unknownSec": 0}
    if not prev:
        return out
    gap = cur["wall"] - prev["wall"]
    out["gapSec"] = int(round(gap))
    if prev.get("boot") != cur.get("boot"):
        out["kind"] = "reboot"
        out["unknownSec"] = 0
        return out
    dmono = cur["mono"] - prev["mono"]
    slept = max(0.0, dmono - (cur["up"] - prev["up"]))
    out["sleptSec"] = int(round(slept))
    out["wakeChanged"] = prev.get("wake") != cur.get("wake")
    jump = gap - dmono
    if abs(jump) > th["clockJumpSec"]:
        out["kind"] = "clock_jump"
        out["clockJumpSec"] = int(round(jump))
        return out
    if gap > th["heartbeatGapFactor"] * interval_sec:
        out["unknownSec"] = int(max(0.0, gap - slept - interval_sec * th["heartbeatGapFactor"]))
        out["kind"] = "sleep_gap" if slept >= th["sleepEvidenceSec"] and out["unknownSec"] == 0 else (
            "sleep_gap_partial" if slept >= th["sleepEvidenceSec"] else "observed_clock_gap")
    elif slept >= th["sleepEvidenceSec"]:
        out["kind"] = "sleep_short"
    else:
        out["kind"] = "normal"
    return out


def db_files(man):
    base = os.path.join(man["ao"]["dataDir"], "ao.db")
    out = {}
    for suffix, key in (("", "size"), ("-wal", "wal"), ("-shm", "shm"), ("-journal", "journal")):
        try:
            st = os.stat(base + suffix)
            out[key] = st.st_size
            if not suffix:
                out["mtime"] = iso(st.st_mtime)
        except OSError:
            out[key] = None
    out["restoreJournal"] = os.path.exists(os.path.join(man["ao"]["dataDir"], ".ao-restore-journal.json"))
    return out


def db_open(db_path, daemon_live):
    wal = db_path + "-wal"
    immutable = (not daemon_live) and not (os.path.exists(wal) and os.path.getsize(wal) > 0)
    uri = "file:%s?mode=ro%s" % (urllib.parse.quote(db_path), "&immutable=1" if immutable else "")
    con = sqlite3.connect(uri, uri=True, timeout=30, isolation_level=None)
    con.execute("PRAGMA query_only=1")
    return con, immutable


def _tables(con):
    return {r[0] for r in con.execute("SELECT name FROM sqlite_master WHERE type='table'")}


def db_facts(con, check="quick"):
    """Durable facts, read-only. Owner tokens stay in memory (returned separately) and are
    never written to evidence."""
    f = {}
    t = time.monotonic()
    if check in ("quick", "full"):
        rows = con.execute("PRAGMA quick_check" if check == "quick" else "PRAGMA integrity_check").fetchall()
        ok = rows == [("ok",)]
        f["check"] = {"mode": check, "ok": ok, "sec": round(time.monotonic() - t, 2)}
        if not ok:
            f["check"]["detail"] = [str(r[0])[:200] for r in rows[:5]]
    if check == "full":
        fk = con.execute("PRAGMA foreign_key_check").fetchall()
        f["foreignKeyViolations"] = len(fk)
        f["foreignKeySample"] = [[str(x) for x in r] for r in fk[:5]]
    tables = _tables(con)
    f["goose"] = con.execute("SELECT MAX(version_id) FROM goose_db_version WHERE is_applied=1").fetchone()[0]
    f["pageCount"] = con.execute("PRAGMA page_count").fetchone()[0]
    f["freelistCount"] = con.execute("PRAGMA freelist_count").fetchone()[0]
    f["runsByState"] = dict(con.execute("SELECT state, COUNT(*) FROM workflow_runs GROUP BY state").fetchall())
    f["runs"] = {rid: [state, str(upd)] for rid, state, upd in
                 con.execute("SELECT id, state, updated_at FROM workflow_runs")}
    f["sessionsByState"] = {"%s/%s" % (a, "terminated" if term else "live"): n for a, term, n in
                            con.execute("SELECT activity_state, is_terminated, COUNT(*) FROM sessions GROUP BY 1, 2")}
    live_rows = con.execute(
        "SELECT id, project_id, issue_id, harness, runtime_instance_id, runtime_owner_token, runtime_launch_id "
        "FROM sessions WHERE is_terminated = 0").fetchall()
    f["liveSessions"] = [{"id": r[0], "projectId": r[1], "stepId": r[2], "harness": r[3],
                          "runtimeInstanceId": r[4], "hasOwnerToken": bool(r[5]), "launchId8": (r[6] or "")[:8]}
                         for r in live_rows]
    f["duplicateLiveSessionsByStep"] = [
        {"stepId": s, "sessions": n} for s, n in con.execute(
            "SELECT issue_id, COUNT(*) FROM sessions WHERE is_terminated = 0 AND issue_id <> '' "
            "GROUP BY issue_id HAVING COUNT(*) > 1")]
    f["runningStepsInTerminalRuns"] = con.execute(
        "SELECT COUNT(*) FROM workflow_steps s JOIN workflow_runs r ON r.id = s.workflow_run_id "
        "WHERE r.state IN ('completed','failed','cancelled') AND s.state = 'running'").fetchone()[0]
    f["dispatchedOutboxBySteps"] = [
        {"stepId": s, "dispatched": n} for s, n in con.execute(
            "SELECT workflow_step_id, COUNT(*) FROM workflow_outbox WHERE status = 'dispatched' "
            "AND workflow_step_id IS NOT NULL GROUP BY workflow_step_id HAVING COUNT(*) > 1")]
    if "review_run" in tables:
        cols = {r[1] for r in con.execute("PRAGMA table_info(review_run)")}
        if "status" in cols:
            f["reviewRunsByStatus"] = dict(con.execute("SELECT status, COUNT(*) FROM review_run GROUP BY status").fetchall())
    f["maxRowids"] = {}
    for tname in GROWTH_TABLES:
        if tname in tables:
            try:
                f["maxRowids"][tname] = con.execute("SELECT MAX(rowid) FROM %s" % tname).fetchone()[0]
            except sqlite3.Error:
                pass
    tokens = {r[5]: {"id": r[0], "runtimeInstanceId": r[4]} for r in live_rows if r[5]}
    return f, tokens


def diff_terminal(prev_terminal, runs):
    """prev_terminal: {id: [state, updated_at]} of every run ever seen terminal.
    Returns (violations, touched, merged). A terminal run that is no longer in the same
    terminal state is a violation; a terminal run whose row changed is only 'touched'."""
    violations, touched = [], []
    merged = dict(prev_terminal or {})
    for rid, (state, upd) in (prev_terminal or {}).items():
        cur = runs.get(rid)
        if cur is None:
            violations.append({"runId": rid, "was": state, "now": "missing"})
        elif cur[0] != state:
            violations.append({"runId": rid, "was": state, "now": cur[0]})
        elif cur[1] != upd:
            touched.append({"runId": rid, "state": state})
            merged[rid] = cur
    for rid, cur in runs.items():
        if cur[0] in TERMINAL and rid not in merged:
            merged[rid] = cur
    return violations, touched, merged


def tmux_env_value(socket, name, var):
    rc, out, err, _ = run_cmd(["tmux", "-L", socket, "show-environment", "-t", "=%s:" % name, var], 10)
    if rc != 0:
        return None
    line = out.strip()
    if line.startswith(var + "="):
        return line.split("=", 1)[1]
    return ""


def classify_tmux_session(owner, inst, sid, tokens, installation_id):
    row = tokens.get(owner) if owner else None
    if row is not None:
        return "proven" if row["runtimeInstanceId"] == sid else "instance_mismatch"
    if owner is None or inst is None:
        return "unreadable"
    if installation_id and inst == installation_id:
        return "orphan_candidate"
    if inst and installation_id and inst != installation_id:
        return "foreign_installation"
    return "unstamped"


def tmux_scan(man, tokens, installation_id):
    socket = man["ao"]["tmuxSocket"]
    rc, out, err, _ = run_cmd(["tmux", "-L", socket, "list-sessions", "-F", "#{session_name}\t#{session_id}"], 15)
    if rc != 0:
        msg = (err or "").lower()
        if "no server running" in msg or "error connecting" in msg or "no such file" in msg:
            return {"server": False, "sessions": [], "counts": {}}
        return {"server": "unreadable", "error": err.strip()[:200], "sessions": [], "counts": {}}
    items, counts = [], {}
    seen_tokens = set()
    for line in out.splitlines():
        if "\t" not in line:
            continue
        name, sid = line.split("\t", 1)
        owner = tmux_env_value(socket, name, "AO_SESSION_OWNER")
        inst = tmux_env_value(socket, name, "AO_INSTALLATION_ID")
        cls = classify_tmux_session(owner, inst, sid, tokens, installation_id)
        if owner:
            seen_tokens.add(owner)
        counts[cls] = counts.get(cls, 0) + 1
        items.append({"name": name, "sid": sid, "class": cls})
    rows_without_runtime = [row["id"] for tok, row in tokens.items() if tok not in seen_tokens]
    return {"server": True, "sessions": items, "counts": counts, "liveRowsWithoutRuntime": rows_without_runtime}


def scan_daemon_log(path, offset):
    """Count WARN/ERROR lines since offset. Records slog msg keys only, never attributes."""
    counts, msgs = {"ERROR": 0, "WARN": 0}, {}
    try:
        size = os.path.getsize(path)
    except OSError:
        return counts, msgs, 0
    if size < offset:
        offset = 0
    with open(path, "rb") as f:
        f.seek(offset)
        for raw in f:
            line = raw.decode("utf-8", "replace")
            m = re.search(r"\blevel=(ERROR|WARN)\b", line)
            if not m:
                continue
            counts[m.group(1)] += 1
            mm = re.search(r'\bmsg=(?:"((?:[^"\\]|\\.)*)"|(\S+))', line)
            key = "%s %s" % (m.group(1), (mm.group(1) or mm.group(2))[:120] if mm else "?")
            msgs[key] = msgs.get(key, 0) + 1
        offset = f.tell()
    top = dict(sorted(msgs.items(), key=lambda kv: -kv[1])[:15])
    return counts, top, offset


def rotate_daemon_log(run):
    path = run.p("process", "daemon.log")
    limit = run.man["thresholds"]["daemonLogRotateMB"] * 1048576
    try:
        if os.path.getsize(path) <= limit:
            return
    except OSError:
        return
    shutil.copyfile(path, path + ".1")
    os.chmod(path + ".1", 0o600)
    os.truncate(path, 0)  # the daemon holds it O_APPEND, so writes continue at the new end
    run.update_state(lambda s: s.update({"daemonLogOffset": 0}))
    run.event("daemon_log_rotated", limitMB=run.man["thresholds"]["daemonLogRotateMB"])


# ----------------------------------------------------------------------------- incidents

def open_incident(run, code, severity, summary, invariant="", evidence=None, source="auto", restart_required=None):
    def upd(s):
        s["incidentSeq"] = s.get("incidentSeq", 0) + 1
        return s["incidentSeq"]
    n = run.update_state(upd)
    inc_id = "INC-%s-%03d" % (run.id[-8:], n)
    t0 = run.t0()
    if restart_required is None:
        restart_required = severity == "critical"
    rec = {"incidentId": inc_id, "timestamp": iso(), "stageElapsedHours": run.elapsed_hours(), "stage": run.man["stage"],
           "code": code, "severity": severity, "symptom": summary, "invariantViolated": invariant or code,
           "evidence": evidence or [], "source": source, "status": "open", "restartRequired": bool(restart_required),
           "stageStarted": bool(t0), "dbAffected": None, "workLost": None, "duplicateOccurred": None,
           "reproduction": None, "suspectedComponent": None}
    rel = run.evidence("incidents/%s.json" % inc_id, rec)
    run.event("incident", "fail" if severity == "critical" else "attention", summary, incidentId=inc_id, code=code,
              severity=severity, file=rel)
    return inc_id


def auto_incident_once(run, code, severity, summary, evidence=None, invariant="", key=None):
    """Open an incident once per key. The default key is the summary text, which is only right for
    summaries that name one fixed cause; anything carrying a changing value (a count, a duration)
    must pass the episode's own key or every observation opens a new incident."""
    key = key or "%s:%s" % (code, summary[:80])
    fresh = run.update_state(lambda s: (key not in s.setdefault("autoIncidentKeys", [])) and
                             (s["autoIncidentKeys"].append(key) or True))
    if fresh:
        return open_incident(run, code, severity, summary, invariant=invariant, evidence=evidence)
    return None


def incidents(run):
    out = []
    d = run.p("incidents")
    if os.path.isdir(d):
        for name in sorted(os.listdir(d)):
            rec = read_json(os.path.join(d, name))
            if rec:
                out.append(rec)
    return out


def maintenance_open(st, now=None):
    m = st.get("maintenance")
    return bool(m and (now or time.time()) < m.get("expires", 0))


def open_maintenance(run, reason, minutes=None):
    minutes = minutes or run.man["thresholds"]["maintenanceWindowMin"]
    run.update_state(lambda s: s.update({"maintenance": {"reason": reason, "opened": time.time(),
                                                         "expires": time.time() + minutes * 60}}))


def close_maintenance(run):
    run.update_state(lambda s: s.pop("maintenance", None))


def begin_event(run, kind, minutes):
    """An operator event (e.g. electron-event) owns the daemon until end_event: scheduled restarts,
    crashes and checkpoints wait for it instead of acting on a daemon it is stopping and starting."""
    now = time.time()
    run.update_state(lambda s: s.update({"eventInProgress": {"kind": kind, "opened": now, "expires": now + minutes * 60}}))


def end_event(run):
    run.update_state(lambda s: s.pop("eventInProgress", None))


def event_in_progress(st, now=None):
    e = st.get("eventInProgress")
    return e if e and (now or time.time()) < e.get("expires", 0) else None


# ----------------------------------------------------------------------------- heartbeat

def heartbeat(run, monitor_pid=None):
    man = run.man
    th = man["thresholds"]
    interval = man["heartbeatSeconds"]
    clk = clock_sample()
    st = daemon_status(man)
    ident = daemon_identity(man, st)
    sysm = system_stats()
    dbf = db_files(man)
    dstats = ps_one(st["pid"]) if st.get("pid") and st.get("state") in ("ready", "not_ready", "unhealthy") else None
    hstats = ps_one(os.getpid())
    rotate_daemon_log(run)
    now = time.time()
    pending = []

    def upd(s):
        iv = classify_interval(s.get("lastClock"), clk, interval, th)
        s["lastClock"] = clk
        s["heartbeats"] = s.get("heartbeats", 0) + 1
        if monitor_pid is not None:
            prev_pid = s.get("monitorPid")
            if prev_pid and prev_pid != monitor_pid:
                pending.append(("monitor_restart", "ok", "", {"previousPid": prev_pid, "pid": monitor_pid,
                                                              "gapSec": iv.get("gapSec")}))
            s["monitorPid"] = monitor_pid
        kind = iv["kind"]
        if kind == "reboot":
            pending.append(("reboot_detected", "ok", "", {"gapSec": iv["gapSec"], "bootAt": iso(clk["boot"])}))
            s["maintenance"] = {"reason": "reboot", "opened": now, "expires": now + 12 * 3600}
            s["postEventCheckpoint"] = "post-reboot"
        elif kind == "clock_jump":
            pending.append(("clock_jump", "attention", "wall clock moved independently of CLOCK_MONOTONIC",
                            {"jumpSec": iv["clockJumpSec"], "gapSec": iv["gapSec"]}))
        elif kind in ("sleep_gap", "sleep_gap_partial", "sleep_short"):
            meta = {"sleptSec": iv["sleptSec"], "gapSec": iv["gapSec"], "unknownSec": iv["unknownSec"],
                    "wakeAt": iso(clk["wake"]) if clk.get("wake") else None,
                    "countsAsCycle": iv["sleptSec"] >= th["sleepWakeMinSec"]}
            pending.append(("sleep_wake_detected", "ok", "kernel-proven sleep", meta))
            if meta["countsAsCycle"]:
                s["sleepCycles"] = s.get("sleepCycles", 0) + 1
                s["postEventCheckpoint"] = "post-wake-%d" % s["sleepCycles"]
        elif kind == "observed_clock_gap":
            pending.append(("observed_clock_gap", "attention", "gap without sleep evidence",
                            {"gapSec": iv["gapSec"], "unknownSec": iv["unknownSec"]}))
        state = st.get("state")
        if state == "ready":
            inst = st.get("instanceId")
            prev_inst = s.get("daemonInstance")
            if prev_inst and inst != prev_inst:
                planned = maintenance_open(s, now) or s.get("expectInstance") == inst
                pending.append(("daemon_instance_changed", "ok" if planned else "attention",
                                "" if planned else "daemon incarnation changed outside a planned event",
                                {"previous": prev_inst, "instanceId": inst, "planned": planned}))
                if not planned:
                    pending.append(("__incident__", "high", "unplanned_daemon_restart",
                                    {"summary": "daemon instance changed outside a planned event (%s -> %s)" % (prev_inst, inst),
                                     "key": "unplanned_daemon_restart:%s" % inst}))
            s["daemonInstance"] = inst
            s["daemonInstanceSince"] = s.get("daemonInstanceSince") if prev_inst == inst else now
            if s.get("downCount"):
                pending.append(("daemon_available", "ok", "", {"afterHeartbeats": s["downCount"],
                                                               "downSince": iso(s["downSince"]) if s.get("downSince") else None}))
            s["downCount"] = 0
            s.pop("downSince", None)
            for problem in ident["problems"]:
                pending.append(("__incident__", "critical", problem, {"summary": "daemon identity: " + problem}))
        else:
            s["downCount"] = s.get("downCount", 0) + 1
            if s["downCount"] == 1 or not s.get("downSince"):
                # Also for an outage already running when this harness version took over its state.
                s["downSince"] = now
                pending.append(("daemon_unavailable", "ok" if maintenance_open(s, now) else "attention", "",
                                {"state": state, "maintenance": (s.get("maintenance") or {}).get("reason")}))
            if s["downCount"] >= th["daemonDownHeartbeats"] and not maintenance_open(s, now):
                # One incident per outage episode (keyed by when it began), however many heartbeats
                # it lasts; the episode's end is the daemon_available event.
                down_since = s.get("downSince") or now
                pending.append(("__incident__", "high", "daemon_down_unplanned",
                                {"summary": "daemon %s since %s, outside a maintenance window (%d heartbeats when opened)"
                                            % (state, iso(down_since), s["downCount"]),
                                 "key": "daemon_down_unplanned:%s" % iso(down_since)}))
        m = s.get("maintenance")
        if m and now >= m.get("expires", 0) and not m.get("expiryRecorded"):
            m["expiryRecorded"] = True
            if state != "ready":
                pending.append(("maintenance_expired_daemon_down", "attention",
                                "maintenance window expired with the daemon %s" % state,
                                {"maintenance": m.get("reason"), "openedAt": iso(m.get("opened")),
                                 "expiredAt": iso(m.get("expires")),
                                 "downSince": iso(s["downSince"]) if s.get("downSince") else None}))
        if sysm.get("diskFreeGB") is not None and sysm["diskFreeGB"] < th["diskFreeMinGB"]:
            pending.append(("disk_low", "attention", "", {"diskFreeGB": sysm["diskFreeGB"]}))
        return iv

    iv = run.update_state(upd)
    rec = {"kind": iv["kind"], "gap": iv.get("gapSec"), "slept": iv.get("sleptSec"), "el": run.elapsed_hours(now),
           "d": {"st": st.get("state"), "pid": st.get("pid"), "inst": (st.get("instanceId") or "")[-12:],
                 "rss": (dstats or {}).get("rssKB"), "cpu": (dstats or {}).get("cpu")},
           "sys": sysm, "db": {k: dbf.get(k) for k in ("size", "wal", "shm")}, "h": (hstats or {}).get("rssKB")}
    run.event("heartbeat", **rec)
    for etype, result, reason, meta in pending:
        if etype == "__incident__":
            auto_incident_once(run, reason, result, meta["summary"], key=meta.get("key"))
        else:
            run.event(etype, result, reason, **meta)
    return rec


# ----------------------------------------------------------------------------- checkpoint

def ownership_readback(man, run_ids):
    out = {}
    for rid in run_ids[:20]:
        rc, text, err, _ = ao_cmd(man, ["workflow", "recover", "ownership", rid], timeout=60)
        out[rid] = {"rc": rc, "text": (text or err)[:4000]}
    return out


def checkpoint(run, label, with_backup=False, trigger="manual"):
    lk = FileLock(run.p(".checkpoint.lock"), blocking=False)
    if not lk.acquire():
        run.event("checkpoint", "skipped", "another checkpoint is in progress", label=label)
        return None
    try:
        return _checkpoint(run, label, with_backup, trigger)
    finally:
        lk.release()


def _checkpoint(run, label, with_backup, trigger):
    man = run.man
    th = man["thresholds"]
    started = time.time()
    st = daemon_status(man)
    ident = daemon_identity(man, st)
    live = st.get("state") == "ready"
    findings = []
    cp = {"label": label, "trigger": trigger, "startedAt": iso(started), "elapsedHours": run.elapsed_hours(started),
          "daemon": ident, "system": system_stats(), "dbFiles": db_files(man)}
    cp["binarySha256Ok"] = sha256_file(man["ao"]["binary"]) == man["ao"]["binarySha256"]
    if not cp["binarySha256Ok"]:
        findings.append(("critical", "binary_under_test_changed", "frozen AO binary no longer matches the manifest"))
    for problem in ident["problems"]:
        findings.append(("critical", problem, "daemon identity"))
    rows = process_table()
    procs = {"harness": ps_one(os.getpid())}
    if st.get("pid") and st.get("state") in ("ready", "not_ready", "unhealthy"):
        procs["daemon"] = ps_one(st["pid"])
        n, rss = descendants(rows, st["pid"])
        procs["daemonChildren"] = {"count": n, "rssKB": rss}
    ecc_electron = man.get("electronPath", "")
    el = [r for r in rows if ecc_electron and r[3].startswith(ecc_electron)]
    procs["electron"] = {"count": len(el), "rssKB": sum(r[2] for r in el)}
    cp["processes"] = procs
    if cp["dbFiles"].get("restoreJournal"):
        findings.append(("critical", "restore_journal_present", "a restore journal exists in the live data dir"))

    facts, tokens = None, {}
    try:
        con, immutable = db_open(os.path.join(man["ao"]["dataDir"], "ao.db"), live)
        try:
            facts, tokens = db_facts(con, "quick")
        finally:
            con.close()
        cp["dbReadImmutable"] = immutable
    except sqlite3.Error as e:
        findings.append(("critical", "db_unreadable", str(e)[:200]))
    if facts:
        cp["db"] = facts
        if not facts["check"]["ok"]:
            findings.append(("critical", "db_quick_check_failed", "; ".join(facts["check"].get("detail", []))))
        if facts["goose"] != man["baseline"].get("goose"):
            findings.append(("attention", "goose_changed", "goose %s -> %s" % (man["baseline"].get("goose"), facts["goose"])))
        if facts["duplicateLiveSessionsByStep"]:
            findings.append(("critical", "duplicate_worker", json.dumps(facts["duplicateLiveSessionsByStep"])[:300]))
        if facts["dispatchedOutboxBySteps"]:
            findings.append(("critical", "duplicate_dispatch", json.dumps(facts["dispatchedOutboxBySteps"])[:300]))

        def term(s):
            v, t, merged = diff_terminal(s.get("terminalRuns"), facts["runs"])
            prev_running = s.get("runningStepsInTerminalRuns")
            s["terminalRuns"] = merged
            s["runningStepsInTerminalRuns"] = facts["runningStepsInTerminalRuns"]
            return v, t, prev_running
        violations, touched, prev_running = run.update_state(term)
        cp["terminalImmutability"] = {"terminalRunsTracked": sum(1 for r in facts["runs"].values() if r[0] in TERMINAL),
                                      "violations": violations, "touched": touched,
                                      "runningStepsInTerminalRuns": facts["runningStepsInTerminalRuns"]}
        if violations:
            findings.append(("critical", "terminal_state_mutated", json.dumps(violations)[:300]))
        if prev_running is not None and facts["runningStepsInTerminalRuns"] > prev_running:
            findings.append(("critical", "terminal_run_step_reopened",
                             "running steps in terminal runs %s -> %s" % (prev_running, facts["runningStepsInTerminalRuns"])))
        nonterminal = sorted(rid for rid, r in facts["runs"].items() if r[0] not in TERMINAL)
        cp["db"].pop("runs", None)

        installation = read_installation_id(man)
        tm = tmux_scan(man, tokens, installation)
        cp["tmux"] = tm
        if tm.get("server") == "unreadable":
            findings.append(("attention", "tmux_unreadable", tm.get("error", "")))
        orphans = sorted(i["name"] for i in tm.get("sessions", []) if i["class"] == "orphan_candidate")

        def orph(s):
            prev = s.get("orphanSeen", {})
            s["orphanSeen"] = {name: prev.get(name, 0) + 1 for name in orphans}
            return [n for n, c in s["orphanSeen"].items() if c >= th["orphanPersistScans"]]
        persistent = run.update_state(orph)
        if orphans:
            findings.append(("attention", "orphan_candidate", ",".join(orphans)))
        if persistent:
            findings.append(("critical", "orphan_runtime", ",".join(persistent)))
        if tm.get("counts", {}).get("instance_mismatch"):
            findings.append(("attention", "tmux_instance_mismatch", str(tm["counts"]["instance_mismatch"])))
        if live:
            cp["ownershipReadback"] = ownership_readback(man, nonterminal)
    offset = run.state().get("daemonLogOffset", 0)
    counts, top, new_offset = scan_daemon_log(run.p("process", "daemon.log"), offset)
    run.update_state(lambda s: s.update({"daemonLogOffset": new_offset}))
    cp["daemonLogSincePrevious"] = {"counts": counts, "topMessages": top}
    if with_backup:
        b = do_backup(run, label, online=live)
        cp["backup"] = b
        if b.get("verifyStatus") != "VALID":
            findings.append(("critical", "backup_not_valid", "%s: %s" % (b.get("backupId"), b.get("verifyStatus"))))
    cp["findings"] = [{"severity": s, "code": c, "detail": d} for s, c, d in findings]
    cp["durationSec"] = round(time.time() - started, 1)
    result = "fail" if any(s == "critical" for s, _, _ in findings) else ("attention" if findings else "ok")
    cp["result"] = result
    rel = run.evidence("checkpoints/%s-%s.json" % (stamp(started), label), cp)
    run.event("checkpoint", result, label=label, trigger=trigger, file=rel,
              findings=[c for _, c, _ in findings], daemon=ident.get("state"),
              quickCheck=(facts or {}).get("check", {}).get("ok"), runsByState=(facts or {}).get("runsByState"),
              liveSessions=len((facts or {}).get("liveSessions", [])), tmux=(cp.get("tmux") or {}).get("counts"),
              backupId=(cp.get("backup") or {}).get("backupId"), sec=cp["durationSec"])
    for sev, code, detail in findings:
        if sev == "critical":
            auto_incident_once(run, code, "critical", "%s (%s)" % (code, detail[:120]), evidence=[rel])
    return cp


# ----------------------------------------------------------------------------- backup / restore

def do_backup(run, label, online):
    man = run.man
    note = "p11 %s %s" % (run.id, label)
    rc, out, err, dur = ao_cmd(man, ["backup", "create", "--json", "--note", note], timeout=3600)
    res = {"label": label, "online": online, "createRc": rc, "createSec": round(dur, 1)}
    try:
        rep = json.loads(out)
    except ValueError:
        rep = {"error": (err or out)[-400:]}
    run.evidence("backups/%s-%s-create.json" % (stamp(), label), rep)
    path = rep.get("path")
    res["backupId"] = rep.get("backupId") or (os.path.basename(path) if path else None)
    res["path"] = path
    if rc != 0 or not path:
        res["verifyStatus"] = "NOT_CREATED"
        run.event("backup_created", "fail", (err or "")[-200:], label=label, online=online)
        return res
    run.event("backup_created", "ok", label=label, online=online, backupId=res["backupId"], sec=res["createSec"])
    res.update(verify_backup(run, path, label))
    return res


def verify_backup(run, path, label):
    rc, out, err, dur = ao_cmd(run.man, ["backup", "verify", "--json", path], timeout=3600)
    try:
        rep = json.loads(out)
    except ValueError:
        rep = {"error": (err or out)[-400:]}
    run.evidence("backups/%s-%s-verify.json" % (stamp(), label), rep)
    status = rep.get("status") or rep.get("result") or ("ERROR rc=%d" % rc)
    res = {"verifyRc": rc, "verifyStatus": status, "compatibility": rep.get("compatibility"),
           "verifySec": round(dur, 1), "gooseVersion": rep.get("gooseVersion")}
    run.event("backup_verified", "ok" if status == "VALID" else "fail", label=label, path=path,
              status=status, compatibility=rep.get("compatibility"), sec=res["verifySec"])
    return res


def compare_counts(a_path, b_path):
    out = {}
    for tag, path in (("backup", a_path), ("restored", b_path)):
        con, _ = db_open(path, False)
        try:
            f, _ = db_facts(con, "none")
        finally:
            con.close()
        out[tag] = {"runsByState": f["runsByState"], "sessionsByState": f["sessionsByState"],
                    "maxRowids": f["maxRowids"], "goose": f["goose"]}
    out["equal"] = out["backup"] == out["restored"]
    return out


def scratch_restore(run, backup_path=None):
    man = run.man
    if not backup_path:
        valid = [e for e in run.events("backup_verified") if e.get("status") == "VALID"]
        if not valid:
            die("no VALID backup recorded in this run")
        backup_path = valid[-1]["path"]
    started = time.time()
    base = ensure_dir(run.p("scratch-restore", "work-" + stamp(started)))
    data, root = ensure_dir(os.path.join(base, "data")), ensure_dir(os.path.join(base, "backups"))
    report = {"backupPath": backup_path, "startedAt": iso(started), "checks": {}}
    flags = man.get("scratchRestoreFlags", [])
    rc, out, err, dur = run_cmd(
        [man["ao"]["binary"], "backup", "restore", backup_path, "--yes", "--json", "--root", root] + flags,
        timeout=3600, cwd=HOME,
        env=ao_env(man, AO_DATA_DIR=data, AO_RUN_FILE=os.path.join(base, "running.json"),
                   AO_PORT=man["scratchPort"], AO_BACKUP_DIR=root))
    try:
        rep = json.loads(out)
    except ValueError:
        rep = {"error": (err or out)[-600:]}
    report.update({"restoreRc": rc, "restoreSec": round(dur, 1), "restoreReport": rep})
    checks = report["checks"]
    db = os.path.join(data, "ao.db")
    manifest = read_json(os.path.join(backup_path, "manifest.json"), {})
    checks["restoredResult"] = rep.get("result") or rep.get("status")
    checks["dbPresent"] = os.path.isfile(db)
    if checks["dbPresent"]:
        con, _ = db_open(db, False)
        try:
            f, _ = db_facts(con, "full")
        finally:
            con.close()
        checks["integrityOk"] = f["check"]["ok"]
        checks["foreignKeyViolations"] = f["foreignKeyViolations"]
        checks["goose"] = f["goose"]
        checks["gooseMatchesManifest"] = f["goose"] == (manifest.get("schema") or {}).get("gooseVersion")
        asset = next((a for a in manifest.get("assets", []) if a.get("path") == "ao.db"), {})
        checks["sha256MatchesManifest"] = bool(asset) and sha256_file(db) == asset.get("sha256")
        cmp_ = compare_counts(os.path.join(backup_path, "ao.db"), db)
        checks["countsEqualBackup"] = cmp_["equal"]
        report["counts"] = cmp_["restored"]
        src_inst = os.path.join(backup_path, "installation_id")
        if os.path.exists(src_inst):
            with open(src_inst) as a, open(os.path.join(data, "installation_id")) as b:
                checks["installationIdRestored"] = a.read().strip() == b.read().strip()
        else:
            checks["installationIdRestored"] = "not_in_backup"
        checks["noSidecars"] = not any(os.path.exists(db + s) for s in ("-wal", "-shm", "-journal"))
        checks["noRestoreJournal"] = not os.path.exists(os.path.join(data, ".ao-restore-journal.json"))
    required = ("dbPresent", "integrityOk", "gooseMatchesManifest", "sha256MatchesManifest", "countsEqualBackup",
                "noSidecars", "noRestoreJournal")
    passed = (rc == 0 and checks.get("foreignKeyViolations") == 0 and all(checks.get(k) is True for k in required)
              and checks.get("installationIdRestored") in (True, "not_in_backup"))
    report["result"] = "pass" if passed else "fail"
    shutil.rmtree(base, ignore_errors=True)
    report["scratchRemoved"] = not os.path.exists(base)
    rel = run.evidence("scratch-restore/%s.json" % stamp(started), report)
    run.event("scratch_restore", report["result"], backupPath=backup_path, file=rel, sec=report["restoreSec"],
              failed=[k for k in required if checks.get(k) is not True] or None)
    return report


# ----------------------------------------------------------------------------- fixtures

def parse_go_test(text):
    res = {"pass": 0, "fail": 0, "skip": 0, "failed": [], "skipped": []}
    for m in re.finditer(r"^\s*--- (PASS|FAIL|SKIP): (\S+)", text, re.M):
        kind, name = m.group(1), m.group(2)
        res[kind.lower()] += 1
        if kind == "FAIL":
            res["failed"].append(name)
        elif kind == "SKIP":
            res["skipped"].append(name)
    res["panic"] = bool(re.search(r"^panic: ", text, re.M))
    res["final"] = "PASS" if re.search(r"^PASS\s*$", text, re.M) and not res["fail"] else (
        "FAIL" if re.search(r"^FAIL", text, re.M) or res["fail"] else "UNKNOWN")
    return res


def tmux_socket_names():
    d = "/private/tmp/tmux-%d" % os.getuid()
    try:
        return set(os.listdir(d))
    except OSError:
        return set()


def run_fixtures(run, names=None, trigger="manual"):
    man = run.man
    results = []
    lk = FileLock(run.p(".fixtures.lock"), blocking=False)
    if not lk.acquire():
        run.event("fixtures", "skipped", "fixtures already running")
        return results
    try:
        for fx in man["fixtures"]:
            if names and fx["name"] not in names:
                continue
            if sha256_file(fx["binary"]) != fx["sha256"]:
                auto_incident_once(run, "fixture_binary_changed", "critical", fx["name"] + " test binary changed")
                continue
            before = tmux_socket_names()
            started = time.time()
            log_rel = "fixtures/%s-%s.log" % (stamp(started), fx["name"])
            env, _ = base_env(man)
            env.update(fx.get("env", {}))
            rc, _, _, dur = run_cmd([fx["binary"]] + fx["args"], timeout=fx["timeoutSec"], env=env, cwd=fx["cwd"],
                                    log_path=run.p(log_rel))
            with open(run.p(log_rel), errors="replace") as f:
                parsed = parse_go_test(f.read())
            new_sockets = sorted(tmux_socket_names() - before)
            residue = []
            for name in new_sockets:
                if run_cmd(["tmux", "-L", name, "list-sessions"], 5)[0] == 0:
                    residue.append(name)
            ok = rc == 0 and parsed["fail"] == 0 and parsed["skip"] == 0 and parsed["pass"] > 0 and not parsed["panic"]
            rec = {"name": fx["name"], "rc": rc, "sec": round(dur, 1), "passed": parsed["pass"], "failed": parsed["failed"],
                   "skipped": parsed["skipped"], "panic": parsed["panic"], "liveTmuxResidue": residue}
            run.hash_file(log_rel)
            run.event("fixture_run", "pass" if ok and not residue else "fail", trigger=trigger, log=log_rel, **rec)
            results.append(rec)
            if not ok:
                open_incident(run, "fixture_failed", "high", "%s failed (rc=%s, failed=%s, skipped=%s)" % (
                    fx["name"], rc, parsed["failed"][:5], parsed["skipped"][:5]), evidence=[log_rel], restart_required=False)
    finally:
        lk.release()
    return results


# ----------------------------------------------------------------------------- daemon control (planned events)

def active_work(man):
    try:
        con, _ = db_open(os.path.join(man["ao"]["dataDir"], "ao.db"), True)
        try:
            runs = con.execute("SELECT COUNT(*) FROM workflow_runs WHERE state IN ('pending','running','waiting')").fetchone()[0]
            sess = con.execute("SELECT COUNT(*) FROM sessions WHERE is_terminated = 0").fetchone()[0]
        finally:
            con.close()
        return runs, sess
    except sqlite3.Error:
        return None, None


def daemon_start(run, reason="operator"):
    man = run.man
    st = daemon_status(man)
    if st.get("state") not in ("stopped", "stale"):
        run.event("daemon_started", "refused", "daemon state is %s" % st.get("state"), reason_=reason)
        die("refusing to start: daemon state is %s" % st.get("state"))
    holders = port_listeners(man["ao"]["port"])
    if holders:
        run.event("daemon_started", "refused", "port %d held by pid %s" % (man["ao"]["port"], holders))
        die("refusing to start: port %d is held by %s" % (man["ao"]["port"], holders))
    if sha256_file(man["ao"]["binary"]) != man["ao"]["binarySha256"]:
        auto_incident_once(run, "binary_under_test_changed", "critical", "frozen AO binary changed before start")
        die("frozen AO binary does not match the manifest")
    env = ao_env(man)
    _, source = base_env(man)
    log = open_private(run.p("process", "daemon.log"), os.O_WRONLY | os.O_CREAT | os.O_APPEND)
    t = time.monotonic()
    proc = subprocess.Popen([man["ao"]["binary"], "daemon"], stdin=subprocess.DEVNULL, stdout=log, stderr=log,
                            env=env, cwd=os.path.join(HOME, ".ao"), start_new_session=True, close_fds=True)
    os.close(log)
    ident = None
    while time.monotonic() - t < 180:
        if proc.poll() is not None:
            break
        st = daemon_status(man)
        if st.get("state") == "ready" and st.get("pid") == proc.pid:
            ident = daemon_identity(man, st)
            break
        time.sleep(1)
    boot = round(time.monotonic() - t, 1)
    if ident is None or ident["problems"]:
        run.event("daemon_started", "fail", "daemon did not become ready with a proven identity",
                  pid=proc.pid, exited=proc.poll(), sec=boot, problems=(ident or {}).get("problems"))
        die("daemon did not become ready (pid %s, exit %s); see %s" % (proc.pid, proc.poll(), run.p("process", "daemon.log")))

    def upd(s):
        s["expectInstance"] = ident["instanceId"]
        s.pop("maintenance", None)
    run.update_state(upd)
    run.event("daemon_started", "ok", pid=ident["pid"], instanceId=ident["instanceId"],
              installationId=ident["installationId"], dataDir=ident["dataDir"], port=ident["port"],
              sec=boot, envSource=source, trigger=reason)
    return ident


def daemon_stop(run, reason="operator"):
    man = run.man
    st = daemon_status(man)
    if st.get("state") != "ready":
        run.event("daemon_stopped", "refused", "daemon state is %s" % st.get("state"))
        return {"result": "refused", "state": st.get("state")}
    pid = st["pid"]
    open_maintenance(run, "stop:" + reason)
    rc, out, err, dur = ao_cmd(man, ["stop", "--json", "--timeout", "120s"], timeout=180)
    t = time.monotonic()
    while pid_alive(pid) and time.monotonic() - t < 60:
        time.sleep(1)
    after = daemon_status(man)
    res = {"rc": rc, "sec": round(dur, 1), "pidGone": not pid_alive(pid),
           "portReleased": not port_listeners(man["ao"]["port"]), "stateAfter": after.get("state"),
           "runFileRemaining": os.path.exists(man["ao"]["runFile"])}
    ok = rc == 0 and res["pidGone"] and res["portReleased"] and res["stateAfter"] in ("stopped", "stale")
    run.event("daemon_stopped", "pass" if ok else "fail", pid=pid, instanceId=st.get("instanceId"), trigger=reason, **res)
    res["result"] = "pass" if ok else "fail"
    return res


def quick_db(man):
    try:
        con, immutable = db_open(os.path.join(man["ao"]["dataDir"], "ao.db"), True)
        try:
            f, _ = db_facts(con, "quick")
        finally:
            con.close()
        return {"ok": f["check"]["ok"], "goose": f["goose"], "runsByState": f["runsByState"]}
    except sqlite3.Error as e:
        return {"ok": False, "error": str(e)[:200]}


def terminal_unchanged(before, after):
    return all(after.get("runsByState", {}).get(s, 0) >= before.get("runsByState", {}).get(s, 0) for s in TERMINAL)


def daemon_restart(run, reason="operator"):
    man = run.man
    before = daemon_status(man)
    if before.get("state") != "ready":
        run.event("daemon_restart", "refused", "daemon state is %s" % before.get("state"), trigger=reason)
        return {"result": "refused"}
    db_before = quick_db(man)
    stop = daemon_stop(run, reason)
    if stop.get("result") != "pass":
        run.event("daemon_restart", "fail", "graceful stop did not complete", trigger=reason, stop=stop)
        return {"result": "fail", "stop": stop}
    ident = daemon_start(run, reason)
    db_after = quick_db(man)
    checks = {"newInstance": ident["instanceId"] != before.get("instanceId"),
              "sameInstallation": ident["installationId"] == before.get("installationId"),
              "dbQuickCheck": db_after.get("ok"), "gooseSame": db_after.get("goose") == db_before.get("goose"),
              "terminalCountsNotDecreased": terminal_unchanged(db_before, db_after)}
    result = "pass" if all(checks.values()) else "fail"
    run.event("daemon_restart", result, trigger=reason, previousInstance=before.get("instanceId"),
              instanceId=ident["instanceId"], checks=checks)
    if result != "pass":
        open_incident(run, "daemon_restart_failed", "critical", "graceful restart validation failed: %s" % checks)
    return {"result": result, "checks": checks}


def daemon_crash(run, restart):
    """SIGKILL a daemon ONLY after proving it is the frozen binary serving this data dir,
    under this installation, as the incarnation the run-file and /healthz both name."""
    man = run.man
    st = daemon_status(man)
    if st.get("state") != "ready":
        die("refusing: daemon state is %s" % st.get("state"))
    ident = daemon_identity(man, st)
    pid = st["pid"]
    rc, cmdline, _, _ = run_cmd(["ps", "-o", "command=", "-p", str(pid)], 10)
    expected = "%s daemon" % man["ao"]["binary"]
    proof = {"identityProblems": ident["problems"], "commandMatches": cmdline.strip() == expected,
             "installationMatches": ident.get("installationId") == read_installation_id(man),
             "dataDirMatches": os.path.realpath(ident.get("dataDir") or "") == os.path.realpath(man["ao"]["dataDir"])}
    if ident["problems"] or not all(v for k, v in proof.items() if k != "identityProblems"):
        run.event("daemon_crash_injected", "refused", "identity not proven", pid=pid, proof=proof)
        die("refusing to signal pid %s: identity not proven %s" % (pid, proof))
    db_before = quick_db(man)
    open_maintenance(run, "controlled-crash")
    os.kill(pid, signal.SIGKILL)
    t = time.monotonic()
    while pid_alive(pid) and time.monotonic() - t < 30:
        time.sleep(0.5)
    after = daemon_status(man)
    run.event("daemon_crash_injected", "ok", pid=pid, instanceId=st.get("instanceId"), signal="SIGKILL",
              pidGone=not pid_alive(pid), stateAfter=after.get("state"), proof=proof)
    if not restart:
        return {"result": "injected"}
    ident2 = daemon_start(run, "post-crash")
    time.sleep(20)
    db_after = quick_db(man)
    cp = checkpoint(run, "post-crash", trigger="daemon-crash")
    checks = {"pidGone": not pid_alive(pid), "staleOrStoppedAfterCrash": after.get("state") in ("stale", "stopped"),
              "newInstance": ident2["instanceId"] != st.get("instanceId"),
              "sameInstallation": ident2["installationId"] == st.get("installationId"),
              "dbQuickCheck": db_after.get("ok"), "terminalCountsNotDecreased": terminal_unchanged(db_before, db_after),
              "postCrashCheckpointNotFailed": bool(cp) and cp.get("result") != "fail"}
    result = "pass" if all(checks.values()) else "fail"
    run.event("daemon_crash", result, checks=checks, previousInstance=st.get("instanceId"), instanceId=ident2["instanceId"])
    if result != "pass":
        open_incident(run, "controlled_crash_recovery_failed", "critical", "post-crash validation failed: %s" % checks)
    return {"result": result, "checks": checks}


# ----------------------------------------------------------------------------- electron event

ELECTRON_READY_SEC = 240        # app launch (forge build + window) until the daemon serves and the renderer asks
ELECTRON_QUIT_SEC = 60          # Ctrl+C to the dev process group until every process in it is gone
APP_DAEMON_EXIT_SEC = 60        # app gone until its app-owned daemon self-stops (supervisor EOF + grace)


def electron_bundle(checkout):
    return os.path.join(checkout, "frontend", "node_modules", "electron", "dist", "Electron.app")


def electron_processes(checkout):
    """PIDs running this checkout's Electron binary (the dev app), matched by full path, never by name."""
    bundle = electron_bundle(checkout)
    rc, out, _, _ = run_cmd(["ps", "-axo", "pid=,command="], 20)
    pids = []
    for line in out.splitlines():
        parts = line.strip().split(None, 1)
        if len(parts) == 2 and parts[0].isdigit() and parts[1].startswith(bundle):
            pids.append(int(parts[0]))
    return sorted(pids)


def electron_preflight(man, checkout):
    """Everything that must hold before the event touches the daemon. Returns a list of problems."""
    problems = []
    if not checkout or not os.path.isdir(os.path.join(checkout, "frontend")):
        return ["electron checkout %r has no frontend/" % checkout]
    rc, head, _, _ = run_cmd(["git", "-C", checkout, "rev-parse", "HEAD"], 20)
    if rc != 0 or head.strip() != man["eccSha"]:
        problems.append("checkout HEAD %s is not the frozen eccSha %s" % (head.strip() or "?", man["eccSha"]))
    rc, dirty, _, _ = run_cmd(["git", "-C", checkout, "status", "--porcelain", "--untracked-files=no", "--", "frontend"], 20)
    if rc != 0 or dirty.strip():
        problems.append("checkout frontend/ has uncommitted changes")
    framework = os.path.join(electron_bundle(checkout), "Contents", "Frameworks", "Electron Framework.framework")
    if not os.path.isdir(framework):
        problems.append("Electron is not fully installed in the checkout (%s missing)" % framework)
    if electron_processes(checkout):
        problems.append("this checkout's Electron app is already running")
    if sha256_file(man["ao"]["binary"]) != man["ao"]["binarySha256"]:
        problems.append("frozen AO binary does not match the manifest")
    return problems


def read_run_file(man):
    return read_json(man["ao"]["runFile"], None)


def electron_env(man):
    """The dev app's environment: the frozen data dir, run-file and port, and the frozen binary as the
    ONLY daemon command. Nothing else can start a daemon (no `go run`, no bundled binary)."""
    env = ao_env(man)
    env["AO_DAEMON_COMMAND"] = "%s daemon" % shlex.quote(man["ao"]["binary"])
    env.pop("AO_KEEP_DAEMON", None)
    return env


def electron_cycle(run, checkout, n, hold_sec, interactive):
    """One open -> use -> quit of the dev app. The harness daemon is already stopped, so the app spawns
    and owns the frozen daemon (owner=app) and that daemon must stop when the app quits."""
    man = run.man
    log_rel = "process/electron-%s-c%d.log" % (stamp(), n)
    log_fd = open_private(run.p(log_rel), os.O_WRONLY | os.O_CREAT | os.O_APPEND)
    t = time.monotonic()
    proc = subprocess.Popen(["npm", "run", "dev"], cwd=os.path.join(checkout, "frontend"), env=electron_env(man),
                            stdin=subprocess.DEVNULL, stdout=log_fd, stderr=log_fd, start_new_session=True, close_fds=True)
    os.close(log_fd)
    checks = {"appDaemonReady": False, "identityProven": False, "ownerApp": False, "rendererConnected": False,
              "noLockRefusal": False, "appQuit": False, "appDaemonExited": False, "portReleased": False}
    facts = {"cycle": n, "log": log_rel, "launcherPid": proc.pid}
    app_daemon_pid = None
    try:
        while time.monotonic() - t < ELECTRON_READY_SEC and proc.poll() is None:
            st = daemon_status(man)
            text = open(run.p(log_rel), errors="replace").read()
            if st.get("state") == "ready":
                ident = daemon_identity(man, st)
                rf = read_run_file(man) or {}
                app_daemon_pid = st.get("pid")
                checks["appDaemonReady"] = True
                checks["identityProven"] = not ident["problems"]
                checks["ownerApp"] = rf.get("owner") == "app" and bool(rf.get("appRunId")) and rf.get("pid") == st.get("pid")
                checks["rendererConnected"] = "path=/api/v1/auth/me" in text
                facts.update({"daemonPid": st.get("pid"), "instanceId": st.get("instanceId"),
                              "identityProblems": ident["problems"], "executablePath": ident.get("executablePath"),
                              "owner": rf.get("owner")})
                if checks["rendererConnected"]:
                    break
            time.sleep(2)
        facts["readySec"] = round(time.monotonic() - t, 1)
        if checks["rendererConnected"]:
            if interactive:
                print("electron-event cycle %d: the app is up. Use it, then quit it (Cmd+Q)." % n, flush=True)
                proc.wait(timeout=hold_sec)
            else:
                time.sleep(hold_sec)
    except subprocess.TimeoutExpired:
        facts["interactiveTimeout"] = True
    finally:
        if proc.poll() is None:
            try:
                os.killpg(proc.pid, signal.SIGINT)  # Ctrl+C to the dev process group, as an operator would
            except ProcessLookupError:
                pass
        q = time.monotonic()
        while (proc.poll() is None or electron_processes(checkout)) and time.monotonic() - q < ELECTRON_QUIT_SEC:
            time.sleep(1)
        checks["appQuit"] = proc.poll() is not None and not electron_processes(checkout)
        if not checks["appQuit"]:
            # Only the process group this cycle created (start_new_session), never by name.
            for sig in (signal.SIGTERM, signal.SIGKILL):
                try:
                    os.killpg(proc.pid, sig)
                except ProcessLookupError:
                    pass
                facts["forcedQuit"] = signal.Signals(sig).name
                q = time.monotonic()
                while (proc.poll() is None or electron_processes(checkout)) and time.monotonic() - q < 15:
                    time.sleep(1)
                if proc.poll() is not None and not electron_processes(checkout):
                    break
    if interactive:
        checks["quitByOperator"] = checks["rendererConnected"] and not facts.get("interactiveTimeout")
    text = open(run.p(log_rel), errors="replace").read()
    checks["noLockRefusal"] = "refusing to start" not in text
    if app_daemon_pid:
        q = time.monotonic()
        while pid_alive(app_daemon_pid) and time.monotonic() - q < APP_DAEMON_EXIT_SEC:
            time.sleep(1)
        checks["appDaemonExited"] = not pid_alive(app_daemon_pid)
    checks["portReleased"] = not port_listeners(man["ao"]["port"])
    facts["checks"] = checks
    return all(checks.values()), facts


def electron_event_minutes(man, cycles, hold_sec):
    """A window that covers the whole event: every cycle at its worst (launch, hold, quit, daemon exit)
    plus the harness stop/start and checkpoint. Shorter would let the event lapse mid-cycle."""
    per_cycle = ELECTRON_READY_SEC + hold_sec + 2 * ELECTRON_QUIT_SEC + APP_DAEMON_EXIT_SEC
    return max(man["thresholds"]["maintenanceWindowMin"], int(math.ceil((cycles * per_cycle + 600) / 60.0)))


def electron_event(run, checkout, cycles=2, hold_sec=20, interactive=False, minutes=None):
    """P11 electron close/reopen, deterministically: stop the soak daemon through the harness, open the
    dev app on the frozen data dir with the frozen binary as its only daemon command, use and quit it
    `cycles` times, then start the harness daemon again and checkpoint. Records electron_close_reopen,
    pass or fail, whatever happens (an exception is recorded as fail and re-raised)."""
    man = run.man
    problems = electron_preflight(man, checkout)
    st = daemon_status(man)
    if st.get("state") != "ready":
        problems.append("the soak daemon is %s, not ready" % st.get("state"))
    elif daemon_identity(man, st)["problems"]:
        problems.append("the soak daemon identity is not proven: %s" % daemon_identity(man, st)["problems"])
    needed = electron_event_minutes(man, cycles, hold_sec)
    if minutes is not None and minutes < needed:
        problems.append("--minutes %d is shorter than the event needs (%d)" % (minutes, needed))
    if problems:
        run.event("electron_event_refused", "refused", "; ".join(problems), checkout=checkout)
        die("electron-event refused: %s" % "; ".join(problems))
    minutes = minutes or needed
    begin_event(run, "electron", minutes)
    open_maintenance(run, "electron-event", minutes)
    run.event("electron_event_started", "ok", checkout=checkout, cycles=cycles, holdSec=hold_sec,
              interactive=interactive, maintenanceMin=minutes, previousInstance=st.get("instanceId"))
    results, restart, db, cp, error, app_left = [], None, {}, None, None, []
    try:
        try:
            stop = daemon_stop(run, "electron-event")
            open_maintenance(run, "electron-event", minutes)  # daemon_stop opened its own, shorter window
            if stop.get("result") != "pass":
                results.append({"cycle": 0, "checks": {"harnessDaemonStopped": False}, "stop": stop})
            else:
                for n in range(1, cycles + 1):
                    ok, facts = electron_cycle(run, checkout, n, hold_sec, interactive)
                    results.append(facts)
                    run.event("electron_cycle", "pass" if ok else "fail", **facts)
                    if not ok:
                        break
        except BaseException as e:  # noqa: BLE001 -- recorded as fail below, then re-raised
            error = e
        # Never hand the data dir back while this checkout's app may still be running: it would take the
        # restored daemon over again. Leave maintenance and the event open for the operator instead.
        app_left = electron_processes(checkout)
        if app_left:
            # Still owned (nothing scheduled acts), but only the operator's recovery is allowed now.
            run.update_state(lambda s: s["eventInProgress"].update({"kind": "electron-blocked"}))
            run.event("electron_event_blocked", "attention",
                      "the desktop app is still running; the soak daemon was NOT restarted. Quit it, then run "
                      "`p11 daemon-start` and `p11 checkpoint --label post-electron`", pids=app_left)
        else:
            # A daemon still serving here is only stopped through `ao stop` (never signalled), and only if
            # it is the frozen binary on this data dir.
            left = daemon_status(man)
            if left.get("state") == "ready" and not daemon_identity(man, left)["problems"]:
                ao_cmd(man, ["stop", "--json", "--timeout", "120s"], timeout=180)
            try:
                restart = daemon_start(run, "electron-event")
            except SystemExit as e:
                restart = {"error": str(e)}
            db = quick_db(man)
            # Before end_event: deferred scheduled items would otherwise take the checkpoint lock first.
            if isinstance(restart, dict) and restart.get("instanceId"):
                cp = checkpoint(run, "post-electron", trigger="electron-event")
    finally:
        if not app_left:
            end_event(run)
        checks = {"harnessDaemonStopped": bool(results) and results[0].get("cycle") != 0,
                  "allCyclesPassed": len(results) == cycles and all(all(r["checks"].values()) for r in results),
                  "appClosed": not app_left,
                  "harnessDaemonRestarted": isinstance(restart, dict) and bool(restart.get("instanceId")),
                  "dbQuickCheck": bool(db.get("ok")), "postCheckpointNotFailed": bool(cp) and cp.get("result") != "fail",
                  "noError": error is None}
        result = "pass" if all(checks.values()) else "fail"
        run.event("electron_close_reopen", result, "electron-event: %d/%d cycles%s" % (
            sum(1 for r in results if r.get("cycle") and all(r["checks"].values())), cycles,
            "; error: %s: %s" % (type(error).__name__, str(error)[:200]) if error else ""),
            checks=checks, cycles=results, checkout=checkout,
            instanceId=restart.get("instanceId") if isinstance(restart, dict) else None)
    if error is not None:
        raise error
    return {"result": result, "checks": checks, "cycles": results}


# ----------------------------------------------------------------------------- monitor

def due_items(schedule, elapsed_h, done):
    """Items whose time has come and that have not run. Overdue backups/fixtures of the same
    kind are coalesced into the latest one, so a long sleep does not queue three backups."""
    due = [it for it in schedule if elapsed_h is not None and elapsed_h >= it["atHours"] and it["id"] not in done]
    out, coalesced = [], []
    for it in due:
        heavy = [a for a in it["actions"] if a in ("backup", "fixtures", "daemon_restart")]
        later_same = [o for o in due if o is not it and o["atHours"] > it["atHours"]
                      and any(a in o["actions"] for a in heavy)]
        if heavy and later_same and "checkpoint" not in it["actions"]:
            coalesced.append(it)
        else:
            out.append(it)
    return out, coalesced


def run_scheduled(run, item):
    acts = item["actions"]
    man = run.man
    ev = event_in_progress(run.state())
    if ev:
        # An operator event owns the daemon right now; the item stays due and runs after it.
        return False
    if "daemon_restart" in acts:
        runs, sess = active_work(man)
        first = run.state().get("deferred", {}).get(item["id"])
        if (runs or sess) and (first is None or time.time() - first < man["thresholds"]["restartGuardDeferHours"] * 3600):
            if first is None:
                run.update_state(lambda s: s.setdefault("deferred", {}).update({item["id"]: time.time()}))
                run.event("planned_event_deferred", "ok", "active work present", item=item["id"], activeRuns=runs, liveSessions=sess)
            return False
        if runs or sess:
            run.event("planned_event_skipped", "attention", "active work never cleared", item=item["id"])
            return True
        run.event("planned_event", "ok", item=item["id"], action="daemon_restart")
        daemon_restart(run, "scheduled:" + item["id"])
    if "checkpoint" in acts:
        checkpoint(run, item["id"], with_backup="backup" in acts, trigger="scheduled")
    if "fixtures" in acts:
        run_fixtures(run, trigger="scheduled:" + item["id"])
    return True


def cmd_run(args):
    run = current_run(args)
    if os.path.exists(run.p("stopped.json")):
        return 0
    stop = {"flag": False}

    def on_term(signum, frame):
        stop["flag"] = True
    signal.signal(signal.SIGTERM, on_term)
    signal.signal(signal.SIGINT, on_term)
    run.event("monitor_started", pid=os.getpid(), harness=HARNESS_VERSION, harnessSha256=sha256_file(__file__))
    interval = run.man["heartbeatSeconds"]
    while not stop["flag"]:
        if os.path.exists(run.p("stopped.json")):
            break
        try:
            heartbeat(run, monitor_pid=os.getpid())
            post = run.state().get("postEventCheckpoint")
            if post and not event_in_progress(run.state()):
                run.update_state(lambda s: s.pop("postEventCheckpoint", None))
                checkpoint(run, post, trigger="event")
            done = run.state().get("done", {})
            items, coalesced = due_items(run.man["schedule"], run.elapsed_hours(), done)
            for it in coalesced:
                run.update_state(lambda s, i=it: s.setdefault("done", {}).update({i["id"]: "coalesced"}))
                run.event("planned_event_coalesced", item=it["id"])
            for it in items:
                if run_scheduled(run, it):
                    run.update_state(lambda s, i=it: s.setdefault("done", {}).update({i["id"]: iso()}))
        except SystemExit as e:
            run.event("monitor_error", "attention", str(e)[:300])
        except Exception as e:  # noqa: BLE001 -- the monitor records and keeps observing
            run.event("monitor_error", "attention", "%s: %s" % (type(e).__name__, str(e)[:300]))
        slept = 0
        while slept < interval and not stop["flag"]:
            time.sleep(5)
            slept += 5
    finished = os.path.exists(run.p("stopped.json"))
    run.event("monitor_stopped", "ok" if finished else "attention", pid=os.getpid(),
              reason="stage stopped" if finished else "signal")
    # launchd relaunches only on an unsuccessful exit (KeepAlive.SuccessfulExit=false): a signal
    # while the stage is live must not look like a clean finish, or observation silently ends.
    return 0 if finished else 75


def plist_path():
    return os.path.join(HOME, "Library", "LaunchAgents", LAUNCHD_LABEL + ".plist")


def monitor_program_args(harness, run_id):
    # --run is a top-level option: it must precede the subcommand.
    return ["/usr/bin/python3", harness, "--run", run_id, "run"]


def monitor_install(run):
    harness = run.p("harness", "p11.py")
    plist = {
        "Label": LAUNCHD_LABEL,
        "ProgramArguments": monitor_program_args(harness, run.id),
        "EnvironmentVariables": {"PATH": run.man["pathFloor"], "P11_ROOT": ROOT, "HOME": HOME},
        "RunAtLoad": True,
        "KeepAlive": {"SuccessfulExit": False},
        "ThrottleInterval": 30,
        "ProcessType": "Background",
        "AbandonProcessGroup": True,
        "WorkingDirectory": run.dir,
        "StandardOutPath": run.p("process", "monitor.out"),
        "StandardErrorPath": run.p("process", "monitor.out"),
    }
    ensure_dir(os.path.dirname(plist_path()))
    with os.fdopen(os.open(plist_path(), os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), "wb") as f:
        plistlib.dump(plist, f)
    uid = os.getuid()
    run_cmd(["launchctl", "bootout", "gui/%d/%s" % (uid, LAUNCHD_LABEL)], 20)
    rc, out, err, _ = run_cmd(["launchctl", "bootstrap", "gui/%d" % uid, plist_path()], 30)
    run.event("monitor_installed", "ok" if rc == 0 else "fail", (err or "")[-200:], label=LAUNCHD_LABEL)
    return rc == 0


def monitor_pid():
    rc, out, _, _ = run_cmd(["launchctl", "print", "gui/%d/%s" % (os.getuid(), LAUNCHD_LABEL)], 10)
    if rc != 0:
        return None
    m = re.search(r"^\s*pid = (\d+)", out, re.M)
    return int(m.group(1)) if m else 0


def monitor_uninstall(run):
    rc, _, err, _ = run_cmd(["launchctl", "bootout", "gui/%d/%s" % (os.getuid(), LAUNCHD_LABEL)], 30)
    try:
        os.unlink(plist_path())
    except OSError:
        pass
    run.event("monitor_uninstalled", "ok", (err or "")[-200:], rc=rc)


# ----------------------------------------------------------------------------- analysis / report

def memory_trend(samples, th):
    """samples: [(wall, instance, start_wall, rssKB)]. Pooled within-instance least-squares slope
    of hourly medians after warm-up. Returns PASS/UNKNOWN, never FAIL on its own (a leak BLOCKER
    needs attribution by a person)."""
    by_inst = {}
    for wall, inst, start, rss in samples:
        if rss is None or start is None or wall - start < th["rssWarmupMin"] * 60:
            continue
        by_inst.setdefault(inst, {}).setdefault(int((wall - start) // 3600), []).append(rss / 1024.0)
    sxy = sxx = 0.0
    span = 0.0
    first = last = None
    per = {}
    for inst, hours in by_inst.items():
        pts = sorted((h, sorted(v)[len(v) // 2]) for h, v in hours.items())
        per[inst[-12:] if inst else "?"] = [[h, round(m, 1)] for h, m in pts]
        if first is None and pts:
            first = pts[0][1]
        if pts:
            last = pts[-1][1]
        if len(pts) >= 2:
            span += pts[-1][0] - pts[0][0]
            mx = sum(h for h, _ in pts) / len(pts)
            my = sum(m for _, m in pts) / len(pts)
            sxy += sum((h - mx) * (m - my) for h, m in pts)
            sxx += sum((h - mx) ** 2 for h, _ in pts)
    slope = sxy / sxx if sxx else None
    res = {"slopeMBPerHour": round(slope, 2) if slope is not None else None, "firstMedianMB": first,
           "lastMedianMB": last, "postWarmupSpanHours": span, "hourlyMediansByInstance": per}
    if span < th["rssMinSpanHours"] or slope is None:
        res["verdict"], res["why"] = "UNKNOWN", "insufficient post-warm-up evidence"
    elif slope > th["rssSlopeMBPerHour"] or (first and last and last > th["rssGrowthFactor"] * first):
        res["verdict"], res["why"] = "UNKNOWN", "growth above the frozen review threshold; needs attribution"
    else:
        res["verdict"], res["why"] = "PASS", "within frozen thresholds"
    return res


def rss_samples(run):
    starts = {}
    for e in run.events("daemon_started"):
        if e.get("result") == "ok":
            starts[e["instanceId"][-12:]] = datetime.strptime(e["ts"], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc).timestamp()
    out = []
    for e in run.events("heartbeat"):
        d = e.get("d") or {}
        if d.get("st") != "ready":
            continue
        wall = datetime.strptime(e["ts"], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc).timestamp()
        out.append((wall, d.get("inst"), starts.get(d.get("inst")), d.get("rss")))
    return out


def crit(status, evidence="", note=""):
    return {"status": status, "evidence": evidence, "note": note}


def count_ok(events, etype, result="pass"):
    return sum(1 for e in events if e.get("type") == etype and e.get("result") == result)


def evaluate(run, now=None):
    man = run.man
    th = man["thresholds"]
    req = man["required"]
    ev = run.events()
    t0 = run.t0()
    now = now or time.time()
    c = {}
    elapsed = run.elapsed_hours(now)
    stopped = read_json(run.p("stopped.json")) or {}
    end_wall = stopped.get("finalizedWall") or now
    unknown = [e for e in ev if e.get("type") in ("observed_clock_gap",) or
               (e.get("type") == "sleep_wake_detected" and e.get("unknownSec"))]
    max_unknown = max([e.get("unknownSec", 0) for e in unknown] or [0])
    jumps = [e for e in ev if e.get("type") == "clock_jump"]
    monitor_gaps = [e for e in ev if e.get("type") == "monitor_restart"]
    if not t0:
        c["duration"] = crit("NOT_STARTED")
    else:
        hours = (end_wall - t0["wall"]) / 3600.0
        if jumps:
            c["duration"] = crit("UNKNOWN", "%d clock_jump events" % len(jumps), "wall clock moved; a person decides")
        elif max_unknown > th["unknownWindowMaxMin"] * 60:
            c["duration"] = crit("UNKNOWN", "longest unexplained window %ds" % max_unknown)
        elif hours + 1e-9 >= man["targetHours"]:
            c["duration"] = crit("PASS", "%.2fh >= %sh" % (hours, man["targetHours"]))
        else:
            c["duration"] = crit("IN_PROGRESS" if not stopped else "FAIL", "%.2fh of %sh" % (hours, man["targetHours"]))
    incs = incidents(run)
    open_crit = [i for i in incs if i["severity"] == "critical" and i["status"] == "open"]
    open_high = [i for i in incs if i["severity"] == "high" and i["status"] == "open"]
    c["noCriticalIncident"] = crit("FAIL" if open_crit else ("UNKNOWN" if open_high else "PASS"),
                                   ",".join(i["incidentId"] for i in open_crit + open_high),
                                   "open high-severity incidents need a disposition" if open_high and not open_crit else "")
    n = count_ok(ev, "daemon_restart")
    fails = count_ok(ev, "daemon_restart", "fail")
    c["daemonRestart"] = crit("FAIL" if fails else ("PASS" if n >= req.get("daemonRestart", 0) else "UNKNOWN"), "%d pass, %d fail" % (n, fails))
    n = count_ok(ev, "daemon_crash")
    fails = count_ok(ev, "daemon_crash", "fail")
    c["controlledCrash"] = crit("FAIL" if fails else ("PASS" if n >= req.get("daemonCrash", 0) else "UNKNOWN"), "%d pass, %d fail" % (n, fails))
    fx = [e for e in ev if e.get("type") == "fixture_run"]
    need = req.get("fixturePasses", req.get("fixtureRuns", 1))
    for name, key in (("p9-daemon-e2e", "workerRecoveryDaemonE2E"), ("p9-lifecycle", "workerRecoveryLifecycle"),
                      ("p9-tmux-e2e", "runtimeFactsTmuxE2E"), ("p10-daemon-e2e", "backupRestoreE2E")):
        p = sum(1 for e in fx if e.get("name") == name and e.get("result") == "pass")
        f = sum(1 for e in fx if e.get("name") == name and e.get("result") == "fail")
        c[key] = crit("FAIL" if f else ("PASS" if p >= need else "UNKNOWN"), "%d pass, %d fail" % (p, f))
    cps = [e for e in ev if e.get("type") == "checkpoint" and e.get("result") in ("ok", "attention", "fail")]
    codes = [code for e in cps for code in (e.get("findings") or [])]
    c["checkpoints"] = crit("PASS" if cps and not any(e["result"] == "fail" for e in cps) else ("FAIL" if cps else "UNKNOWN"),
                            "%d checkpoints, %d failed" % (len(cps), sum(1 for e in cps if e["result"] == "fail")))
    for key, bad in (("duplicateWorkers", ("duplicate_worker", "duplicate_dispatch")),
                     ("terminalImmutability", ("terminal_state_mutated", "terminal_run_step_reopened")),
                     ("orphanRuntime", ("orphan_runtime",)),
                     ("ownershipIdentity", ("installation_mismatch", "data_dir_mismatch", "binary_under_test_not_serving",
                                            "healthz_instance_mismatch", "binary_under_test_changed"))):
        hit = sum(1 for code in codes if code in bad)
        c[key] = crit("FAIL" if hit else ("PASS" if len(cps) >= 2 else "UNKNOWN"), "%d findings over %d checkpoints" % (hit, len(cps)))
    cycles = [e for e in ev if e.get("type") == "sleep_wake_detected" and e.get("countsAsCycle")]
    post_wake = [e for e in cps if str(e.get("label", "")).startswith("post-wake") and e["result"] != "fail"]
    if req.get("sleepWake", 0):
        c["sleepWake"] = crit("PASS" if len(cycles) >= req["sleepWake"] and len(post_wake) >= req["sleepWake"] else "UNKNOWN",
                              "%d cycles, %d healthy post-wake checkpoints" % (len(cycles), len(post_wake)))
    backups = [e for e in ev if e.get("type") == "backup_verified"]
    bad_b = [e for e in backups if e.get("status") != "VALID"]
    online = [e for e in ev if e.get("type") == "backup_created" and e.get("online") and e.get("result") == "ok"]
    c["backups"] = crit("FAIL" if bad_b else ("PASS" if len(online) >= req.get("onlineBackups", 0) and backups else "UNKNOWN"),
                        "%d verified, %d invalid, %d online" % (len(backups), len(bad_b), len(online)))
    if man["stage"] != "selftest":
        sr = [e for e in ev if e.get("type") == "scratch_restore"]
        c["scratchRestore"] = crit("FAIL" if any(e["result"] == "fail" for e in sr) else ("PASS" if sr else "UNKNOWN"), "%d runs" % len(sr))
        fin = [e for e in ev if e.get("type") == "final_db_check"]
        c["dbFinal"] = crit(("PASS" if fin[-1]["result"] == "pass" else "FAIL") if fin else "UNKNOWN")
    if req.get("electron"):
        el = [e for e in ev if e.get("type") == "electron_close_reopen"]
        c["electronCloseReopen"] = crit("FAIL" if any(e["result"] == "fail" for e in el) else
                                        ("PASS" if len(el) >= req["electron"] else "UNKNOWN"), "%d recorded" % len(el))
    if req.get("rebootAcrossP11"):
        reboots = sum(1 for rid in os.listdir(os.path.join(ROOT, "runs")) for e in Run(rid).events("reboot_detected")) \
            if os.path.isdir(os.path.join(ROOT, "runs")) else 0
        c["rebootAcrossP11"] = crit("PASS" if reboots else "UNKNOWN", "%d reboots observed across runs" % reboots)
    mt = memory_trend(rss_samples(run), th)
    c["memoryTrend"] = crit(mt["verdict"], "slope %s MB/h, first %s MB, last %s MB" % (mt["slopeMBPerHour"], mt["firstMedianMB"], mt["lastMedianMB"]), mt["why"])
    if man["stage"] == "selftest":
        hb = len([e for e in ev if e.get("type") == "heartbeat"])
        c["heartbeats"] = crit("PASS" if hb >= req["heartbeats"] else "UNKNOWN", "%d heartbeats" % hb)
        c["monitorRestart"] = crit("PASS" if len(monitor_gaps) >= req["monitorRestart"] else "UNKNOWN", "%d" % len(monitor_gaps))
        c["memoryTrend"]["status"] = "NOT_APPLICABLE"
    statuses = [v["status"] for v in c.values() if v["status"] != "NOT_APPLICABLE"]
    if "FAIL" in statuses:
        verdict = "FAIL"
    elif not t0:
        verdict = "NOT_STARTED"
    elif not stopped:
        verdict = "IN_PROGRESS"
    elif all(s == "PASS" for s in statuses):
        verdict = "PASS"
    else:
        verdict = "UNKNOWN"
    go = "NO-GO" if open_crit or "FAIL" in statuses else "GO"
    return {"verdict": verdict, "goNoGo": go, "criteria": c, "memory": mt, "elapsedHours": elapsed,
            "incidents": [{k: i[k] for k in ("incidentId", "code", "severity", "status", "symptom")} for i in incs]}


# ----------------------------------------------------------------------------- commands

def cmd_init(args):
    stage = STAGES[args.stage]
    ensure_dir(ROOT)
    ensure_dir(os.path.join(ROOT, "runs"))
    run_id = "run-%s-%s-%s" % (args.stage, stamp(), uuid.uuid4().hex[:6])
    rdir = ensure_dir(os.path.join(ROOT, "runs", run_id))
    for sub in ("checkpoints", "incidents", "process", "db", "backups", "fixtures", "scratch-restore", "harness"):
        ensure_dir(os.path.join(rdir, sub))
    harness = os.path.join(rdir, "harness", "p11.py")
    shutil.copyfile(os.path.abspath(__file__), harness)
    os.chmod(harness, 0o700)
    binary = os.path.abspath(args.ao_binary)
    data_dir = os.path.abspath(args.data_dir)
    fixtures = []
    src = os.path.abspath(args.src)
    for name, binname, rel, env, extra, timeout in (
            ("p9-tmux-e2e", "tmuxe2e.test", "internal/workerownership/tmuxe2e", {"AO_P9_REQUIRE_TMUX": "1"}, [], 600),
            ("p9-daemon-e2e", "daemone2e.test", "internal/workerownership/daemone2e", {"AO_P9_DAEMON_E2E": "1"}, [], 1200),
            ("p9-lifecycle", "workflow.test", "internal/workflow", {}, ["-test.run", "^TestP9"], 1800),
            ("p10-daemon-e2e", "backup-e2e.test", "internal/backup/e2e", {"AO_P10_E2E": "1"}, [], 1200)):
        path = os.path.join(os.path.abspath(args.tests_dir), binname)
        fixtures.append({"name": name, "binary": path, "sha256": sha256_file(path), "cwd": os.path.join(src, "backend", rel),
                         "env": env, "args": extra + ["-test.count=1", "-test.v"], "timeoutSec": timeout})
    man = {
        "runId": run_id, "stage": args.stage, "harnessVersion": HARNESS_VERSION, "harnessSha256": sha256_file(harness),
        "createdAt": iso(), "targetHours": args.target_hours if args.target_hours is not None else stage["targetHours"],
        "heartbeatSeconds": args.heartbeat_seconds or stage["heartbeatSeconds"],
        "schedule": stage["schedule"], "manualEvents": stage["manual"], "required": stage["required"],
        "thresholds": THRESHOLDS, "eccSha": args.ecc_sha,
        "ao": {"binary": binary, "binarySha256": sha256_file(binary), "dataDir": data_dir,
               "runFile": os.path.abspath(args.run_file), "port": args.port,
               "tmuxSocket": "ao-" + hashlib.sha256(data_dir.encode()).hexdigest()[:12],
               "extraEnv": dict(kv.split("=", 1) for kv in (args.daemon_env or []))},
        "fixtures": fixtures, "srcSnapshot": src, "scratchPort": args.scratch_port,
        "scratchRestoreFlags": args.scratch_restore_flag or [],
        "electronPath": args.electron_path or "",
        "electronCheckout": os.path.abspath(args.electron_checkout) if args.electron_checkout else "",
        "pathFloor": args.path_floor, "loginShell": args.login_shell,
        "platform": {"system": os.uname().sysname, "release": os.uname().release, "machine": os.uname().machine,
                     "macos": run_cmd(["sw_vers", "-productVersion"], 10)[1].strip(),
                     "memBytes": int(sysctl("hw.memsize") or 0)},
        "baseline": {},
        "invariants": ["no duplicate worker or dispatch", "no terminal run mutated", "no DB corruption",
                       "no AO-owned orphan runtime", "daemon identity proven (installation, data dir, frozen binary)",
                       "every P11 backup VALID", "scratch restore of the last backup passes",
                       "no silent recovery failure (fixtures pass)", "memory trend within frozen thresholds"],
        "limitations": ["The shipped daemon cannot run a worker/review/Fix lifecycle without a real LLM (no registered "
                        "fake harness or reviewer). Worker lifecycle, crash recovery, late completion, runtime-unavailable "
                        "and Fix liveness are exercised by the frozen P9/P10 E2E and lifecycle fixtures on scratch data "
                        "dirs; the live daemon soak covers time, restarts, crash, sleep/wake, backups, DB, runtime and memory."],
    }
    write_json(os.path.join(rdir, "manifest.json"), man)
    with open(os.path.join(ROOT, "current.tmp"), "w") as f:
        f.write(run_id + "\n")
    os.replace(os.path.join(ROOT, "current.tmp"), os.path.join(ROOT, "current"))
    run = Run(run_id)
    run.hash_file("manifest.json")
    run.event("run_initialized", stage=args.stage, targetHours=man["targetHours"])
    print(run_id)
    return 0


def cmd_baseline(args):
    run = current_run(args)
    man = run.man
    st = daemon_status(man)
    if st.get("state") not in ("stopped", "stale"):
        die("baseline needs AO stopped (state %s)" % st.get("state"))
    db = os.path.join(man["ao"]["dataDir"], "ao.db")
    con, immutable = db_open(db, False)
    try:
        f, _ = db_facts(con, "full")
    finally:
        con.close()
    f.pop("runs", None)
    base = {"label": args.label, "at": iso(), "dbFiles": db_files(man), "dbSha256": sha256_file(db),
            "dbReadImmutable": immutable, "db": f, "installationId": read_installation_id(man),
            "system": system_stats(), "daemon": st, "tmux": tmux_scan(man, {}, read_installation_id(man)),
            "eccSha": man["eccSha"], "binarySha256": sha256_file(man["ao"]["binary"])}
    rel = run.evidence("db/baseline-%s.json" % args.label, base)
    ok = f["check"]["ok"] and f["foreignKeyViolations"] == 0
    if args.label == "t0":
        man["baseline"] = {"goose": f["goose"], "dbSize": base["dbFiles"]["size"], "dbSha256": base["dbSha256"],
                           "runsByState": f["runsByState"], "file": rel}
        write_json(run.p("manifest.json"), man)
        run.hash_file("manifest.json")
    run.event("final_db_check" if args.label == "final" else "baseline", "pass" if ok else "fail", label=args.label, file=rel,
              integrity=f["check"]["ok"], fkViolations=f["foreignKeyViolations"], goose=f["goose"],
              sidecars={k: base["dbFiles"][k] for k in ("wal", "shm", "journal")}, restoreJournal=base["dbFiles"]["restoreJournal"])
    print(json.dumps({"result": "pass" if ok else "fail", "file": rel, "goose": f["goose"], "sha256": base["dbSha256"],
                      "integrity": f["check"], "fkViolations": f["foreignKeyViolations"]}, indent=2))
    return 0 if ok else 1


def cmd_backup(args):
    run = current_run(args)
    live = daemon_status(run.man).get("state") == "ready"
    res = do_backup(run, args.label, online=live)
    print(json.dumps(res, indent=2))
    return 0 if res.get("verifyStatus") == "VALID" else 1


def cmd_start(args):
    run = current_run(args)
    if run.t0():
        die("clock already started at %s" % run.t0()["at"])
    base = [e for e in run.events("baseline") if e.get("label") == "t0" and e.get("result") == "pass"]
    bk = [e for e in run.events("backup_verified") if e.get("status") == "VALID"]
    if not base:
        die("no passing t0 baseline (p11 baseline --label t0)")
    if not bk:
        die("no VALID t0 backup (p11 backup --label t0)")
    st = daemon_status(run.man)
    ident = daemon_identity(run.man, st)
    if st.get("state") != "ready" or ident["problems"]:
        die("daemon not ready with a proven identity: %s %s" % (st.get("state"), ident["problems"]))
    clk = clock_sample()
    t0 = {"at": iso(clk["wall"]), "wall": clk["wall"], "clock": clk, "daemon": ident,
          "expectedEnd": iso(clk["wall"] + run.man["targetHours"] * 3600), "t0BackupPath": bk[-1]["path"]}
    write_json(run.p("t0.json"), t0)
    run.hash_file("t0.json")
    run.update_state(lambda s: s.update({"daemonInstance": ident["instanceId"], "expectInstance": ident["instanceId"],
                                         "lastClock": clk}))
    run.event("stage_started", stage=run.man["stage"], targetHours=run.man["targetHours"], expectedEnd=t0["expectedEnd"],
              instanceId=ident["instanceId"], installationId=ident["installationId"])
    if not args.no_monitor:
        if not monitor_install(run):
            die("launchd bootstrap failed; see events")
    print(json.dumps({"runId": run.id, "t0": t0["at"], "expectedEnd": t0["expectedEnd"]}, indent=2))
    return 0


def next_planned(run):
    done = run.state().get("done", {})
    el = run.elapsed_hours() or 0
    todo = [it for it in run.man["schedule"] if it["id"] not in done]
    if not todo:
        return None
    it = min(todo, key=lambda i: i["atHours"])
    t0 = run.t0()
    return {"id": it["id"], "actions": it["actions"], "atHours": it["atHours"],
            "at": iso(t0["wall"] + it["atHours"] * 3600) if t0 else None, "inHours": round(it["atHours"] - el, 2)}


def cmd_status(args):
    run = current_run(args)
    ev = evaluate(run)
    st = run.state()
    hbs = run.events("heartbeat")
    cps = run.events("checkpoint")
    t0 = run.t0()
    d = daemon_status(run.man)
    out = {
        "runId": run.id, "stage": run.man["stage"], "start": t0["at"] if t0 else None,
        "elapsedHours": ev["elapsedHours"], "targetHours": run.man["targetHours"],
        "expectedEnd": t0["expectedEnd"] if t0 else None,
        "lastHeartbeat": hbs[-1]["ts"] if hbs else None,
        "lastCheckpoint": {k: cps[-1].get(k) for k in ("ts", "label", "result", "findings")} if cps else None,
        "monitor": {"launchdPid": monitor_pid(), "stopped": os.path.exists(run.p("stopped.json"))},
        "daemon": {k: d.get(k) for k in ("state", "pid", "instanceId", "uptime")},
        "db": (hbs[-1].get("db") if hbs else None),
        "backup": (lambda b: {k: b.get(k) for k in ("ts", "status", "path")} if b else None)(
            (run.events("backup_verified") or [None])[-1]),
        "incidents": ev["incidents"],
        "memoryTrend": {k: ev["memory"].get(k) for k in ("verdict", "slopeMBPerHour", "firstMedianMB", "lastMedianMB")},
        "sleepCycles": st.get("sleepCycles", 0),
        "nextPlannedEvent": next_planned(run),
        "manualEvents": run.man["manualEvents"],
        "verdict": ev["verdict"], "goNoGo": ev["goNoGo"],
        "criteria": {k: v["status"] for k, v in ev["criteria"].items()},
    }
    if args.json:
        print(json.dumps(out, indent=2))
    else:
        for k, v in out.items():
            if isinstance(v, (dict, list)):
                v = json.dumps(v, sort_keys=True)
            print("%-18s %s" % (k.upper(), v))
    return 0


# After electron_event_blocked the operator quits the app and restores the soak daemon by hand.
BLOCKED_EVENT_RECOVERY = ("daemon-start", "checkpoint")


def refuse_during_event(run, what):
    ev = event_in_progress(run.state())
    if ev and not (ev.get("kind") == "electron-blocked" and what in BLOCKED_EVENT_RECOVERY):
        die("refusing %s: a %s event owns the daemon until %s" % (what, ev.get("kind"), iso(ev.get("expires"))))


def cmd_checkpoint(args):
    run = current_run(args)
    refuse_during_event(run, "checkpoint")
    cp = checkpoint(run, args.label, with_backup=args.backup, trigger="manual")
    if cp is None:
        return 1
    print(json.dumps({"result": cp["result"], "findings": cp["findings"], "durationSec": cp["durationSec"]}, indent=2))
    return 0 if cp["result"] != "fail" else 1


def cmd_event(args):
    run = current_run(args)
    rec = run.event(args.type, args.result, args.note or "", manual=True)
    print(json.dumps(rec))
    return 0


def cmd_maintenance(args):
    """Open a planned window (e.g. the Electron reopen, which replaces the daemon) so the
    expected instance change and downtime are classified as planned, not as incidents."""
    run = current_run(args)
    if args.close:
        close_maintenance(run)
        run.event("maintenance_closed", "ok", args.reason or "", manual=True)
        return 0
    open_maintenance(run, "manual:" + (args.reason or "unspecified"), minutes=args.minutes)
    run.event("maintenance_opened", "ok", args.reason or "", minutes=args.minutes, manual=True)
    return 0


def cmd_incident(args):
    run = current_run(args)
    if args.resolve:
        path = run.p("incidents", args.resolve + ".json")
        rec = read_json(path)
        if not rec:
            die("no incident %s" % args.resolve)
        rec["status"] = "resolved"
        rec["disposition"] = args.disposition or ""
        rec["resolvedAt"] = iso()
        run.evidence("incidents/%s.json" % args.resolve, rec)
        run.event("incident_resolved", "ok", args.disposition or "", incidentId=args.resolve)
        print(args.resolve)
        return 0
    if not args.code or not args.summary:
        die("--code and --summary are required", 2)
    print(open_incident(run, args.code, args.severity, args.summary, invariant=args.invariant or "", source="operator",
                        restart_required=args.restart_required))
    return 0


def cmd_fixtures(args):
    run = current_run(args)
    refuse_during_event(run, "fixtures")
    res = run_fixtures(run, names=args.name, trigger="manual")
    print(json.dumps(res, indent=2))
    return 0 if res and all(not r["failed"] and not r["skipped"] and r["rc"] == 0 for r in res) else 1


def cmd_daemon(args):
    run = current_run(args)
    refuse_during_event(run, "daemon-" + args.daemon_cmd)
    if args.daemon_cmd == "start":
        ev = event_in_progress(run.state())
        if ev and ev.get("kind") == "electron-blocked" and electron_processes(run.man.get("electronCheckout", "")):
            die("refusing daemon-start: the desktop app is still running; quit it first")
        print(json.dumps(daemon_start(run), indent=2))
        if ev and ev.get("kind") == "electron-blocked":
            end_event(run)
            run.event("electron_event_recovered", "ok", "soak daemon restored by the operator after a blocked electron-event")
    elif args.daemon_cmd == "stop":
        res = daemon_stop(run)
        print(json.dumps(res, indent=2))
        return 0 if res.get("result") == "pass" else 1
    elif args.daemon_cmd == "restart":
        res = daemon_restart(run)
        print(json.dumps(res, indent=2))
        return 0 if res.get("result") == "pass" else 1
    elif args.daemon_cmd == "crash":
        if not args.confirm:
            die("daemon-crash sends SIGKILL to the proven soak daemon; pass --confirm", 2)
        res = daemon_crash(run, args.restart)
        print(json.dumps(res, indent=2))
        return 0 if res.get("result") in ("pass", "injected") else 1
    return 0


def cmd_electron_event(args):
    run = current_run(args)
    checkout = os.path.abspath(args.checkout) if args.checkout else run.man.get("electronCheckout", "")
    hold = args.hold if args.hold is not None else (1800 if args.interactive else 20)
    res = electron_event(run, checkout, cycles=args.cycles, hold_sec=hold, interactive=args.interactive,
                         minutes=args.minutes)
    print(json.dumps({k: res[k] for k in ("result", "checks")}, indent=2))
    return 0 if res["result"] == "pass" else 1


def cmd_scratch_restore(args):
    run = current_run(args)
    rep = scratch_restore(run, args.backup)
    print(json.dumps({"result": rep["result"], "checks": rep["checks"], "sec": rep["restoreSec"]}, indent=2))
    return 0 if rep["result"] == "pass" else 1


def cmd_monitor(args):
    run = current_run(args)
    if args.monitor_cmd == "install":
        return 0 if monitor_install(run) else 1
    if args.monitor_cmd == "uninstall":
        monitor_uninstall(run)
        return 0
    if args.monitor_cmd == "heartbeat":
        print(json.dumps(heartbeat(run), indent=2))
        return 0
    return 0


def cmd_finalize(args):
    run = current_run(args)
    man = run.man
    if not run.t0():
        die("stage never started")
    el = run.elapsed_hours()
    if el < man["targetHours"] and not args.abort:
        die("elapsed %.2fh < target %sh; finalize refuses (use --abort to end the stage as not passed)" % (el, man["targetHours"]))
    steps = {}
    done = run.state().get("done", {})
    last_cp = man["schedule"][-1]["id"]
    if not args.abort and last_cp not in done:
        steps["finalCheckpoint"] = (checkpoint(run, last_cp + "-at-finalize", with_backup=True, trigger="finalize") or {}).get("result")
    st = daemon_status(man)
    if st.get("state") == "ready":
        steps["gracefulStop"] = daemon_stop(run, "finalize").get("result")
    else:
        steps["gracefulStop"] = "not_running:%s" % st.get("state")
    monitor_uninstall(run)
    write_json(run.p("stopped.json"), {"at": iso(), "finalizedWall": time.time(), "abort": bool(args.abort), "elapsedHours": el})
    ns = argparse.Namespace(run=run.id, label="final")
    try:
        steps["finalDb"] = cmd_baseline(ns)
    except SystemExit as e:
        steps["finalDb"] = "error: %s" % e
    valid = [e for e in run.events("backup_verified") if e.get("status") == "VALID"]
    if valid:
        steps["reverifyLastBackup"] = verify_backup(run, valid[-1]["path"], "final-reverify").get("verifyStatus")
        steps["scratchRestore"] = scratch_restore(run, valid[-1]["path"])["result"]
    run.event("stage_complete" if not args.abort else "stage_aborted", "ok", elapsedHours=el, steps=steps)
    return cmd_report(argparse.Namespace(run=run.id, json=False))


def cmd_report(args):
    run = current_run(args)
    ev = evaluate(run)
    ev.update({"runId": run.id, "stage": run.man["stage"], "generatedAt": iso(), "eccSha": run.man["eccSha"],
               "binarySha256": run.man["ao"]["binarySha256"], "t0": (run.t0() or {}).get("at"),
               "limitations": run.man["limitations"], "platform": run.man["platform"],
               "counts": {t: len(run.events(t)) for t in ("heartbeat", "checkpoint", "daemon_restart", "daemon_crash",
                                                         "sleep_wake_detected", "reboot_detected", "monitor_restart",
                                                         "fixture_run", "backup_verified", "scratch_restore",
                                                         "observed_clock_gap", "clock_jump", "incident")}})
    rel = run.evidence("final-report.json" if os.path.exists(run.p("stopped.json")) else "report-%s.json" % stamp(), ev)
    if getattr(args, "json", False):
        print(json.dumps(ev, indent=2))
    else:
        print("RUN %s  STAGE %s  VERDICT %s  %s" % (run.id, run.man["stage"], ev["verdict"], ev["goNoGo"]))
        for k, v in ev["criteria"].items():
            print("  %-26s %-12s %s %s" % (k, v["status"], v["evidence"], ("(" + v["note"] + ")") if v["note"] else ""))
        print("report: " + run.p(rel))
    return 0


def main(argv=None):
    args = build_parser().parse_args(argv)
    os.umask(0o077)
    return args.fn(args) or 0


def build_parser():
    ap = argparse.ArgumentParser(prog="p11", description="AO P11 reliability soak harness (observe, record, planned events only)")
    ap.add_argument("--run", help="run id (default: $P11_ROOT/current)")
    sub = ap.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("init")
    p.add_argument("--stage", choices=sorted(STAGES), required=True)
    p.add_argument("--target-hours", type=float)
    p.add_argument("--heartbeat-seconds", type=int)
    p.add_argument("--ao-binary", required=True)
    p.add_argument("--ecc-sha", required=True)
    p.add_argument("--data-dir", default=os.path.join(HOME, ".ao", "data"))
    p.add_argument("--run-file", default=os.path.join(HOME, ".ao", "dev", "running.json"))
    p.add_argument("--port", type=int, default=3002)
    p.add_argument("--scratch-port", type=int, default=3019)
    p.add_argument("--tests-dir", required=True)
    p.add_argument("--src", required=True, help="frozen source snapshot root (contains backend/)")
    p.add_argument("--daemon-env", action="append", help="extra KEY=VALUE for the daemon (frozen)")
    p.add_argument("--scratch-restore-flag", action="append")
    p.add_argument("--electron-path")
    p.add_argument("--electron-checkout", help="repo checkout at --ecc-sha whose frontend electron-event launches")
    p.add_argument("--path-floor", default="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin")
    p.add_argument("--login-shell", default=pwd.getpwuid(os.getuid()).pw_shell or "/bin/zsh")
    p.set_defaults(fn=cmd_init)

    p = sub.add_parser("baseline")
    p.add_argument("--label", default="t0")
    p.set_defaults(fn=cmd_baseline)
    p = sub.add_parser("backup")
    p.add_argument("--label", required=True)
    p.set_defaults(fn=cmd_backup)
    p = sub.add_parser("start")
    p.add_argument("--no-monitor", action="store_true")
    p.set_defaults(fn=cmd_start)
    p = sub.add_parser("run")
    p.set_defaults(fn=cmd_run)
    p = sub.add_parser("status")
    p.add_argument("--json", action="store_true")
    p.set_defaults(fn=cmd_status)
    p = sub.add_parser("checkpoint")
    p.add_argument("--label", default="manual")
    p.add_argument("--backup", action="store_true")
    p.set_defaults(fn=cmd_checkpoint)
    p = sub.add_parser("event")
    p.add_argument("type")
    p.add_argument("--result", default="ok", choices=("ok", "pass", "fail", "attention"))
    p.add_argument("--note")
    p.set_defaults(fn=cmd_event)
    p = sub.add_parser("maintenance")
    p.add_argument("--reason")
    p.add_argument("--minutes", type=int, default=30)
    p.add_argument("--close", action="store_true")
    p.set_defaults(fn=cmd_maintenance)
    p = sub.add_parser("incident")
    p.add_argument("--code")
    p.add_argument("--severity", default="high", choices=("critical", "high", "attention"))
    p.add_argument("--summary")
    p.add_argument("--invariant")
    p.add_argument("--restart-required", action="store_true", default=None)
    p.add_argument("--resolve", metavar="INCIDENT_ID")
    p.add_argument("--disposition")
    p.set_defaults(fn=cmd_incident)
    p = sub.add_parser("fixtures")
    p.add_argument("--name", action="append")
    p.set_defaults(fn=cmd_fixtures)
    for name in ("start", "stop", "restart", "crash"):
        p = sub.add_parser("daemon-" + name)
        p.set_defaults(fn=cmd_daemon, daemon_cmd=name)
        if name == "crash":
            p.add_argument("--confirm", action="store_true")
            p.add_argument("--restart", action="store_true")
    p = sub.add_parser("scratch-restore")
    p.add_argument("--backup")
    p.set_defaults(fn=cmd_scratch_restore)
    for name in ("install", "uninstall", "heartbeat"):
        p = sub.add_parser("monitor-" + name)
        p.set_defaults(fn=cmd_monitor, monitor_cmd=name)
    p = sub.add_parser("electron-event")
    p.add_argument("--checkout", help="default: the manifest's electronCheckout")
    p.add_argument("--cycles", type=int, default=2)
    p.add_argument("--hold", type=int, help="seconds the app stays open per cycle (default 20; --interactive: max wait, default 1800)")
    p.add_argument("--interactive", action="store_true", help="wait for the operator to quit the app each cycle")
    p.add_argument("--minutes", type=int)
    p.set_defaults(fn=cmd_electron_event)
    p = sub.add_parser("finalize")
    p.add_argument("--abort", action="store_true")
    p.set_defaults(fn=cmd_finalize)
    p = sub.add_parser("report")
    p.add_argument("--json", action="store_true")
    p.set_defaults(fn=cmd_report)
    return ap


if __name__ == "__main__":
    sys.exit(main())
