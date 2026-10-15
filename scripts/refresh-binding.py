"""Refresh a CF app binding and verify trust without printing credentials.

Does not restart the app. Coordinate with its owner before running.
"""
import argparse
import json
import pathlib
import subprocess
import ssl
import _ssl
import tempfile
import time
import urllib.parse

parser = argparse.ArgumentParser()
parser.add_argument('--service', required=True)
parser.add_argument('--app', required=True)
parser.add_argument('--space-guid', required=True)
parser.add_argument('--verify-only', action='store_true')
args = parser.parse_args()


def cf_json(path):
    result = subprocess.run(['cf', 'curl', path], capture_output=True, text=True, check=True)
    value = json.loads(result.stdout)
    if 'errors' in value:
        raise RuntimeError('CF API request failed')
    return value


def resource(kind, name):
    query = urllib.parse.urlencode({'names': name, 'space_guids': args.space_guid})
    resources = cf_json('/v3/' + kind + '?' + query)['resources']
    if len(resources) != 1:
        raise RuntimeError('Expected one ' + kind + ' in specified space')
    return resources[0]


def credentials(service_id, app_id):
    query = urllib.parse.urlencode({'service_instance_guids': service_id, 'app_guids': app_id, 'type': 'app'})
    resources = cf_json('/v3/service_credential_bindings?' + query)['resources']
    if len(resources) != 1:
        raise RuntimeError('Expected one existing app binding')
    return cf_json('/v3/service_credential_bindings/' + resources[0]['guid'] + '/details')['credentials']


service = resource('service_instances', args.service)
app = resource('apps', args.app)
before = credentials(service['guid'], app['guid'])
for action in ([] if args.verify_only else ['unbind-service', 'bind-service']):
    result = subprocess.run(['cf', action, args.app, args.service], capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError(action + ' failed; inspect CF operation status')
after = credentials(service['guid'], app['guid'])
if before['api_key'] != after['api_key'] or before['url'] != after['url']:
    raise RuntimeError('Shared credential changed unexpectedly')
trusted_ca = after.get('trusted_ca', '')
if not isinstance(trusted_ca, str) or not trusted_ca:
    raise RuntimeError('Binding has no trusted_ca PEM')
with tempfile.TemporaryDirectory() as directory:
    path = pathlib.Path(directory) / 'ca.pem'
    path.write_text(trusted_ca)
    certificate = getattr(_ssl, '_test_decode_cert')(str(path))
    if not ssl.cert_time_to_seconds(certificate['notBefore']) <= time.time() < ssl.cert_time_to_seconds(certificate['notAfter']):
        raise RuntimeError('trusted_ca certificate is not currently valid')
    context = ssl.create_default_context(cafile=str(path))
    if not context.get_ca_certs():
        raise RuntimeError('trusted_ca contains no usable CA certificate')
print('Binding refreshed; fields: url, api_key, trusted_ca')
print('trusted_ca: PEM present, parseable, currently valid, accepted as TLS trust CA')
print('Shared key and URL preserved. App was not restarted.')
