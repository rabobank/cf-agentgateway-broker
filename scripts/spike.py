"""Two real gateways + PostgreSQL + deterministic OpenAI fixture; no provider spend."""
import json
import os
import base64
import pathlib
import socket
import subprocess
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = pathlib.Path(__file__).resolve().parent.parent
WORK = ROOT / 'test/.work/spike'
WORK.mkdir(parents=True, exist_ok=True)


def port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


class Provider(BaseHTTPRequestHandler):
    def log_message(self, format, *args):
        pass

    def do_POST(self):
        assert self.headers.get('Authorization') == 'Bearer provider-fixture', 'tenant key reached provider'
        request = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        self.send_response(200)
        if request.get('stream'):
            self.send_header('Content-Type', 'text/event-stream')
            self.end_headers()
            for chunk in [
                {'id': 'fixture', 'object': 'chat.completion.chunk', 'created': 1, 'model': 'fixture-model', 'choices': [{'index': 0, 'delta': {'role': 'assistant', 'content': 'ok'}, 'finish_reason': None}]},
                {'id': 'fixture', 'object': 'chat.completion.chunk', 'created': 1, 'model': 'fixture-model', 'choices': [{'index': 0, 'delta': {}, 'finish_reason': 'stop'}], 'usage': {'prompt_tokens': 1, 'completion_tokens': 1, 'total_tokens': 2}},
            ]:
                self.wfile.write(('data: ' + json.dumps(chunk) + '\n\n').encode())
            self.wfile.write(b'data: [DONE]\n\n')
        else:
            self.send_header('Content-Type', 'application/json')
            self.end_headers()
            self.wfile.write(json.dumps({'id': 'fixture', 'object': 'chat.completion', 'created': 1, 'model': 'fixture-model', 'choices': [{'index': 0, 'message': {'role': 'assistant', 'content': 'ok'}, 'finish_reason': 'stop'}], 'usage': {'prompt_tokens': 1, 'completion_tokens': 1, 'total_tokens': 2}}).encode())


def request(url, method='GET', body=None, key=None, broker=False):
    headers = {}
    if key:
        headers['Authorization'] = 'Bearer ' + key
    if broker:
        headers['Authorization'] = 'Basic ' + base64.b64encode(b'broker:broker-fixture').decode()
        headers['X-Broker-API-Version'] = '2.16'
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        headers['Content-Type'] = 'application/json'
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=10) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def eventually(fn, description, timeout=25):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        try:
            if fn():
                print('PASS:', description, flush=True)
                return
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(.25)
    raise AssertionError(description)


processes = []
logs = []
pg_started = False
provider = ThreadingHTTPServer(('127.0.0.1', port()), Provider)
threading.Thread(target=provider.serve_forever, daemon=True).start()
pg_port = port()
pgdata = WORK / 'postgres'


def run(*args):
    subprocess.run(args, check=True, stdout=subprocess.DEVNULL)


def start_gateway(index, config):
    log = (WORK / f'gateway-{index}.log').open('a')
    logs.append(log)
    env = dict(os.environ, LD_LIBRARY_PATH=str(ROOT / 'cf/dist/gateway/lib'))
    binary = os.environ.get('GATEWAY_BINARY', str(ROOT / 'cf/dist/gateway/bin/agentgateway'))
    process = subprocess.Popen([binary, '-f', str(config)], stdout=log, stderr=subprocess.STDOUT, env=env)
    processes.append(process)
    return process


