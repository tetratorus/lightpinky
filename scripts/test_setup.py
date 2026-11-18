from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import setup


class SetupTests(unittest.TestCase):
    def test_failed_build_preserves_installed_binary(self):
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(setup, "ROOT", Path(directory)), \
                patch.object(setup.platform, "system", return_value="Darwin"), \
                patch.object(setup.platform, "machine", return_value="arm64"), \
                patch.object(setup.subprocess, "run", side_effect=subprocess.CalledProcessError(1, "swift")):
            installed = Path(directory) / "lightpinky"
            installed.write_bytes(b"previous")
            with self.assertRaises(subprocess.CalledProcessError):
                setup.main()
            self.assertEqual(installed.read_bytes(), b"previous")

    def test_validated_native_binary_replaces_install_atomically(self):
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(setup, "ROOT", Path(directory)), \
                patch.object(setup.platform, "system", return_value="Darwin"), \
                patch.object(setup.platform, "machine", return_value="arm64"), \
                patch.object(setup.subprocess, "run") as run:
            source = Path(directory) / ".build/release/lightpinky"
            source.parent.mkdir(parents=True)
            source.write_bytes(b"native executable")
            setup.main()
            self.assertEqual((Path(directory) / "lightpinky").read_bytes(), source.read_bytes())
            self.assertEqual(run.call_args_list[0].args[0][:3], ["/usr/bin/xcrun", "swift", "build"])
            self.assertEqual(run.call_args_list[1].args[0][-1], "--help")


if __name__ == "__main__":
    unittest.main()
