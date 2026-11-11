#!/usr/bin/env python3
"""Profile a command's process tree. Raw counters are not additive wall time."""

import argparse
import json
import math
import os
from pathlib import Path
import resource
import signal
import subprocess
import sys
import time


def read_process(pid):
    try:
        text = Path(f"/proc/{pid}/stat").read_text()
        end = text.rindex(")")
        fields = text[end + 2 :].split()
        result = {
            "pid": pid,
            "name": text[text.index("(") + 1 : end],
            "ppid": int(fields[1]),
            "start_ticks": int(fields[19]),
            "user_ticks": int(fields[11]),
            "system_ticks": int(fields[12]),
            "minor_faults": int(fields[7]),
            "major_faults": int(fields[9]),
            "rss_pages": int(fields[21]),
        }
        try:
            result["io"] = {
                key: int(value)
                for key, value in (
                    line.split(":", 1)
                    for line in Path(f"/proc/{pid}/io").read_text().splitlines()
                )
            }
        except (PermissionError, FileNotFoundError, ProcessLookupError):
            result["io"] = None
        return result
    except (PermissionError, FileNotFoundError, ProcessLookupError):
        return None


def sample(root, known):
    processes = {}
    for path in Path("/proc").iterdir():
        if path.name.isdigit():
            process = read_process(int(path.name))
            if process is not None:
                processes[process["pid"]] = process
    selected = {
        pid
        for pid, process in processes.items()
        if (root is None or pid == root or known.get(pid) == process["start_ticks"])
    }
    while True:
        children = {p["pid"] for p in processes.values() if p["ppid"] in selected}
        if children <= selected:
            break
        selected |= children
    result = [processes[pid] for pid in sorted(selected)]
    known.update({p["pid"]: p["start_ticks"] for p in result})
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--interval", type=float, default=0.25)
    parser.add_argument("--system", action="store_true",
                        help="sample all visible processes; use inside disposable lab nodes")
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command
    if command and command[0] == "--":
        command = command[1:]
    if not command or not math.isfinite(args.interval) or not 0.01 <= args.interval <= 60:
        parser.error("supply a command and an interval between 0.01 and 60 seconds")
    args.output.mkdir(parents=True, mode=0o700, exist_ok=False)
    started = time.monotonic_ns()
    before = resource.getrusage(resource.RUSAGE_CHILDREN)
    known = {}
    with (args.output / "samples.jsonl").open("x") as samples, (
        args.output / "command.log"
    ).open("xb") as log:
        def emit(event, **fields):
            samples.write(json.dumps({
                "event": event,
                "wall_ns": time.time_ns(),
                "elapsed_ns": time.monotonic_ns() - started,
                **fields,
            }) + "\n")
            samples.flush()

        emit("start", executable=command[0], clock_ticks=os.sysconf("SC_CLK_TCK"),
             page_bytes=os.sysconf("SC_PAGE_SIZE"),
             scope="system" if args.system else "command-tree",
             boot_id=Path("/proc/sys/kernel/random/boot_id").read_text().strip())
        process = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT,
                                   start_new_session=True)

        def forward(signum, _frame):
            try:
                os.killpg(process.pid, signum)
            except ProcessLookupError:
                pass

        signal.signal(signal.SIGTERM, forward)
        signal.signal(signal.SIGINT, forward)
        try:
            while True:
                emit("sample", processes=sample(None if args.system else process.pid, known), pressure={
                    name: Path(f"/proc/pressure/{name}").read_text()
                    for name in ("cpu", "io", "memory")
                })
                try:
                    code = process.wait(timeout=args.interval)
                    break
                except subprocess.TimeoutExpired:
                    pass
        finally:
            if process.poll() is None:
                forward(signal.SIGTERM, None)
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    forward(signal.SIGKILL, None)
                    process.wait()
        after = resource.getrusage(resource.RUSAGE_CHILDREN)
        emit("exit", returncode=code,
             user_seconds=after.ru_utime - before.ru_utime,
             system_seconds=after.ru_stime - before.ru_stime,
             input_blocks=after.ru_inblock - before.ru_inblock,
             output_blocks=after.ru_oublock - before.ru_oublock)
    return 128 - code if code < 0 else code


if __name__ == "__main__":
    sys.exit(main())
