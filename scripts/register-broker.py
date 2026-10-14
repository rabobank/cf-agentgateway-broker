"""Register the homelab broker without printing its credentials."""
import json
import pathlib
import subprocess

ROOT = pathlib.Path(__file__).resolve().parent.parent


def bosh(command):
    result = subprocess.run(['bash', '-c', 'source ./bosh.env >/dev/null 2>&1 && ' + command], cwd=ROOT, capture_output=True, text=True, check=True)
    return result.stdout


output = bosh("bosh -d agentgateway-broker ssh broker/0 -c 'sudo cat /var/vcap/jobs/agentgateway-broker/config/config.json'")
text = '\n'.join(line.split('stdout | ', 1)[1] for line in output.splitlines() if 'stdout | ' in line)
config = json.loads(text)
output = bosh("bosh -d agentgateway-broker ssh broker/0 -c 'sudo cat /var/vcap/jobs/agentgateway-broker/config/ca.crt'")
ca = '\n'.join(line.split('stdout | ', 1)[1] for line in output.splitlines() if 'stdout | ' in line) + '\n'
work = ROOT / 'test/.work'
work.mkdir(parents=True, exist_ok=True)
(work / 'gateway-ca.crt').write_text(ca)
result = subprocess.run(['bash', '-c', 'source ./bosh.env >/dev/null 2>&1 && bosh -d cf scp test/.work/gateway-ca.crt control/0:/tmp/agentgateway-ca.crt'], cwd=ROOT, capture_output=True, text=True)
if result.returncode:
    raise RuntimeError('Cannot copy broker CA to CF control')
bosh("bosh -d cf ssh control/0 -c 'sudo cp /tmp/agentgateway-ca.crt /usr/local/share/ca-certificates/agentgateway-ca.crt && sudo update-ca-certificates'")
brokers = subprocess.run(['cf', 'service-brokers'], capture_output=True, text=True, check=True).stdout
action = 'update-service-broker' if any(line.split() and line.split()[0] == 'agentgateway' for line in brokers.splitlines()) else 'create-service-broker'
result = subprocess.run(['cf', action, 'agentgateway', config['username'], config['password'], 'https://broker.agentgateway.internal:8443'], cwd=ROOT, capture_output=True, text=True)
print(result.stdout)
if result.returncode:
    print(result.stderr)
    raise SystemExit(result.returncode)
subprocess.run(['cf', 'enable-service-access', 'agentgateway'], check=True)
