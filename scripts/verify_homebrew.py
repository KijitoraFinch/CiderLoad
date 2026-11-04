#!/usr/bin/env python3
"""Run real brew installs in isolated prefixes against a repeatable local CDN.

The existing Homebrew checkout and installed packages are read only. Each case
gets a separate checkout, cache, Cellar, logs, and CiderLoad state. Download rate
is bounded per connection so this measures the intended scheduling behavior,
not internet variability. Setup time is excluded; all of brew install is timed.
"""

import argparse
import hashlib
import http.server
import io
import json
import os
from pathlib import Path
import random
import shutil
import subprocess
import tarfile
import threading
import time


class CDN(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def do_HEAD(self):
        self.serve(False)

    def do_GET(self):
        self.serve(True)

    def serve(self, body):
        name = self.path.removeprefix("/")
        data = self.server.artifacts.get(name)
        if data is None:
            self.send_error(404)
            return
        start, end, status = 0, len(data) - 1, 200
        value = self.headers.get("Range")
        if value:
            bounds = value.removeprefix("bytes=").split("-")
            start = int(bounds[0])
            if bounds[1]:
                end = min(int(bounds[1]), end)
            if start > end:
                self.send_response(416)
                self.send_header("Content-Range", f"bytes */{len(data)}")
                self.send_header("Content-Length", "0")
                self.end_headers()
                return
            status = 206
        size = end - start + 1
        self.send_response(status)
        self.send_header("Content-Length", str(size))
        self.send_header("Content-Type", "application/gzip")
        self.send_header("Content-Disposition", f'attachment; filename="{name}"')
        self.send_header("Accept-Ranges", "bytes")
        self.send_header("ETag", f'"{self.server.digests[name]}"')
        self.send_header("Last-Modified", "Thu, 02 Jan 2025 03:04:05 GMT")
        if status == 206:
            self.send_header("Content-Range", f"bytes {start}-{end}/{len(data)}")
        self.end_headers()
        if not body:
            return
        transfer = size > 1
        with self.server.metrics_lock:
            m = self.server.metrics
            m["requests"] += 1
            m["range_requests"] += bool(value)
            if transfer:
                m["active"] += 1
                m["peak"] = max(m["peak"], m["active"])
        try:
            for offset in range(start, end + 1, 32 * 1024):
                block = data[offset : min(end + 1, offset + 32 * 1024)]
                self.wfile.write(block)
                self.wfile.flush()
                if transfer:
                    time.sleep(len(block) / (4 * 1024 * 1024))
        except (BrokenPipeError, ConnectionResetError):
            pass
        finally:
            if transfer:
                with self.server.metrics_lock:
                    self.server.metrics["active"] -= 1


def run(args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--brew-repository", type=Path, default=Path("/opt/homebrew"))
    p.add_argument("--binary", type=Path, default=Path("build/ciderload"))
    p.add_argument("--cidl", type=Path, default=Path("build/cidl"))
    p.add_argument("--targets", nargs="+", choices=("single", "main"), default=["single", "main"])
    p.add_argument("--output-dir", type=Path, default=Path("build/homebrew-verification"))
    args = p.parse_args()
    repo, binary, root = (x.resolve() for x in (args.brew_repository, args.binary, args.output_dir))
    if root.exists():
        p.error("output directory already exists; choose a fresh directory")
    cidl = args.cidl.resolve()
    if not binary.is_file() or not cidl.is_file() or not (repo / "bin/brew").is_file():
        p.error("build CiderLoad and supply an existing Homebrew checkout")
    root.mkdir(parents=True)
    artifacts, checksums, payload_hashes = {}, {}, {}
    names = {"single": 64, "main": 48, "dep-a": 48, "dep-b": 48}
    for i, (name, mib) in enumerate(names.items()):
        payload = random.Random(101 + i).randbytes(mib * 1024 * 1024)
        payload_hashes[name] = hashlib.sha256(payload).hexdigest()
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w:gz", compresslevel=1) as archive:
            info = tarfile.TarInfo(f"ciderload-{name}-1.0/payload")
            info.size = len(payload)
            archive.addfile(info, io.BytesIO(payload))
        filename = f"ciderload-{name}-1.0.tar.gz"
        artifacts[filename] = buf.getvalue()
        checksums[filename] = hashlib.sha256(artifacts[filename]).hexdigest()

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), CDN)
    server.daemon_threads = True
    server.artifacts, server.digests = artifacts, checksums
    server.metrics_lock = threading.Lock()
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    rows = []
    try:
        for target in args.targets:
            for mode in ("curl", "ciderload"):
                case = root / f"{target}-{mode}"
                prefix = case / "prefix"
                case.mkdir()
                run(["git", "clone", "--quiet", "--shared", str(repo), str(prefix)])
                shutil.rmtree(prefix / "Library/Homebrew/vendor")
                shutil.copytree(repo / "Library/Homebrew/vendor", prefix / "Library/Homebrew/vendor",
                                symlinks=True)
                tap = prefix / "Library/Taps/ciderload/homebrew-fixtures"
                formulas = tap / "Formula"
                formulas.mkdir(parents=True)
                run(["git", "init", "--quiet", str(tap)])
                for name in names:
                    filename = f"ciderload-{name}-1.0.tar.gz"
                    klass = "Ciderload" + "".join(part.capitalize() for part in name.split("-"))
                    deps = ""
                    if name == "main":
                        deps = '\n  depends_on "ciderload/fixtures/ciderload-dep-a"\n  depends_on "ciderload/fixtures/ciderload-dep-b"'
                    (formulas / f"ciderload-{name}.rb").write_text(f'''class {klass} < Formula
  desc "CiderLoad local verification fixture"
  homepage "https://example.invalid/ciderload"
  url "http://127.0.0.1:{server.server_port}/{filename}"
  sha256 "{checksums[filename]}"
  license "MIT"{deps}
  def install
    libexec.install "payload"
  end
end
''')
                env = os.environ.copy()
                for name in list(env):
                    if name.startswith(("HOMEBREW_", "CIDERLOAD_", "CIDL_")) or name == "_CIDERLOAD_DELEGATING":
                        del env[name]
                env.update({
                    "HOMEBREW_NO_AUTO_UPDATE": "1", "HOMEBREW_NO_ANALYTICS": "1",
                    "HOMEBREW_NO_INSTALL_FROM_API": "1", "HOMEBREW_NO_INSTALL_CLEANUP": "1",
                    "HOMEBREW_NO_ENV_HINTS": "1", "HOMEBREW_DEVELOPER": "1",
                    "HOMEBREW_CACHE": str(case / "cache"), "HOMEBREW_LOGS": str(case / "logs"),
                    "HOMEBREW_TEMP": str(case / "temp"),
                    "XDG_CONFIG_HOME": str(case / "config"),
                    "HOMEBREW_CURL_PATH": str(binary) if mode == "ciderload" else "/usr/bin/curl",
                    "CIDL_BREW": str(prefix / "bin/brew"), "CIDL_DOWNLOADER": str(binary),
                    "HOMEBREW_CIDERLOAD_STATE_DIR": str(case / "state"),
                    "HOMEBREW_CIDERLOAD_TRACE": "1",
                    "HOMEBREW_CIDERLOAD_CONNECTIONS": "4",
                    "HOMEBREW_CIDERLOAD_TOTAL_CONNECTIONS": "6",
                })
                (case / "temp").mkdir()
                run([str(prefix / "bin/brew"), "trust", "ciderload/fixtures"], env=env,
                    stdout=subprocess.DEVNULL)
                with server.metrics_lock:
                    server.metrics = dict(active=0, peak=0, requests=0, range_requests=0)
                print(f"Installing {target} with {mode}...", flush=True)
                with (case / "install.log").open("w") as log:
                    start = time.monotonic()
                    executable = cidl if mode == "ciderload" else prefix / "bin/brew"
                    run([str(executable), "install", "--build-from-source",
                         f"ciderload/fixtures/ciderload-{target}"], env=env, stdout=log, stderr=subprocess.STDOUT)
                    elapsed = time.monotonic() - start
                installed = [target] if target == "single" else ["main", "dep-a", "dep-b"]
                for name in installed:
                    path = prefix / f"Cellar/ciderload-{name}/1.0/libexec/payload"
                    if hashlib.sha256(path.read_bytes()).hexdigest() != payload_hashes[name]:
                        raise RuntimeError(f"installed content differs: {name}")
                with server.metrics_lock:
                    metrics = server.metrics.copy()
                if mode == "ciderload" and (metrics["peak"] > 6 or metrics["range_requests"] < 4):
                    raise RuntimeError(f"parallel path or connection limit was not exercised: {metrics}")
                row = dict(target=target, mode=mode, seconds=round(elapsed, 3), **metrics)
                rows.append(row)
                print(json.dumps(row), flush=True)
                (root / "results.json").write_text(json.dumps(rows, indent=2) + "\n")
    finally:
        server.shutdown()
        server.server_close()


if __name__ == "__main__":
    main()
