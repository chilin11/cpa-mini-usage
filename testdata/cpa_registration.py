#!/usr/bin/env python3
"""Load the built plugin with the real CPA 8.0.22 Linux amd64 release."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.request

library = Path(sys.argv[1]).resolve()
assert library.is_file(), library
base = "https://github.com/router-for-me/CLIProxyAPI/releases/download/v8.0.22/"
archive_name = "CLIProxyAPI_8.0.22_linux_amd64.tar.gz"
key = "cpa-registration-test-only"
root = Path(tempfile.mkdtemp(prefix="cpa-registration-", dir=os.getenv("RUNNER_TEMP") or os.getenv("PI_SCRATCH_DIR")))
process = None
try:
    archive = root / archive_name
    with urllib.request.urlopen(base + archive_name, timeout=60) as response:
        archive.write_bytes(response.read())
    with urllib.request.urlopen(base + "checksums.txt", timeout=60) as response:
        checksums = response.read().decode()
    expected = next(line.split()[0] for line in checksums.splitlines() if line.split()[-1].lstrip("*") == archive_name)
    assert hashlib.sha256(archive.read_bytes()).hexdigest() == expected, "CPA archive checksum mismatch"
    with tarfile.open(archive) as bundle:
        bundle.extractall(root, filter="data")
    executables = [p for p in root.rglob("*") if p.is_file() and p.name.lower() in ("cliproxyapi", "cli-proxy-api")]
    assert len(executables) == 1, f"CPA executable not found: {executables}"
    executable = executables[0]
    executable.chmod(0o755)
    (root / "plugins/linux/amd64").mkdir(parents=True)
    shutil.copy2(library, root / "plugins/linux/amd64/cpa-mini-usage.so")
    (root / "auth").mkdir()
    (root / "auth/test-codex.json").write_text(json.dumps({"type": "codex", "email": "test-account@example.invalid"}))
    (root / "config.yaml").write_text(f"""config-version: 8
server:
  host: 127.0.0.1
  port: 18317
management:
  allow-remote: false
  secret-key: {key}
  disable-control-panel: true
access:
  api-keys: [cpa-integration-client-only]
oauth:
  auth-dir: {root}/auth
plugins:
  enabled: true
  dir: {root}/plugins
  configs:
    cpa-mini-usage:
      enabled: true
      priority: 10
