"""topology/ca.pem is the public Bluebox extension CA and nothing else."""
import pathlib
import subprocess
import unittest

CA = pathlib.Path(__file__).with_name("ca.pem")


class CAIsPublic(unittest.TestCase):
    def test_holds_one_certificate_and_no_key(self):
        text = CA.read_text()
        self.assertEqual(text.count("BEGIN CERTIFICATE"), 1)
        self.assertNotIn("PRIVATE KEY", text)

    def test_is_a_ca(self):
        out = subprocess.run(["openssl", "x509", "-in", str(CA), "-noout", "-ext", "basicConstraints"],
                             capture_output=True, text=True, check=True).stdout
        self.assertIn("CA:TRUE", out)


if __name__ == "__main__":
    unittest.main()
