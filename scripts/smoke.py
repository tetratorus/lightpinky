#!/usr/bin/env python3
"""Drive lightpinky over stdio MCP and call tools: smoke.py ./lightpinky devices screenshot elements ..."""
import base64
import json
import os
from pathlib import Path
import queue
import subprocess
import sys
import tempfile
import threading


def server_command(binary, operations):
    if any(op.partition("=")[0] == "unlock" for op in operations) and not os.environ.get("IPHONE_UNLOCK_CODE"):
        return ["/bin/zsh", "-c",
                'set +x; if [ -f "$HOME/.zshrc" ]; then source "$HOME/.zshrc" >/dev/null 2>&1; fi; unsetopt xtrace; export IPHONE_UNLOCK_CODE; exec "$@"',
                "lightpinky", binary]
    return [binary]


class Client:
    def __init__(self, command, timeout=120):
        self.server = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        self.responses = queue.Queue()
        self.n = 0
        self.timeout = timeout
        threading.Thread(target=self.read, daemon=True).start()

    def read(self):
        for line in self.server.stdout:
            try:
                self.responses.put(json.loads(line))
            except ValueError:
                self.responses.put(RuntimeError("Server emitted invalid MCP JSON"))
                return
        self.responses.put(RuntimeError("Server exited before responding"))

    def call(self, method, params=None, notify=False):
        msg = {"jsonrpc": "2.0", "method": method, "params": params or {}}
        if not notify:
            self.n += 1
            msg["id"] = self.n
        self.server.stdin.write(json.dumps(msg) + "\n")
        self.server.stdin.flush()
        if notify:
            return None
        while True:
            try:
                response = self.responses.get(timeout=self.timeout)
            except queue.Empty:
                raise RuntimeError(f"{method} timed out; the operation was not retried") from None
            if isinstance(response, Exception):
                raise response
            if response.get("id") == self.n:
                return response

    def close(self):
        self.server.stdin.close()
        try:
            self.server.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.server.terminate()
            try:
                self.server.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.server.kill()
                self.server.wait()
        self.server.stdout.close()


def run(binary, operations):
    client = Client(server_command(str(Path(binary).resolve()), operations))
    try:
        result = client.call("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "lightpinky-cli", "version": "1"}})
        if "error" in result:
            raise RuntimeError(json.dumps(result["error"]))
        client.call("notifications/initialized", notify=True)
        for op in operations:
            name, _, args = op.partition("=")
            if name == "tools":
                response = client.call("tools/list")
                print(json.dumps(response, indent=2), flush=True)
            else:
                response = client.call("tools/call", {"name": name, "arguments": json.loads(args) if args else {}})
                for content in response.get("result", {}).get("content", []):
                    if content["type"] == "image":
                        with tempfile.NamedTemporaryFile(prefix="lightpinky-", suffix=".jpg", delete=False) as image:
                            image.write(base64.b64decode(content["data"]))
                        print(name, "->", image.name, flush=True)
                    elif content["type"] == "text":
                        print(name, "->", content["text"], flush=True)
            if "error" in response:
                print(json.dumps(response["error"]), file=sys.stderr)
                return 1
            if response.get("result", {}).get("isError"):
                return 1
        return 0
    finally:
        client.close()


if __name__ == "__main__":
    if len(sys.argv) < 2:
        sys.exit("Usage: smoke.py BINARY [tools | TOOL | TOOL=JSON] ...")
    try:
        sys.exit(run(sys.argv[1], sys.argv[2:] or ["tools"]))
    except (RuntimeError, OSError, ValueError) as error:
        sys.exit(str(error))
