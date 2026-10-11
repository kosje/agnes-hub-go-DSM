"""Release provenance regression checks; no compiler or downloads needed."""
import unittest
import os
import sys
from unittest.mock import patch

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from build_spk import validate_build_provenance


class BuildProvenanceTests(unittest.TestCase):
    def check(self, outputs):
        with patch("build_spk.subprocess.check_output", side_effect=outputs):
            validate_build_provenance("agnes-hub-go-linux-amd64")

    def test_clean_matching_revision(self):
        self.check(["build vcs.revision=abc123\nbuild vcs.modified=false", "abc123\n", ""])

    def test_dirty_binary_rejected(self):
        with self.assertRaises(ValueError):
            self.check(["build vcs.revision=abc123\nbuild vcs.modified=true"])

    def test_missing_metadata_rejected(self):
        with self.assertRaises(ValueError):
            self.check(["go1.27.0 linux/amd64"])

    def test_wrong_revision_rejected(self):
        with self.assertRaises(ValueError):
            self.check(["build vcs.revision=abc123\nbuild vcs.modified=false", "def456\n"])

    def test_modified_source_rejected(self):
        with self.assertRaises(ValueError):
            self.check(["build vcs.revision=abc123\nbuild vcs.modified=false", "abc123\n", " M main.go\n"])


if __name__ == "__main__":
    unittest.main()
