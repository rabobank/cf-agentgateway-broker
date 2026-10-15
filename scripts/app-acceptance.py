"""Exercise bound credentials from the actual OpenCode CF container."""
import pathlib
import shlex
import subprocess

ROOT = pathlib.Path(__file__).resolve().parent.parent
ruby = r'''
require 'json'; require 'net/http'; require 'openssl'
services = JSON.parse(ENV.fetch('VCAP_SERVICES'))
credentials = services.fetch('agentgateway').first.fetch('credentials')
store = OpenSSL::X509::Store.new
store.set_default_paths
trusted_ca = credentials.fetch('trusted_ca')
certificates = trusted_ca.scan(/-----BEGIN CERTIFICATE-----.*?-----END CERTIFICATE-----/m)
raise 'binding has no CA certificate' if certificates.empty?
certificates.each { |pem| store.add_cert(OpenSSL::X509::Certificate.new(pem)) }
uri = URI(credentials.fetch('url') + '/models')
client = Net::HTTP.new(uri.host, uri.port)
client.use_ssl = true; client.cert_store = store; client.read_timeout = 180
req = Net::HTTP::Get.new(uri); req['Authorization'] = 'Bearer ' + credentials.fetch('api_key')
response = client.request(req)
puts "models status=#{response.code} body=#{response.body}"
raise 'models request failed' unless response.code == '200'
require 'resolv'
Resolv.getaddresses(uri.host).each do |address|
  replica = Net::HTTP.new(uri.host, uri.port)
  replica.ipaddr = address
  replica.use_ssl = true; replica.cert_store = store
  response = replica.request(req)
  puts "replica=#{address} models status=#{response.code}"
  raise 'replica models failed' unless response.code == '200'
end
uri = URI(credentials.fetch('url') + '/chat/completions')
[false, true].each do |stream|
  req = Net::HTTP::Post.new(uri)
  req['Authorization'] = 'Bearer ' + credentials.fetch('api_key')
  req['Content-Type'] = 'application/json'
  payload = {model: 'deepseek-v4-flash', messages: [{role: 'user', content: 'Reply with exactly: gateway works'}], max_tokens: 128, stream: stream}
  payload[:stream_options] = {include_usage: true} if stream
  req.body = JSON.generate(payload)
  response = client.request(req)
  puts "chat stream=#{stream} status=#{response.code} body=#{response.body[0,2000]}"
  raise 'chat request failed' unless response.code == '200'
  raise 'stream incomplete' if stream && !response.body.include?('[DONE]')
end
'''
command = "/home/vcap/deps/0/ruby/bin/ruby -e " + shlex.quote(ruby)
result = subprocess.run(['cf', 'ssh', 'opencode-demo', '-c', command], cwd=ROOT, capture_output=True, text=True, timeout=420)
print(result.stdout)
print(result.stderr)
raise SystemExit(result.returncode)
