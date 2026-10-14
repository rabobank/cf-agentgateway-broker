require 'json'
require 'yaml'
require 'bosh/template/test'

RSpec.describe 'release templates' do
  let(:release) { Bosh::Template::Test::ReleaseDir.new(File.expand_path('..', __dir__)) }

  it 'serializes broker credentials without shell or JSON injection' do
    properties = {'broker' => {
      'password' => "quote\"\nsecret", 'gateway_url' => 'https://gateway.test/v1',
      'management_url' => 'https://gateway.test:8444', 'management_token' => 'token',
      'models' => ['test-model'], 'tls' => {'certificate' => 'cert', 'private_key' => 'key', 'ca' => 'ca'}
    }}
    rendered = release.job('agentgateway-broker').template('config/config.json').render(properties)
    config = JSON.parse(rendered)
    expect(config['password']).to eq("quote\"\nsecret")
    expect(config['budget']).to eq(10)
    expect(config['management_token']).to eq('token')
  end

  it 'separates tenant and management authentication and encodes database credentials' do
    properties = {'gateway' => {
      'management_token' => 'management-secret', 'database' => {'password' => 'p@ss:/ word'},
      'models' => [{'name' => 'model', 'provider' => 'openAI', 'params' => {'apiKey' => 'provider-secret'}}],
      'catalog' => {}, 'tls' => {'certificate' => 'cert', 'private_key' => 'key'}
    }}
    postgres = Bosh::Template::Test::Link.new(
      name: 'postgres', properties: {'databases' => {'port' => 5432}},
      instances: [Bosh::Template::Test::LinkInstance.new(address: '10.246.0.101')]
    )
    rendered = release.job('agentgateway').template('config/config.json').render(properties, consumes: [postgres])
    config = JSON.parse(rendered)
    expect(config.dig('llm', 'gateways')).to eq(['inference'])
    expect(config.dig('llm', 'policies', 'apiKey', 'keys')).to eq([])
    expect(config.dig('ui', 'gateways')).to eq(['management'])
    expect(config.dig('ui', 'policies', 'apiKey', 'keys', 0, 'key')).to eq('management-secret')
    expect(config.dig('config', 'adminAddr')).to eq('127.0.0.1:15000')
    expect(config.dig('config', 'database', 'url')).to include('p%40ss%3A%2F%20word@10.246.0.101:5432')
  end
end
