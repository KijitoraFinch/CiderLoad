#!/usr/bin/env python3
"""Measure real brew fetch/install in disposable prefixes and remove payloads.

Pass an existing API cache snapshot; package downloads are always fresh.
By default, fetch LLVM with its runtime dependencies and Go. A full Go
installation can be measured separately with --cases go-install.
TeX Live bottle fetches and Firefox, Docker Desktop, and Visual Studio Code
Cask fetches are also available through --cases.
"""

import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import shlex
import shutil
import statistics
import subprocess
import tempfile

from benchmark_downloads import clean_env, digest, execute, load_profiles


CASES = {
    "llvm-fetch": ["fetch", "--force-bottle", "llvm", "lz4", "xz", "zstd", "z3"],
    "go-fetch": ["fetch", "--force-bottle", "go"],
    "texlive-fetch": ["fetch", "--force-bottle", "texlive"],
    "go-install": ["install", "--force-bottle", "go"],
    "firefox-fetch": ["fetch", "--cask", "firefox"],
    "docker-desktop-fetch": ["fetch", "--cask", "docker-desktop"],
    "visual-studio-code-fetch": ["fetch", "--cask", "visual-studio-code"],
}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument("--api-cache", type=Path, required=True)
    parser.add_argument("--brew-repository", type=Path, default=Path("/opt/homebrew"))
    parser.add_argument("--binary", type=Path, default=Path("build/ciderload"))
    parser.add_argument("--cidl", type=Path, default=Path("build/cidl"))
    parser.add_argument("--profiles", type=Path)
    parser.add_argument("--modes", nargs="+")
    parser.add_argument("--cases", nargs="+", choices=CASES, default=["llvm-fetch", "go-fetch"])
    parser.add_argument("--repeats", type=int, default=3)
    parser.add_argument("--timeout", type=int, default=900)
    parser.add_argument("--resume", action="store_true", help="continue previously validated rows")
    args = parser.parse_args()
    if args.repeats < 1 or args.timeout <= 0:
        parser.error("repeats and timeout must be positive")
    root, repo, binary, api = (p.resolve() for p in
                              (args.output_dir, args.brew_repository, args.binary, args.api_cache))
    cidl = args.cidl.resolve()
    if not cidl.is_file():
        parser.error("build cidl first (make build)")
    try:
        profiles = load_profiles(args.profiles, binary, args.modes)
    except (KeyError, ValueError, OSError) as error:
        parser.error(str(error))
    result_path = root / "brew.json"
    if (result_path.exists() and not args.resume) or not api.is_dir():
        parser.error("results must be new and --api-cache must be an API cache directory")
    logs = root / "logs-brew"
    logs.mkdir(parents=True, exist_ok=args.resume)
    result = dict(started_at=datetime.now(timezone.utc).isoformat(),
                  brew_head=subprocess.check_output(["git", "-C", str(repo), "rev-parse", "HEAD"], text=True).strip(),
                  binary_sha256=digest(binary),
                  cidl_sha256=digest(cidl),
                  profiles=profiles,
                  api_snapshot={str(p.relative_to(api)): digest(p) for p in api.rglob("*") if p.is_file()},
                  cases={name: CASES[name] for name in args.cases},
                  timing="entire command (brew for curl, cidl for CiderLoad); checkout/cache setup, extra validation and deletion excluded",
                  download_concurrency=5, total_connections=16, prepare_connections=4,
                  rows=[])
    if args.resume:
        previous = json.loads(result_path.read_text())
        for key in ("brew_head", "binary_sha256", "cidl_sha256", "api_snapshot", "cases", "profiles"):
            if previous.get(key) != result[key]:
                parser.error(f"resume conditions differ: {key}")
        if not all(row["ok"] for row in previous["rows"]):
            parser.error("resume requires validated existing rows; retain failures in a separate result")
        result = previous
    result_path.write_text(json.dumps(result, indent=2) + "\n")
    for repeat in range(args.repeats):
        for case_index, case in enumerate(args.cases):
            modes = list(profiles)
            offset = (repeat + case_index) % len(modes)
            modes = modes[offset:] + modes[:offset]
            for mode in modes:
                profile = profiles[mode]
                label = f"r{repeat + 1}-{case}-{mode}"
                if any(r["repeat"] == repeat + 1 and r["case"] == case and r["mode"] == mode
                       for r in result["rows"]):
                    continue
                print(f"SETUP {label}", flush=True)
                row = dict(case=case, mode=mode, repeat=repeat + 1,
                           log=f"{logs.name}/{label}.log", trace=f"{logs.name}/{label}.trace")
                with tempfile.TemporaryDirectory(prefix="brew-work-", dir=root) as temporary:
                    work = Path(temporary)
                    prefix = work / "prefix"
                    subprocess.run(["git", "clone", "--quiet", "--shared", str(repo), str(prefix)], check=True)
                    shutil.rmtree(prefix / "Library/Homebrew/vendor")
                    shutil.copytree(repo / "Library/Homebrew/vendor", prefix / "Library/Homebrew/vendor", symlinks=True)
                    shutil.copytree(api, work / "cache/api", symlinks=True)
                    (work / "temp").mkdir()
                    # Retain the engine trace: Homebrew's parallel queue normally
                    # hides stderr from successful jobs. Native brew selects curl.
                    wrapper = work / "curl-wrapper"
                    executable = profile["binary"]
                    wrapper.write_text("#!/bin/sh\nexec " + shlex.quote(executable) +
                                       ' "$@" 2>>' + shlex.quote(str(root / row["trace"])) + "\n")
                    wrapper.chmod(0o755)
                    env = clean_env()
                    env.update({
                        "HOMEBREW_NO_AUTO_UPDATE": "1", "HOMEBREW_NO_ANALYTICS": "1",
                        "HOMEBREW_NO_INSTALL_CLEANUP": "1", "HOMEBREW_NO_ENV_HINTS": "1",
                        "HOMEBREW_API_AUTO_UPDATE_SECS": "86400000",
                        "HOMEBREW_CACHE": str(work / "cache"), "HOMEBREW_LOGS": str(work / "logs"),
                        "HOMEBREW_TEMP": str(work / "temp"), "XDG_CONFIG_HOME": str(work / "config"),
                        "HOMEBREW_CURL_PATH": str(wrapper), "HOMEBREW_DOWNLOAD_CONCURRENCY": "5",
                        "CIDL_BREW": str(prefix / "bin/brew"), "CIDL_DOWNLOADER": str(wrapper),
                        "HOMEBREW_CIDERLOAD_CURL": "/usr/bin/curl",
                        "HOMEBREW_CIDERLOAD_STATE_DIR": str(work / "state"),
                        "HOMEBREW_CIDERLOAD_TRACE": "1",
                        "HOMEBREW_CIDERLOAD_CONNECTIONS": str(profile.get("connections", 8)),
                        "HOMEBREW_CIDERLOAD_TOTAL_CONNECTIONS": "16",
                        "HOMEBREW_CIDERLOAD_PREPARE_CONNECTIONS": "4",
                    })
                    if "--cask" in CASES[case]:
                        env["HOMEBREW_CASK_OPTS"] = "--language=en"
                        row["cask_language"] = "en"
                    launcher = prefix / "bin/brew" if profile["engine"] == "curl" else cidl
                    command = [str(launcher), *CASES[case]]
                    row["command"] = command
                    print(f"START {label}", flush=True)
                    row["started_at"] = datetime.now(timezone.utc).isoformat()
                    code, seconds, timed_out = execute(command, env, root / row["log"], args.timeout)
                    row.update(exit_code=code, seconds=round(seconds, 6), timed_out=timed_out)
                    trace = root / row["trace"]
                    row["parallel_completions"] = trace.read_text().count("parallel download complete") if trace.exists() else 0
                    row["downloads"] = [dict(name=p.name, bytes=p.stat().st_size, sha256=digest(p))
                                         for p in sorted((work / "cache/downloads").glob("*"))
                                         if p.is_file() and not p.is_symlink()]
                    row["ok"] = code == 0 and (profile["engine"] == "curl" or row["parallel_completions"] > 0)
                    row["bottle_count"] = sum(".bottle." in d["name"] and d["name"].endswith(".tar.gz")
                                              for d in row["downloads"])
                    if "--cask" in CASES[case]:
                        row["cask_count"] = sum(d["name"].endswith((".dmg", ".zip")) for d in row["downloads"])
                        row["ok"] = row["ok"] and row["cask_count"] == 1
                    else:
                        row["ok"] = row["ok"] and row["bottle_count"] == (5 if case == "llvm-fetch" else 1)
                    if code == 0 and case == "go-install":
                        check = subprocess.run([str(prefix / "opt/go/bin/go"), "version"],
                                               capture_output=True, text=True, env=env, timeout=30)
                        row["installed_version"] = check.stdout.strip()
                        row["ok"] = row["ok"] and check.returncode == 0
                        row["install_receipts"] = [json.loads(p.read_text()) for p in
                                                   (prefix / "Cellar").glob("*/*/INSTALL_RECEIPT.json")]
                row["payload_deleted"] = not work.exists()
                result["rows"].append(row)
                result_path.write_text(json.dumps(result, indent=2) + "\n")
                print(json.dumps({k: v for k, v in row.items() if k not in ("downloads", "install_receipts")}), flush=True)
                if not row["ok"]:
                    # Preserve the failure and logs; avoid repeatedly doing an
                    # unsupported install or benchmarking curl delegation.
                    return 1
    result["finished_at"] = datetime.now(timezone.utc).isoformat()
    result["summary"] = []
    for case in args.cases:
        for mode in profiles:
            times = [r["seconds"] for r in result["rows"] if r["case"] == case and r["mode"] == mode and r["ok"]]
            result["summary"].append(dict(case=case, mode=mode, n=len(times), median=statistics.median(times),
                                          min=min(times), max=max(times)))
    result_path.write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result["summary"], indent=2), flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
