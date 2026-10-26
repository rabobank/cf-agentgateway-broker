"""Replace disposable gateway service instances after updating broker registration.

Only changes agentgateway bindings. Does not restart consumer apps.
"""
import subprocess


def cf(*args):
    p = subprocess.run(['cf', *args], capture_output=True, text=True)
    if p.returncode:
        raise RuntimeError('CF lifecycle failed: ' + ' '.join(args[:2]))
    return p.stdout


for service, app in [('agentgateway-opencode', 'opencode-demo'), ('agentgateway-rabo-os', 'rabo-os-demo')]:
    cf('unbind-service', app, service)
    cf('delete-service', service, '-f')
    cf('create-service', 'agentgateway', 'shared', service)
    cf('bind-service', app, service)
    print('Recreated', service, '->', app, '(restart pending)')
