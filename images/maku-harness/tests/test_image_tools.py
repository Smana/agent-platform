"""The image carries what agent-checks needs (v3 feature 3, R1 and R4). Inside the image only."""
import os
import shutil
import subprocess
import sys
import unittest

import yaml

IN_IMAGE = os.path.exists("/agent-server/.venv")


@unittest.skipUnless(IN_IMAGE, "outside the harness image")
class ImageToolsTest(unittest.TestCase):
    def test_mise_runs_and_stays_off_path(self):
        self.assertEqual(subprocess.run(["/opt/agent/bin/mise", "--version"], capture_output=True).returncode, 0)
        self.assertIsNone(shutil.which("mise"))

    def test_the_system_python_has_the_venvs_pyyaml(self):
        out = subprocess.run(["/usr/local/bin/python3", "-c", "import yaml; print(yaml.__version__)"],
                             capture_output=True, text=True, check=True).stdout.strip()
        self.assertEqual(out, yaml.__version__)

    def test_rsync_and_agent_checks_are_on_path(self):
        self.assertIsNotNone(shutil.which("rsync"))
        self.assertEqual(os.path.realpath(shutil.which("agent-checks")), "/opt/agent/agent_checks.py")


class ChecksRequirementsTest(unittest.TestCase):
    """The system python's PyYAML pin is the lock's own: same version, same hashes."""

    @staticmethod
    def pyyaml_block(name):
        here = os.path.dirname(os.path.abspath(__file__))
        with open(os.path.join(here, "..", name)) as f:
            text = f.read().replace("\\\n", " ")
        (line,) = [ln.split() for ln in text.splitlines() if ln.startswith("pyyaml==")]
        return line

    def test_the_checks_requirements_follow_the_lock(self):
        block = self.pyyaml_block("checks-requirements.txt")
        self.assertGreater(len(block), 1)
        self.assertEqual(block, self.pyyaml_block("requirements.txt"))


if __name__ == "__main__":
    unittest.main(argv=sys.argv)