""")
    environment = os.environ.copy()
    (root / "data").mkdir()
    timing_event = {"at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "auth_id": "timing-only", "provider": "codex", "model": "test", "input": 900, "output": 100, "total": 1000, "ttft_ms": 500, "latency_ms": 2500}
    (root / "data/events.jsonl").write_text(json.dumps(timing_event) + "\n")
    environment["CPA_MINI_USAGE_DATA_DIR"] = str(root / "data")
    # Do not expose the runner's GitHub token to the test host.
    environment.pop("GITHUB_TOKEN", None)
    environment.pop("GH_TOKEN", None)
    with (root / "host.log").open("w") as log:
        process = subprocess.Popen([str(executable), "-config", str(root / "config.yaml"), "-local-model"], cwd=root, env=environment, stdout=log, stderr=subprocess.STDOUT)
        def request(path, authenticated=True):
            headers = {"Authorization": "Bearer " + key} if authenticated else {}
            req = urllib.request.Request("http://127.0.0.1:18317" + path, headers=headers)
            with urllib.request.urlopen(req, timeout=5) as response:
                return response.read()
        for attempt in range(60):
            if process.poll() is not None:
                raise RuntimeError(f"CPA exited: {process.returncode}")
            try:
                listing = json.loads(request("/v0/management/plugins"))
                break
            except (urllib.error.URLError, TimeoutError):
                time.sleep(1)
        else:
            raise RuntimeError("CPA management API did not become ready")
        plugin = next(p for p in listing["plugins"] if p["id"] == "cpa-mini-usage")
        assert plugin["registered"] and plugin["effective_enabled"], plugin
        assert plugin["metadata"]["github_repository"] == "https://github.com/chilin11/cpa-mini-usage", plugin
        assert plugin["menus"], plugin
        print("PASS CPA 8.0.22: plugin registered, enabled, metadata and menu present")
        html = request("/v0/resource/plugins/cpa-mini-usage/dashboard", False)
        assert b"<html" in html.lower(), html[:200]
        print("PASS dashboard resource: HTTP 200 HTML")
        json.loads(request("/v0/management/plugins/cpa-mini-usage/data"))
        print("PASS authenticated data endpoint: HTTP 200 JSON")
        try:
            request("/v0/management/plugins/cpa-mini-usage/data", False)
        except urllib.error.HTTPError as error:
            assert error.code in (401, 403), error.code
        else:
            raise AssertionError("Data endpoint allowed unauthenticated access")
        print("PASS unauthenticated data endpoint: access denied")
        refresh_url = "http://127.0.0.1:18317/v0/management/plugins/cpa-mini-usage/refresh"
        for _ in range(40):
            req = urllib.request.Request(refresh_url, data=b"", headers={"Authorization": "Bearer " + key}, method="POST")
            try:
                with urllib.request.urlopen(req, timeout=5) as response:
                    assert response.status == 202
                    accepted = json.loads(response.read())
                break
            except urllib.error.HTTPError as error:
                assert error.code == 409, error.code
                time.sleep(0.25)
        else:
            raise AssertionError("Full refresh stayed busy")
        for _ in range(40):
            state = json.loads(request("/v0/management/plugins/cpa-mini-usage/data"))
            if not state.get("quota_refreshing") and state.get("quota_refresh_finished_at") and state["quota_refresh_finished_at"] != accepted.get("quota_refresh_finished_at"):
                break
            time.sleep(0.25)
        else:
            raise AssertionError("Manual refresh did not finish in test host")
        print("PASS manual refresh: HTTP 202 and observable completion")
        account_url = refresh_url + "-account"
        for authenticated, payload, expected in [(False, {"auth_id": "test"}, (401, 403)), (True, {}, (400,)), (True, {"auth_id": "unknown"}, (404,))]:
            headers = {"Content-Type": "application/json"}
            if authenticated:
                headers["Authorization"] = "Bearer " + key
            req = urllib.request.Request(account_url, data=json.dumps(payload).encode(), headers=headers, method="POST")
            try:
                urllib.request.urlopen(req, timeout=5)
            except urllib.error.HTTPError as error:
                assert error.code in expected, (error.code, expected)
            else:
                raise AssertionError("Account refresh incorrectly accepted invalid/unauthenticated request")
        print("PASS account route: authentication and JSON-body validation")
        account = next(q for q in state["quotas"] if q["provider"] == "codex")
        auth_id = account["auth_id"]
        full_finished = state["quota_refresh_finished_at"]
        req = urllib.request.Request(account_url, data=json.dumps({"auth_id": auth_id}).encode(), headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"}, method="POST")
        with urllib.request.urlopen(req, timeout=5) as response:
            assert response.status == 202
            accepted = json.loads(response.read())
        for _ in range(40):
            state = json.loads(request("/v0/management/plugins/cpa-mini-usage/data"))
            refresh = state.get("account_refresh", {}).get(auth_id, {})
            if not refresh.get("refreshing") and refresh.get("finished_at") and refresh["finished_at"] != accepted.get("refresh_finished_at"):
                break
            time.sleep(0.25)
        else:
            raise AssertionError("Single-account refresh did not finish")
        assert refresh.get("error"), "Tokenless test account must report failure without making provider calls"
        assert state["quota_refresh_finished_at"] == full_finished, "Single-account refresh changed full refresh state"
        print("PASS single-account refresh: HTTP 202, observable failure completion, full scan untouched")
        assert state["persistence"]["enabled"] is True
        timing = next(event for event in state["recent"] if event["auth_id"] == "timing-only")
        assert timing["ttft_ms"] == 500 and timing["latency_ms"] == 2500
        assert timing["output"] / ((timing["latency_ms"] - timing["ttft_ms"]) / 1000) == 50
        settings_url = refresh_url.rsplit("/", 1)[0] + "/settings"
        for authenticated, payload, expected in [(False, {"persistence_enabled": False}, (401, 403)), (True, {}, (400,))]:
            headers = {"Content-Type": "application/json"}
            if authenticated:
                headers["Authorization"] = "Bearer " + key
            req = urllib.request.Request(settings_url, data=json.dumps(payload).encode(), headers=headers, method="POST")
            try:
                urllib.request.urlopen(req, timeout=5)
            except urllib.error.HTTPError as error:
                assert error.code in expected, (error.code, expected)
            else:
                raise AssertionError("Settings incorrectly accepted invalid/unauthenticated request")
        for enabled in (True, False):
            req = urllib.request.Request(settings_url, data=json.dumps({"persistence_enabled": enabled}).encode(), headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"}, method="POST")
            with urllib.request.urlopen(req, timeout=5) as response:
                settings = json.loads(response.read())["persistence"]
                assert response.status == 200 and settings["enabled"] is enabled
        saved_events = (root / "data/events.jsonl").read_bytes()
        process.terminate()
        process.wait(timeout=10)
        process = subprocess.Popen([str(executable), "-config", str(root / "config.yaml"), "-local-model"], cwd=root, env=environment, stdout=log, stderr=subprocess.STDOUT)
        for _ in range(60):
            try:
                state = json.loads(request("/v0/management/plugins/cpa-mini-usage/data"))
                break
            except (urllib.error.URLError, TimeoutError):
                time.sleep(1)
        else:
            raise AssertionError("Restarted CPA did not become ready")
        assert state["persistence"]["enabled"] is False
        assert any(q["auth_id"] == auth_id for q in state["quotas"])
        timing = next(event for event in state["recent"] if event["auth_id"] == "timing-only")
        assert timing["ttft_ms"] == 500 and timing["latency_ms"] == 2500
        assert (root / "data/events.jsonl").read_bytes() == saved_events
        print("PASS persistent storage: authenticated settings, quota/timing restore after host restart, disabled writes preserve history")
except Exception:
    if (root / "host.log").exists():
        print((root / "host.log").read_text(), file=sys.stderr)
    raise
finally:
    if process is not None and process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
    shutil.rmtree(root)
