#!/usr/bin/env python3
"""Drive lightpinky over stdio MCP and call tools: smoke.py ./lightpinky devices screenshot elements ..."""
import json, subprocess, sys, base64

srv = subprocess.Popen([sys.argv[1]], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
n = 0

def call(method, params=None, notify=False):
    global n
    msg = {"jsonrpc": "2.0", "method": method, "params": params or {}}
    if not notify:
        n += 1
        msg["id"] = n
    srv.stdin.write(json.dumps(msg) + "\n"); srv.stdin.flush()
    if notify:
        return None
    while True:
        r = json.loads(srv.stdout.readline())
        if r.get("id") == n:
            return r

call("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "smoke", "version": "1"}})
call("notifications/initialized", notify=True)
for arg in sys.argv[2:]:
    name, _, js = arg.partition("=")
    r = call("tools/call", {"name": name, "arguments": json.loads(js) if js else {}})
    for c in r.get("result", {}).get("content", []):
        if c["type"] == "image":
            open(f"{name}.jpg", "wb").write(base64.b64decode(c["data"]))
            print(name, "-> wrote", f"{name}.jpg")
        else:
            print(name, "->", c.get("text", "")[:2000])
    if "error" in r:
        print(name, "ERROR", r["error"])
srv.stdin.close(); srv.wait()
