#!/usr/bin/env python3
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import tempfile
import urllib.request

ROOT = Path(__file__).resolve().parent.parent


def version(value):
    match = re.match(r"(?:go)?(\d+)\.(\d+)(?:\.(\d+))?", value)
    return tuple(int(part or 0) for part in match.groups()) if match else (0, 0, 0)


def toolchain(required, env):
    go = shutil.which("go")
    if go:
        result = subprocess.run([go, "env", "GOVERSION"], env=env, capture_output=True, text=True)
        if result.returncode == 0 and version(result.stdout.strip()) >= version(required):
            return go
    system = {"Darwin": "darwin", "Linux": "linux"}.get(platform.system())
    arch = {"arm64": "arm64", "aarch64": "arm64", "x86_64": "amd64", "AMD64": "amd64"}.get(platform.machine())
    if not system or not arch:
        raise RuntimeError(f"Install Go {required} or newer for {platform.system()}/{platform.machine()}")
    cache = Path(os.environ.get("XDG_CACHE_HOME", str(Path.home() / ".cache"))) / "lightpinky"
    target = cache / f"go{required}-{system}-{arch}"
    binary = target / "bin" / "go"
    if binary.exists():
        return str(binary)
    cache.mkdir(parents=True, exist_ok=True)
    print(f"Installing Go {required} into {target} (no system changes)", flush=True)
    with urllib.request.urlopen("https://go.dev/dl/?mode=json&include=all", timeout=60) as response:
        releases = json.load(response)
    release = next(item for item in releases if item["version"] == "go" + required)
    archive = next(item for item in release["files"] if item["os"] == system and item["arch"] == arch and item["kind"] == "archive")
    filename = f"go{required}.{system}-{arch}.tar.gz"
    if archive["filename"] != filename:
        raise RuntimeError("Unexpected Go archive filename")
    with tempfile.TemporaryDirectory(prefix="go-download-", dir=cache) as directory:
        package = Path(directory) / filename
        digest = hashlib.sha256()
        with urllib.request.urlopen("https://go.dev/dl/" + filename, timeout=120) as response, package.open("wb") as output:
            while chunk := response.read(1024 * 1024):
                output.write(chunk)
                digest.update(chunk)
        if digest.hexdigest() != archive["sha256"]:
            raise RuntimeError("Go archive checksum mismatch")
        subprocess.run(["tar", "-xzf", str(package), "-C", directory], check=True)
        os.rename(Path(directory) / "go", target)
    return str(binary)


def main():
    required = re.search(r"^go (\S+)$", (ROOT / "go.mod").read_text(), re.MULTILINE).group(1)
    env = dict(os.environ, GOTOOLCHAIN="local", GOPROXY="off", GOSUMDB="off", CGO_ENABLED="0")
    env.pop("GOROOT", None)
    env.pop("GOOS", None)
    env.pop("GOARCH", None)
    go = toolchain(required, env)
    print("Building lightpinky using vendored dependencies", flush=True)
    with tempfile.TemporaryDirectory(prefix=".lightpinky-build-", dir=ROOT) as directory:
        binary = Path(directory) / "lightpinky"
        subprocess.run([go, "build", "-mod=vendor", "-trimpath", "-o", str(binary), "./cmd/lightpinky"], cwd=ROOT, env=env, check=True)
        os.replace(binary, ROOT / "lightpinky")
    print(f"Ready: {ROOT / 'scripts/phone'} tools", flush=True)


if __name__ == "__main__":
    main()
