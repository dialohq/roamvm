import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest


SCRIPT = Path(__file__).with_name("profile-command.py")


class ProfileCommandTest(unittest.TestCase):
    def command(self, output, code, system=False):
        return [sys.executable, str(SCRIPT), "--output", str(output),
                "--interval", "0.02", *(["--system"] if system else []),
                "--", sys.executable, "-c", code]

    def events(self, output):
        return [json.loads(line) for line in
                (output / "samples.jsonl").read_text().splitlines()]

    def test_child_work_and_nonzero_exit_are_preserved(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "profile"
            result = subprocess.run(self.command(output, """
import subprocess, sys
subprocess.run([sys.executable, '-c',
    'import hashlib,time; end=time.monotonic()+0.3; '
    '\\nwhile time.monotonic()<end: hashlib.sha256(b"a"*65536).digest()'])
print('completed workload', flush=True)
sys.exit(7)
"""), timeout=10, capture_output=True)
            self.assertEqual(result.returncode, 7, result.stderr)
            self.assertEqual((output / "command.log").read_text(), "completed workload\n")
            events = self.events(output)
            self.assertEqual(events[-1]["returncode"], 7)
            self.assertGreater(events[-1]["user_seconds"], 0)
            self.assertGreaterEqual(events[-1]["elapsed_ns"], 300_000_000)
            self.assertTrue(any(len(e.get("processes", [])) >= 2 for e in events))
            original = (output / "samples.jsonl").read_bytes()
            again = subprocess.run(self.command(output, "print('overwrite')"),
                                   timeout=10, capture_output=True)
            self.assertNotEqual(again.returncode, 0)
            self.assertEqual((output / "samples.jsonl").read_bytes(), original)

    def test_system_scope_includes_processes_outside_command_tree(self):
        with tempfile.TemporaryDirectory() as temporary:
            for system in (False, True):
                with self.subTest(system=system):
                    output = Path(temporary) / str(system)
                    result = subprocess.run(self.command(output,
                        "import time; time.sleep(0.1)", system=system),
                        timeout=10, capture_output=True)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    events = self.events(output)
                    self.assertEqual(events[0]["scope"],
                                     "system" if system else "command-tree")
                    pids = {p["pid"] for e in events for p in e.get("processes", [])}
                    self.assertEqual(os.getpid() in pids, system)

    def test_term_reaches_workload(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "profile"
            process = subprocess.Popen(self.command(output,
                "import time; print('ready', flush=True); time.sleep(60)"),
                stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
            try:
                deadline = time.monotonic() + 5
                while time.monotonic() < deadline:
                    log = output / "command.log"
                    if log.exists() and "ready" in log.read_text():
                        break
                    time.sleep(0.02)
                else:
                    self.fail("workload did not start")
                process.send_signal(signal.SIGTERM)
                _, errors = process.communicate(timeout=5)
                self.assertEqual(process.returncode, 143, errors)
                self.assertEqual(self.events(output)[-1]["returncode"], -signal.SIGTERM)
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait()
                process.stderr.close()


if __name__ == "__main__":
    unittest.main()
