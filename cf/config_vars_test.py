"""Executable contracts for YAML application configuration conversion."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


class ConfigVarsTests(unittest.TestCase):
    def convert(self, text):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "operator.yml"
            path.write_text(text)
            return subprocess.run(
                [sys.executable, str(Path(__file__).with_name("config-vars")),
                 "broker_config", str(path)], capture_output=True, text=True)

    def test_json_string_preserves_native_values(self):
        result = self.convert('budget: 1.25\nfree: true\nrates: {input: "0.25"}\nca: |\n  PEM\n')
        self.assertEqual(result.returncode, 0, result.stderr)
        value = json.loads(json.loads(result.stdout)["broker_config"])
        self.assertEqual(value, {"budget": 1.25, "free": True,
                                 "rates": {"input": "0.25"}, "ca": "PEM\n"})

    def test_invalid_yaml_fails_without_partial_output_or_secrets(self):
        for text in ['password: secret-marker\npassword: other\n',
                     'x: .nan\n', 'x: 1\n---\nx: 2\n',
                     'true: value\n', 'password: [secret-marker\n']:
            with self.subTest(text=text):
                result = self.convert(text)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")
                self.assertNotIn("secret-marker", result.stderr)

    def test_cross_file_validation_rejects_unknown_resources(self):
        with tempfile.TemporaryDirectory() as directory:
            broker = Path(directory) / "broker.yml"
            gateway = Path(directory) / "gateway.yml"
            broker.write_text('installation: lab\nservices:\n- protocol: llm\n  plans:\n  - access: {models: [missing]}\n')
            gateway.write_text('llm: {models: []}\ncapsules: {log_id: lab}\n')
            result = subprocess.run([sys.executable, str(Path(__file__).with_name("config-vars")),
                                     "--check", str(broker), str(gateway)], capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("unknown model", result.stderr)


if __name__ == "__main__":
    unittest.main()
