import io
import json
import pathlib
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

SCRIPT = pathlib.Path(__file__).with_name("maintenance-restart.py")
TOKEN = "secret-for-maintenance-test"
FOREIGN = "2050-01-01T00:00:00Z"


class RestartTest(unittest.TestCase):
    def setUp(self):
        temp_dir = tempfile.TemporaryDirectory()
        self.addCleanup(temp_dir.cleanup)
        workdir = temp_dir.name
        self.token_file = pathlib.Path(workdir, "token")
        self.token_file.write_text(TOKEN + "\n")
        self.marker = pathlib.Path(workdir, "ran")
        self.windows = {}
        self.requests = []
        self.fail_after_open = None
        self.failed_once = False
        self.delay_after_open = 0
        self.replace_on_close = None
        state = self

        class Master(BaseHTTPRequestHandler):
            def do_PUT(self):
                target = self.path.rsplit("/", 1)[1]
                payload = json.load(io.BytesIO(self.rfile.read(int(self.headers["Content-Length"]))))
                if self.headers.get("Authorization") != "Bearer " + TOKEN:
                    self.send_error(401)
                    return
                expected = payload["expected_maintenance_until"]
                desired = payload["settings"]["maintenance_until"]
                if not desired and state.marker.exists() and target == state.replace_on_close:
                    state.windows[target] = FOREIGN
                current = state.windows.get(target, "")
                state.requests.append((target, expected, desired, state.marker.exists()))
                if current != expected:
                    self.send_error(409)
                    return
                if desired:
                    state.windows[target] = desired
                    if state.delay_after_open:
                        time.sleep(state.delay_after_open)
                else:
                    state.windows.pop(target, None)
                if target == state.fail_after_open and desired and not state.failed_once:
                    state.failed_once = True
                    self.send_error(500)
                    return
                self.send_response(200)
                self.end_headers()

            def log_message(self, fmt, *args):
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Master)
        self.addCleanup(self.server.server_close)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.shutdown)
        self.url = "http://127.0.0.1:" + str(self.server.server_port)

    def run_restart(self, *targets, command_code=0, seconds=180, master=None):
        args = [sys.executable, str(SCRIPT), "--master", master or self.url,
                "--token-file", str(self.token_file), "--seconds", str(seconds)]
        for target in targets:
            args.extend(["--target", target])
        child = ["--", sys.executable, "-c",
                 "from pathlib import Path; import sys; Path(sys.argv[1]).write_text(sys.argv[2]); sys.exit(int(sys.argv[3]))",
                 str(self.marker), "literal; [argv]", str(command_code)]
        return subprocess.run(args + child, capture_output=True, text=True, timeout=10)

    def test_success_opens_all_then_closes_owned_windows(self):
        result = self.run_restart("ps7", "pr1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.marker.read_text(), "literal; [argv]")
        self.assertFalse(self.windows)
        self.assertEqual(len(self.requests), 4)
        self.assertTrue(all(not r[3] for r in self.requests[:2]))
        self.assertTrue(all(r[3] for r in self.requests[2:]))
        self.assertEqual({r[2] for r in self.requests[:2]}, {r[1] for r in self.requests[2:]})
        self.assertNotIn(TOKEN, result.stdout + result.stderr)

    def test_conflict_preserves_foreign_window_and_rolls_back_owned(self):
        self.windows["pr1"] = FOREIGN
        result = self.run_restart("ps7", "pr1")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.marker.exists())
        self.assertEqual(self.windows, {"pr1": FOREIGN})
        self.assertNotIn(TOKEN, result.stdout + result.stderr)

    def test_lost_confirmation_rolls_back_opening(self):
        self.fail_after_open = "pr1"
        result = self.run_restart("ps7", "pr1")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.marker.exists())
        self.assertFalse(self.windows)

    def test_failed_restart_retains_time_bounded_window(self):
        result = self.run_restart("ps7", command_code=74)
        self.assertEqual(result.returncode, 74, result.stderr)
        self.assertTrue(self.marker.exists())
        self.assertRegex(self.windows["ps7"], r"Z$")
        self.assertEqual(len(self.requests), 1)

    def test_expiry_during_opening_prevents_restart(self):
        self.delay_after_open = 1.1
        result = self.run_restart("ps7", seconds=1)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.marker.exists())
        self.assertFalse(self.windows)

    def test_failed_close_does_not_clear_new_foreign_window(self):
        self.replace_on_close = "ps7"
        result = self.run_restart("ps7")
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertTrue(self.marker.exists())
        self.assertEqual(self.windows, {"ps7": FOREIGN})
        self.assertIn("окно не закрыто", result.stderr)

    def test_rejects_invalid_inputs_before_opening_windows(self):
        for options in ({"seconds": 901}, {"master": "http://example.com"},
                        {"master": "https://example.com?secret=never-log-this"}):
            with self.subTest(options=options):
                result = self.run_restart("ps7", **options)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(self.marker.exists())
                self.assertFalse(self.requests)
                self.assertNotIn("never-log-this", result.stderr)


if __name__ == "__main__":
    unittest.main()
