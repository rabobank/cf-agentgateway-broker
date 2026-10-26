"""Verify an existing key on each gateway container without printing credentials."""
import base64
import json
import subprocess


def get(path):
    return json.loads(subprocess.run(['cf', 'curl', path], capture_output=True, text=True, check=True).stdout)


service = get('/v3/service_instances?names=agentgateway-opencode')['resources'][0]
binding = get('/v3/service_credential_bindings?service_instance_guids=' + service['guid'])['resources'][0]
credentials = get('/v3/service_credential_bindings/' + binding['guid'] + '/details')['credentials']
payload = base64.b64encode(credentials['api_key'].encode()).decode()
for index in range(3):
    command = 'key=$(printf %s ' + payload + ' | base64 -d); status=$(/bin/curl -sS -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $key" http://localhost:$PORT/v1/models); test "$status" = 200'
    result = subprocess.run(['cf', 'ssh', 'agentgateway', '-i', str(index), '-c', command], capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError('Replica ' + str(index) + ' did not accept existing key')
    print('Replica', index, 'accepts existing persisted key')
