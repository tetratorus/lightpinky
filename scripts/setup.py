#!/usr/bin/env python3
"""Build the native macOS executable with this Mac's Xcode."""
import os
from pathlib import Path
import platform
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parent.parent


def main():
    if platform.system() != "Darwin" or platform.machine() != "arm64":
        raise RuntimeError("This build targets an Apple silicon Mac with Xcode installed")
    subprocess.run(
        ["/usr/bin/xcrun", "swift", "build", "-c", "release", "--package-path", str(ROOT)],
        check=True,
    )
    source = ROOT / ".build" / "release" / "lightpinky"
    # Failed builds/copies leave the installed executable usable.
    with tempfile.TemporaryDirectory(prefix=".lightpinky-build-", dir=ROOT) as directory:
        binary = Path(directory) / "lightpinky"
        shutil.copy2(source, binary)
        binary.chmod(0o755)
        subprocess.run([str(binary), "--help"], check=True, stdout=subprocess.DEVNULL)
        os.replace(binary, ROOT / "lightpinky")
    print(f"Ready: {ROOT / 'scripts/phone'} tools", flush=True)


if __name__ == "__main__":
    main()
