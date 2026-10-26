"""Generate ignored CF vars with fresh secrets and platform route CA trust."""
import json
import os
import pathlib
import secrets
import ssl
import argparse
import socket

root = pathlib.Path(__file__).resolve().parent.parent
parser = argparse.ArgumentParser()
parser.add_argument('--trusted-ca', required=True, help='PEM issuer CA bundle for the inference CF route')
args = parser.parse_args()
host = 'agentgateway.10.246.0.21.sslip.io'
ca = pathlib.Path(args.trusted_ca).read_text()
trusted = ssl.create_default_context(cadata=ca)
with socket.create_connection((host, 443), timeout=10) as conn:
    with trusted.wrap_socket(conn, server_hostname=host):
        pass
vars = {
    'domain': '10.246.0.21.sslip.io',
    'management_token': secrets.token_urlsafe(32),
    'broker_password': secrets.token_urlsafe(32),
    'trusted_ca': ca,
    'gateway_models': json.dumps([{'name': 'deepseek-v4-flash', 'provider': 'openAI', 'params': {'model': 'deepseek-v4-flash', 'baseUrl': 'http://192.168.2.148:8000/v1'}}]),
    'model_catalog': json.dumps({'providers': {'openai': {'models': {'deepseek-v4-flash': {'rates': {'input': '1', 'output': '1'}}}}}}),
}
path = root / 'cf/local-vars.yml'
if path.exists():
    raise SystemExit('Local vars already exist; preserve deployed secrets or remove deliberately')
with open(path, 'x', opener=lambda name, flags: os.open(name, flags, 0o600)) as output:
    json.dump(vars, output, indent=2)
print('Created ignored CF deployment vars; no secret values printed')
