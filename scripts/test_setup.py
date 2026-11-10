import hashlib
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

import setup


class SetupTests(unittest.TestCase):
    def test_version_comparison(self):
        self.assertGreater(setup.version("go1.27.3"), setup.version("1.26.5"))
        self.assertEqual(setup.version("go1.27"), (1, 27, 0))
        self.assertEqual(setup.version("garbage"), (0, 0, 0))

    def test_existing_go_does_not_download(self):
        with patch.object(setup.shutil, "which", return_value="/go"), patch.object(setup.subprocess, "run", return_value=Mock(returncode=0, stdout="go1.27.3")), patch.object(setup.urllib.request, "urlopen") as download:
            self.assertEqual(setup.toolchain("1.27.3", {}), "/go")
            download.assert_not_called()

    def test_cached_go_does_not_download(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {"XDG_CACHE_HOME": directory}), patch.object(setup.shutil, "which", return_value=None), patch.object(setup.platform, "system", return_value="Darwin"), patch.object(setup.platform, "machine", return_value="arm64"), patch.object(setup.urllib.request, "urlopen") as download:
            binary = Path(directory) / "lightpinky/go1.27.3-darwin-arm64/bin/go"
            binary.parent.mkdir(parents=True)
            binary.touch()
            self.assertEqual(setup.toolchain("1.27.3", {}), str(binary))
            download.assert_not_called()

    def test_bad_checksum_is_not_extracted(self):
        metadata = [{"version": "go1.27.3", "files": [{
            "os": "darwin", "arch": "arm64", "kind": "archive",
            "filename": "go1.27.3.darwin-arm64.tar.gz",
            "sha256": hashlib.sha256(b"expected").hexdigest(),
        }]}]
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {"XDG_CACHE_HOME": directory}), patch.object(setup.shutil, "which", return_value=None), patch.object(setup.platform, "system", return_value="Darwin"), patch.object(setup.platform, "machine", return_value="arm64"), patch.object(setup.urllib.request, "urlopen", side_effect=[io.BytesIO(json.dumps(metadata).encode()), io.BytesIO(b"wrong")]), patch.object(setup.subprocess, "run") as extract:
            with self.assertRaisesRegex(RuntimeError, "checksum mismatch"):
                setup.toolchain("1.27.3", {})
            extract.assert_not_called()


if __name__ == "__main__":
    unittest.main()
