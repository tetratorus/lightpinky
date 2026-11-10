#!/usr/bin/env python3
"""Call lightpinky's MCP tools: smoke.py ./lightpinky devices screenshot elements ...

By default the calls go to a lightpinky that this script keeps running in the
background on a private Unix socket, so phones stay connected between
invocations (a Simulator's WebDriverAgent takes seconds to start). It exits
after LIGHTPINKY_IDLE (default 15m) without calls; `stop` ends it at once.
LIGHTPINKY_DAEMON=0 runs a fresh stdio server for this invocation instead.
LIGHTPINKY_DEVICE is passed as `device` to every tool that takes one.
"""
import base64
import contextlib
import fcntl
import hashlib
import http.client
import json
import os
from pathlib import Path
import queue
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time

LOAD_UNLOCK_CODE = 'set +x; if [ -f "$HOME/.zshrc" ]; then source "$HOME/.zshrc" >/dev/null 2>&1; fi; unsetopt xtrace; export IPHONE_UNLOCK_CODE; exec "$@"'


def server_command(binary, operations):
    if any(op.partition("=")[0] == "unlock" for op in operations) and not os.environ.get("IPHONE_UNLOCK_CODE"):
        return ["/bin/zsh", "-c", LOAD_UNLOCK_CODE, "lightpinky", binary]
    return [binary]


def daemon_command(binary, sock, idle):
    command = [binary, "-socket", str(sock), "-idle", idle]
    if not os.environ.get("IPHONE_UNLOCK_CODE") and os.path.exists("/bin/zsh"):
        return ["/bin/zsh", "-c", LOAD_UNLOCK_CODE, "lightpinky"] + command
    return command


class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, path, timeout):
        super().__init__("localhost", timeout=timeout)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


class SocketClient:
    """MCP over the background server's socket (stateless, JSON responses)."""

    def __init__(self, sock, timeout=1000):
        self.sock = str(sock)
        self.timeout = timeout
        self.n = 0

    def call(self, method, params=None, notify=False):
        if notify:
            return None
        self.n += 1
        body = json.dumps({"jsonrpc": "2.0", "id": self.n, "method": method, "params": params or {}})
        conn = UnixHTTPConnection(self.sock, self.timeout)
        try:
            conn.request("POST", "/mcp", body, {"Content-Type": "application/json", "Accept": "application/json, text/event-stream"})
            response = conn.getresponse()
            data = response.read()
        except socket.timeout:
            raise RuntimeError(f"{method} timed out; the operation was not retried") from None
        finally:
            conn.close()
        if response.status != 200:
            raise RuntimeError(f"{method}: HTTP {response.status}: {data.decode(errors='replace').strip()}")
        try:
            return json.loads(data)
        except ValueError:
            raise RuntimeError("Server emitted invalid MCP JSON") from None

    def close(self):
        pass


def state_dir():
    cache = Path(os.environ.get("XDG_CACHE_HOME", str(Path.home() / ".cache"))) / "lightpinky" / "run"
    cache.mkdir(parents=True, exist_ok=True, mode=0o700)
    return cache


def daemon_paths(binary):
    key = hashlib.sha256(str(binary).encode()).hexdigest()[:12]
    base = state_dir() / key
    return base.with_suffix(".sock"), base.with_suffix(".json"), base.with_suffix(".log"), base.with_suffix(".lock")


def stamp(binary):
    st = os.stat(binary)
    return [st.st_mtime_ns, st.st_size]


def answering(sock):
    with contextlib.closing(socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)) as s:
        try:
            s.connect(str(sock))
            return True
        except OSError:
            return False


def stop_daemon(meta, sock):
    try:
        pid = json.loads(meta.read_text())["pid"]
        os.kill(pid, signal.SIGTERM)
    except (OSError, ValueError, KeyError):
        return
    for _ in range(100):
        if not answering(sock):
            break
        time.sleep(0.1)


def ensure_daemon(binary):
    """Return the socket of a background server running this binary, starting it if needed."""
    sock, meta, log, lock = daemon_paths(binary)
    with open(lock, "w") as held:
        fcntl.flock(held, fcntl.LOCK_EX)
        if answering(sock):
            try:
                current = json.loads(meta.read_text()).get("stamp")
            except (OSError, ValueError):
                current = None
            if current == stamp(binary):
                return sock
            stop_daemon(meta, sock)
        with open(log, "a") as out:
            server = subprocess.Popen(daemon_command(binary, sock, os.environ.get("LIGHTPINKY_IDLE", "15m")),
                                      stdin=subprocess.DEVNULL, stdout=out, stderr=out, start_new_session=True)
        meta.write_text(json.dumps({"pid": server.pid, "stamp": stamp(binary)}))
        for _ in range(200):
            if answering(sock):
                return sock
            if server.poll() is not None:
                break
            time.sleep(0.05)
        raise RuntimeError(f"lightpinky did not start; see {log}")


def with_device(client, operations):
    """Tool names that take a device, when LIGHTPINKY_DEVICE asks for one."""
    if not os.environ.get("LIGHTPINKY_DEVICE"):
        return set()
    tools = client.call("tools/list").get("result", {}).get("tools", [])
    return {t["name"] for t in tools if "device" in t.get("inputSchema", {}).get("properties", {})}


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


def connect(binary, operations):
    if os.environ.get("LIGHTPINKY_DAEMON", "1") == "0":
        client = Client(server_command(binary, operations))
        result = client.call("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "lightpinky-cli", "version": "1"}})
        if "error" in result:
            client.close()
            raise RuntimeError(json.dumps(result["error"]))
        client.call("notifications/initialized", notify=True)
        return client
    return SocketClient(ensure_daemon(binary))


def run(binary, operations):
    binary = str(Path(binary).resolve())
    if operations == ["stop"]:
        sock, meta, _, _ = daemon_paths(binary)
        stop_daemon(meta, sock)
        print("stopped", flush=True)
        return 0
    client = connect(binary, operations)
    try:
        takes_device = with_device(client, operations)
        for op in operations:
            name, _, args = op.partition("=")
            if name == "tools":
                response = client.call("tools/list")
                print(json.dumps(response, indent=2), flush=True)
            else:
                arguments = json.loads(args) if args else {}
                if name in takes_device:
                    arguments.setdefault("device", os.environ["LIGHTPINKY_DEVICE"])
                response = client.call("tools/call", {"name": name, "arguments": arguments})
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
        sys.exit("Usage: smoke.py BINARY [tools | stop | TOOL | TOOL=JSON] ...")
    try:
        sys.exit(run(sys.argv[1], sys.argv[2:] or ["tools"]))
    except (RuntimeError, OSError, ValueError) as error:
        sys.exit(str(error))
