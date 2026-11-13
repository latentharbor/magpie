"""CI-only supervisor: keep test progress and dump owned Go stacks if stuck."""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

target = sys.argv[1]
seconds = int(sys.argv[2])
log = Path(os.environ["RUNNER_TEMP"]) / "mac-test-progress.jsonl"
active = {}
fragment = ""
started = time.monotonic()
next_report = started
timed_out = False

with log.open("wb") as output, log.open("r", errors="replace") as progress:
    process = subprocess.Popen(
        ["go", "test", "-json", "-timeout", "30m", "-tags", "nogui", target],
        stdout=output, stderr=subprocess.STDOUT, start_new_session=True,
    )
    while process.poll() is None:
        fragment += progress.read()
        lines = fragment.split("\n")
        fragment = lines.pop()
        for line in lines:
            try:
                event = json.loads(line)
            except (ValueError, TypeError):
                continue
            package, test, action = event.get("Package"), event.get("Test"), event.get("Action")
            if not package:
                continue
            if action == "start":
                active[package] = set()
            elif action == "run" and test:
                active.setdefault(package, set()).add(test)
            elif action in {"pass", "fail", "skip"}:
                if test:
                    active.setdefault(package, set()).discard(test)
                else:
                    active.pop(package, None)
        now = time.monotonic()
        if now >= next_report:
            print(json.dumps({"elapsed_seconds": int(now-started), "active": {p: sorted(t) for p, t in active.items()}}), flush=True)
            next_report = now + 30
        if now-started >= seconds:
            timed_out = True
            print("Diagnostic deadline reached; dumping the owned test process group", flush=True)
            snapshot = subprocess.run(["ps", "-axo", "pid,ppid,pgid,etime,comm"], capture_output=True, text=True).stdout
            print("\n".join(line for line in snapshot.splitlines() if any(word in line for word in (".test", "/node", "/bun", "/go", "PID"))), flush=True)
            try:
                os.killpg(process.pid, signal.SIGQUIT)
            except ProcessLookupError:
                pass
            try:
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
            break
        time.sleep(2)
    result = process.wait()

print(json.dumps({"returncode": result, "diagnostic_timeout": timed_out, "log": str(log)}), flush=True)
if result or timed_out:
    print("\n".join(log.read_text(errors="replace").splitlines()[-120:]), flush=True)
sys.exit(124 if timed_out else (result if result >= 0 else 1))
