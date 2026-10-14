"""Run a long build with a durable log; inspect with context-mode execute_file."""
import pathlib
import subprocess
import sys

root = pathlib.Path(__file__).resolve().parent.parent
work = root / "test/.work"
work.mkdir(parents=True, exist_ok=True)
log = work / (sys.argv[1] + ".log")
with log.open("w") as output:
    result = subprocess.run(["devbox", "run", "--", "bash", "scripts/" + sys.argv[1]], cwd=root, stdout=output, stderr=subprocess.STDOUT)
print("Exit status:", result.returncode, "Log:", log)
sys.exit(result.returncode)
