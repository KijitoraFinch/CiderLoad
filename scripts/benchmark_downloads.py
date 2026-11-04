#!/usr/bin/env python3
"""Compare real CDN downloads; retain measurements and delete all payloads.

Metadata must be Homebrew formula/cask API JSON snapshots in metadata/.
Network runs are serial. Every attempt has fresh output and coordinator state.
Process time includes startup, redirects, transfer and finalization; SHA-256
verification is measured separately. Remote CDN and OS caches are not flushed.
"""

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import platform
import signal
import statistics
import subprocess
import tempfile
import time


def digest(path):
    h = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(8 * 1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()


def clean_env():
    env = os.environ.copy()
    for key in list(env):
        if key.startswith(("HOMEBREW_", "CIDERLOAD_", "CIDL_")) or key == "_CIDERLOAD_DELEGATING":
            del env[key]
    # Respect proxy/certificate settings. Trace validation will catch delegation.
    return env


def load_profiles(path, binary, modes=None, min_size_mib=40):
    """Profile thresholds describe the built binaries; they do not configure them."""
    if path:
        profiles = json.loads(path.read_text())
        modes = modes or list(profiles)
    else:
        modes = modes or ["curl", "ciderload8"]
        profiles = {"curl": dict(engine="curl", binary="/usr/bin/curl")}
        for mode in modes:
            if mode == "curl":
                continue
            if not mode.startswith("ciderload") or not mode[9:].isdigit():
                raise ValueError(f"invalid mode: {mode}")
            profiles[mode] = dict(engine="ciderload", binary=str(binary),
                                  connections=int(mode[9:]), min_size_mib=min_size_mib)
    selected = {}
    for mode in modes:
        profile = profiles[mode].copy()
        if profile["engine"] not in ("curl", "ciderload"):
            raise ValueError(f"invalid engine: {mode}")
        executable = Path(profile["binary"]).resolve()
        if not executable.is_file() or not os.access(executable, os.X_OK):
            raise ValueError(f"executable missing: {executable}")
        profile["binary"] = str(executable)
        profile["binary_sha256"] = digest(executable)
        if profile["engine"] == "ciderload":
            if not 2 <= profile["connections"] <= 256 or profile["min_size_mib"] <= 0:
                raise ValueError(f"invalid connection count or threshold: {mode}")
        selected[mode] = profile
    return selected


def execute(command, env, logfile, timeout):
    with logfile.open("w") as log:
        start = time.monotonic()
        process = subprocess.Popen(command, env=env, stdout=log, stderr=subprocess.STDOUT,
                                   start_new_session=True)
        timed_out = False
        try:
            code = process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            timed_out = True
            kill_tree(process)
            code = process.wait()
        except BaseException:
            kill_tree(process)
            process.wait()
            raise
    return code, time.monotonic() - start, timed_out


def kill_tree(process):
    # Homebrew can put curl children in separate process groups. Collect the
    # descendants before killing the parent so they cannot outlive cleanup.
    try:
        listing = subprocess.check_output(["/bin/ps", "-axo", "pid=,ppid="], text=True)
        pairs = [tuple(map(int, line.split())) for line in listing.splitlines()]
    except (OSError, subprocess.CalledProcessError):
        pairs = []
    descendants = {process.pid}
    while True:
        expanded = descendants | {pid for pid, parent in pairs if parent in descendants}
        if expanded == descendants:
            break
        descendants = expanded
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    for pid in descendants - {process.pid}:
        try:
            os.kill(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument("--binary", type=Path, default=Path("build/ciderload"))
    parser.add_argument("--targets", nargs="+", default=["llvm", "texlive", "firefox"])
    parser.add_argument("--modes", nargs="+")
    parser.add_argument("--profiles", type=Path, help="JSON mapping mode names to engine/binary/threshold settings")
    parser.add_argument("--min-size-mib", type=int, default=40, help="expected threshold of --binary, for validation")
    parser.add_argument("--repeats", type=int, default=3)
    parser.add_argument("--timeout", type=int, default=900)
    parser.add_argument("--tag", default="default")
    args = parser.parse_args()
    root, binary = args.output_dir.resolve(), args.binary.resolve()
    result_path = root / f"downloads-{args.tag}.json"
    if result_path.exists():
        parser.error("results already exist; use a new tag or output directory")
    if args.repeats < 1 or not binary.is_file():
        parser.error("positive repeats and a built binary are required")
    try:
        profiles = load_profiles(args.profiles, binary, args.modes, args.min_size_mib)
    except (KeyError, ValueError, OSError) as error:
        parser.error(str(error))
    args.modes = list(profiles)
    targets = []
    for name in args.targets:
        source = root / "metadata" / f"{name}.json"
        data = json.loads(source.read_text())
        if "bottle" in data:
            bottle = data["bottle"]["stable"]["files"]["arm64_tahoe"]
            targets.append(dict(name=name, version=data["versions"]["stable"],
                                url=bottle["url"], sha256=bottle["sha256"],
                                authorization="Bearer QQ==", metadata_sha256=digest(source)))
        else:
            targets.append(dict(name=name, version=data["version"], url=data["url"],
                                sha256=data["sha256"], metadata_sha256=digest(source)))
    logs = root / f"logs-{args.tag}"
    logs.mkdir(parents=True)
    env = clean_env()
    result = dict(started_at=datetime.now(timezone.utc).isoformat(),
                  host=platform.platform(), binary_sha256=digest(binary),
                  curl_version=subprocess.check_output(["/usr/bin/curl", "--version"], text=True),
                  targets=targets, modes=args.modes, profiles=profiles, repeats=args.repeats,
                  timing="process wall time; SHA-256 excluded; fresh local files each run",
                  ordering="rotate mode order by repetition and target; sequential transfers",
                  payload_cleanup="after every attempt, including failures",
                  rows=[])
    result_path.write_text(json.dumps(result, indent=2) + "\n")
    for repeat in range(args.repeats):
        for index, target in enumerate(targets):
            offset = (repeat + index) % len(args.modes)
            order = args.modes[offset:] + args.modes[:offset]
            for mode in order:
                profile = profiles[mode]
                name = f"r{repeat + 1}-{target['name']}-{mode}"
                print(f"START {name}", flush=True)
                row = dict(target=target["name"], mode=mode, repeat=repeat + 1,
                           started_at=datetime.now(timezone.utc).isoformat(), log=f"{logs.name}/{name}.log")
                with tempfile.TemporaryDirectory(prefix="payload-", dir=root) as temporary:
                    work = Path(temporary)
                    output = work / "artifact"
                    runenv = env.copy()
                    runenv.update({"HOMEBREW_CIDERLOAD_STATE_DIR": str(work / "state"),
                                   "HOMEBREW_CIDERLOAD_TRACE": "1",
                                   "HOMEBREW_CIDERLOAD_TOTAL_CONNECTIONS": "16",
                                   "HOMEBREW_CIDERLOAD_PREPARE_CONNECTIONS": "4",
                                   "HOMEBREW_CIDERLOAD_CURL": "/usr/bin/curl"})
                    executable = profile["binary"]
                    if profile["engine"] == "ciderload":
                        runenv["HOMEBREW_CIDERLOAD_CONNECTIONS"] = str(profile["connections"])
                    command = [executable, "-q", "-fLsS", "--retry", "3", "--connect-timeout", "30",
                               "--cookie", "/dev/null", "--user-agent", "CiderLoad-benchmark/1",
                               "--output", str(output)]
                    if "authorization" in target:
                        command += ["--header", f"Authorization: {target['authorization']}"]
                    command.append(target["url"])
                    code, seconds, timed_out = execute(command, runenv, root / row["log"], args.timeout)
                    row.update(exit_code=code, seconds=round(seconds, 6), timed_out=timed_out)
                    trace = (root / row["log"]).read_text()
                    row["parallel_completed"] = "parallel download complete" in trace
                    row["sequential_completed"] = "sequential download complete" in trace
                    row["direct_get"] = "direct GET:" in trace
                    row["ordinary_curl"] = "response requires ordinary curl" in trace
                    if output.exists():
                        row["bytes"] = output.stat().st_size
                        start = time.monotonic()
                        row["sha256"] = digest(output)
                        row["hash_seconds"] = round(time.monotonic() - start, 6)
                        row["checksum_ok"] = row["sha256"] == target["sha256"]
                        row["mib_per_second"] = round(row["bytes"] / 1048576 / seconds, 3)
                    path_ok = profile["engine"] == "curl"
                    if profile["engine"] == "ciderload" and "bytes" in row:
                        row["parallel_expected"] = row["bytes"] >= profile["min_size_mib"] * 1048576
                        path_ok = row["parallel_completed"] if row["parallel_expected"] else (
                            (row["sequential_completed"] if profile.get("direct_get") else row["ordinary_curl"])
                            and not row["parallel_completed"])
                        if profile.get("direct_get"):
                            path_ok = path_ok and row["direct_get"] and not row["ordinary_curl"]
                    row["ok"] = code == 0 and row.get("checksum_ok", False) and path_ok
                row["payload_deleted"] = not work.exists()
                result["rows"].append(row)
                result_path.write_text(json.dumps(result, indent=2) + "\n")
                print(json.dumps(row), flush=True)
    result["finished_at"] = datetime.now(timezone.utc).isoformat()
    result["summary"] = []
    for target in targets:
        for mode in args.modes:
            rows = [r for r in result["rows"] if r["target"] == target["name"] and r["mode"] == mode]
            times = [r["seconds"] for r in rows if r["ok"]]
            result["summary"].append(dict(target=target["name"], mode=mode, attempts=len(rows),
                                          successes=len(times), median=statistics.median(times) if times else None,
                                          min=min(times) if times else None, max=max(times) if times else None))
    result_path.write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result["summary"], indent=2), flush=True)
    return 0 if all(row["ok"] for row in result["rows"]) else 1


if __name__ == "__main__":
    raise SystemExit(main())