try:
    if not pgdata.exists():
        run('initdb', '-D', str(pgdata), '-A', 'trust', '-U', 'postgres')
    run('pg_ctl', '-D', str(pgdata), '-l', str(WORK / 'postgres.log'), '-o', f'-p {pg_port} -h 127.0.0.1 -k {WORK}', '-w', 'start')
    pg_started = True
    run('psql', '-h', '127.0.0.1', '-p', str(pg_port), '-U', 'postgres', '-c', 'DROP DATABASE IF EXISTS gateway_spike')
    run('createdb', '-h', '127.0.0.1', '-p', str(pg_port), '-U', 'postgres', 'gateway_spike')
    catalog = {'providers': {'openai': {'models': {'fixture-model': {'rates': {'input': '1000000', 'output': '1000000'}}}}}}
    (WORK / 'catalog.json').write_text(json.dumps(catalog))
    endpoints = []
    configs = []
    for index in range(2):
        inference, admin, management, ready, stats = [port() for _ in range(5)]
        config = {
            'config': {'adminAddr': f'127.0.0.1:{admin}', 'readinessAddr': f'127.0.0.1:{ready}', 'statsAddr': f'127.0.0.1:{stats}', 'storage': {'mode': 'hybrid'}, 'database': {'url': f'postgres://postgres@127.0.0.1:{pg_port}/gateway_spike?sslmode=disable'}, 'modelCatalog': [{'file': str(WORK / 'catalog.json')}]},
            'gateways': {'inference': {'port': inference, 'bindAddress': '127.0.0.1'}, 'management': {'port': management, 'bindAddress': '127.0.0.1'}},
            'llm': {'gateways': ['inference'], 'models': [{'name': 'fixture-model', 'provider': 'openAI', 'params': {'model': 'fixture-model', 'apiKey': 'provider-fixture', 'baseUrl': f'http://127.0.0.1:{provider.server_port}/v1'}}], 'policies': {'apiKey': {'mode': 'strict', 'keys': []}}},
            'ui': {'gateways': ['management'], 'policies': {'apiKey': {'mode': 'strict', 'keys': [{'key': 'management-fixture'}]}}},
        }
        path = WORK / f'gateway-{index}.json'
        path.write_text(json.dumps(config))
        configs.append(path)
        endpoints.append((f'http://127.0.0.1:{inference}', f'http://127.0.0.1:{management}'))
        start_gateway(index, path)
        eventually(lambda url=endpoints[-1][1]: request(url + '/api/runtime', key='management-fixture')[0] == 200, f'replica {index} management API without UI assets')
    assert request(endpoints[0][1] + '/api/config/resources/llm.apiKey')[0] == 401
    print('PASS: management requires its own credential', flush=True)
    key = {'key': 'sk-shared-fixture', 'metadata': {'name': 'shared-fixture', 'cf.agentgateway-broker/instance': 'instance'}, 'allowedModels': ['fixture-model'], 'budgets': [{'name': 'shared', 'limit': {'unit': 'USD', 'amount': 3}, 'window': {'rolling': '24h'}, 'onBudgetExceeded': 'Block'}]}
    status, body = request(endpoints[0][1] + '/api/config/resources/llm.apiKey', 'PUT', {'resources': [{'value': key}]}, 'management-fixture')
    assert status == 200, (status, body)
    item = json.loads(body)['resources'][0]
    assert item['value']['key'] == key['key'] and item['value']['metadata']['cf.agentgateway-broker/instance'] == 'instance'
    print('PASS: raw credential and metadata round trip', flush=True)
    for index, (url, _) in enumerate(endpoints):
        eventually(lambda: request(url + '/v1/models', key=key['key'])[0] == 200, f'key active on replica {index}')
    chat = {'model': 'fixture-model', 'messages': [{'role': 'user', 'content': 'test'}]}
    status, body = request(endpoints[0][0] + '/v1/chat/completions', 'POST', chat, key['key'])
    assert status == 200, (status, body)
    chat['stream'] = True
    chat['stream_options'] = {'include_usage': True}
    status, body = request(endpoints[1][0] + '/v1/chat/completions', 'POST', chat, key['key'])
    assert status == 200 and b'[DONE]' in body, (status, body)
    for index, (url, _) in enumerate(endpoints):
        eventually(lambda: request(url + '/v1/chat/completions', 'POST', chat, key['key'])[0] == 429, f'shared USD exhaustion reaches replica {index}')
    processes[1].terminate(); processes[1].wait(timeout=30)
    start_gateway(1, configs[1])
    eventually(lambda: request(endpoints[1][0] + '/v1/chat/completions', 'POST', chat, key['key'])[0] == 429, 'restart retains budget usage')
    status, body = request(endpoints[0][1] + '/api/config/resources/llm.apiKey/' + item['id'], 'DELETE', key='management-fixture')
    assert status == 200, (status, body)
    for index, (url, _) in enumerate(endpoints):
        eventually(lambda: request(url + '/v1/models', key=key['key'])[0] == 401, f'deletion revokes replica {index}')
    broker_port = port()
    broker_config = {'listen': f'127.0.0.1:{broker_port}', 'username': 'broker', 'password': 'broker-fixture', 'installation': 'spike', 'gateway_url': endpoints[0][0] + '/v1', 'management_url': endpoints[0][1], 'management_token': 'management-fixture', 'service_id': 'service', 'plan_id': 'plan', 'models': ['fixture-model'], 'budget': 100, 'window': '24h'}
    config_path = WORK / 'broker.json'
    config_path.write_text(json.dumps(broker_config))
    broker_log = (WORK / 'broker.log').open('w'); logs.append(broker_log)
    process = subprocess.Popen([str(ROOT / 'cf/dist/broker/bin/broker'), '-config', str(config_path)], stdout=broker_log, stderr=subprocess.STDOUT)
    processes.append(process)
    broker_url = f'http://127.0.0.1:{broker_port}'
    eventually(lambda: request(broker_url + '/ready')[0] == 200, 'real broker readiness against gateway API')
    instance_path = '/v2/service_instances/osb-instance'
    provision = {'service_id': 'service', 'plan_id': 'plan', 'organization_guid': 'org', 'space_guid': 'space'}
    assert request(broker_url + instance_path, 'PUT', provision, broker=True)[0] == 201
    assert request(broker_url + instance_path, 'PUT', provision, broker=True)[0] == 200
    credentials = []
    for binding_id in ['app', 'service-key']:
        status, body = request(broker_url + instance_path + '/service_bindings/' + binding_id, 'PUT', {'service_id': 'service', 'plan_id': 'plan'}, broker=True)
        assert status == 201, (status, body)
        credentials.append(json.loads(body)['credentials'])
    assert credentials[0] == credentials[1]
    recovered = credentials[0]['api_key']
    for index, (url, _) in enumerate(endpoints):
        eventually(lambda: request(url + '/v1/models', key=recovered)[0] == 200, f'OSB-created key active on replica {index}')
    assert request(broker_url + instance_path + '/service_bindings/app?service_id=service&plan_id=plan', 'DELETE', broker=True)[0] == 200
    assert request(endpoints[0][0] + '/v1/models', key=recovered)[0] == 200
    assert request(broker_url + instance_path + '?service_id=service&plan_id=plan', 'DELETE', broker=True)[0] == 200
    for index, (url, _) in enumerate(endpoints):
        eventually(lambda: request(url + '/v1/models', key=recovered)[0] == 401, f'OSB deprovision revokes replica {index}')
    print('PASS: real OSB lifecycle, retry and shared binding/service-key credentials', flush=True)
    print('SPIKE PASSED', flush=True)
finally:
    for process in processes:
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=30)
            except subprocess.TimeoutExpired:
                process.kill(); process.wait()
    for log in logs:
        log.close()
    provider.shutdown()
    if pg_started:
        run('pg_ctl', '-D', str(pgdata), '-m', 'fast', '-w', 'stop')
