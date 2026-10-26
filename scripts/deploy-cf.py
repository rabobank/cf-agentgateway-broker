"""Push CF apps and wire private management access. Does not recreate consumers."""
import json
import pathlib
import subprocess

root = pathlib.Path(__file__).resolve().parent.parent
vars = json.loads((root / 'cf/local-vars.yml').read_text())
subprocess.run(['cf', 'target', '-o', 'poc', '-s', 'demo'], capture_output=True, check=True)


def cf(*args):
    result = subprocess.run(['cf', *args], cwd=root, capture_output=True, text=True)
    if result.returncode:
        # CF push may print manifest secrets; do not forward raw deployment output.
        safe = result.stdout + result.stderr
        for secret in [vars['management_token'], vars['broker_password']]:
            safe = safe.replace(secret, '[REDACTED]')
        print(safe[-5000:])
        raise RuntimeError('CF command failed: ' + ' '.join(args[:2]))
    print('Completed:', ' '.join(args[:2]), flush=True)
    return result.stdout


cf('push', 'agentgateway', '-f', 'cf/manifest.yml', '--vars-file', 'cf/local-vars.yml', '--no-start')
cf('push', 'agentgateway-broker', '-f', 'cf/manifest.yml', '--vars-file', 'cf/local-vars.yml', '--no-start')
cf('map-route', 'agentgateway', 'apps.internal', '--hostname', 'agentgateway-management')
cf('add-network-policy', 'agentgateway-broker', 'agentgateway', '--protocol', 'tcp', '--port', '9090')
cf('bind-security-group', 'dgx-spark-model-egress', 'poc', '--space', 'demo')
cf('start', 'agentgateway')
cf('start', 'agentgateway-broker')
# Broker registration goes through the public TLS route; gateway management stays private.
cf('update-service-broker', 'agentgateway', 'broker', vars['broker_password'], 'https://agentgateway-broker.' + vars['domain'])
cf('enable-service-access', 'agentgateway')
