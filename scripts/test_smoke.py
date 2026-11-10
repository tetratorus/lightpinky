import contextlib
import io
import os
import sys
import unittest
from unittest.mock import Mock, patch

import smoke


class SmokeTests(unittest.TestCase):
    def test_secret_is_never_in_arguments(self):
        with patch.dict(os.environ, {"IPHONE_UNLOCK_CODE": "test-secret"}):
            self.assertEqual(smoke.server_command("/binary", ["unlock"]), ["/binary"])

    def test_unlock_loads_shell_environment_only_when_needed(self):
        with patch.dict(os.environ, {}, clear=True):
            command = smoke.server_command("/binary", ["unlock", "screenshot"])
            self.assertEqual(command[0], "/bin/zsh")
            self.assertIn('export IPHONE_UNLOCK_CODE; exec "$@"', command[2])
            self.assertEqual(command[-1], "/binary")
            self.assertEqual(smoke.server_command("/binary", ["devices"]), ["/binary"])

    def test_tool_error_stops_remaining_calls(self):
        client = Mock()
        client.call.side_effect = [
            {"result": {}}, None,
            {"result": {"isError": True, "content": [{"type": "text", "text": "failed"}]}},
        ]
        with patch.object(smoke, "Client", return_value=client), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(smoke.run("/binary", ["unlock", "launch"]), 1)
        self.assertEqual(client.call.call_count, 3)
        client.close.assert_called_once()

    def test_text_is_not_truncated(self):
        text = "x" * 8000
        client = Mock()
        client.call.side_effect = [
            {"result": {}}, None,
            {"result": {"content": [{"type": "text", "text": text}]}},
        ]
        output = io.StringIO()
        with patch.object(smoke, "Client", return_value=client), contextlib.redirect_stdout(output):
            self.assertEqual(smoke.run("/binary", ["elements"]), 0)
        self.assertIn(text, output.getvalue())

    def test_invalid_server_output_fails(self):
        client = smoke.Client([sys.executable, "-u", "-c", "import sys; sys.stdin.readline(); print('invalid')"])
        try:
            with self.assertRaisesRegex(RuntimeError, "invalid MCP JSON"):
                client.call("initialize")
        finally:
            client.close()

    def test_timeout_is_not_retried(self):
        client = smoke.Client([sys.executable, "-u", "-c", "import time; time.sleep(0.2)"], timeout=0.02)
        try:
            with self.assertRaisesRegex(RuntimeError, "not retried"):
                client.call("initialize")
            self.assertEqual(client.n, 1)
        finally:
            client.close()


if __name__ == "__main__":
    unittest.main()
