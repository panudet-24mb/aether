#!/usr/bin/env python3
"""Provision this installation's first MG3. Secrets stay in .secrets (no stdout).
Run once: python3 infra/mqtt/provision.py --email ADDRESS --host LAN_IP
Existing owner credentials may be placed in .secrets/owner.json first.
"""
import argparse, base64, ipaddress, json, os, pathlib, secrets, subprocess, urllib.request
root=pathlib.Path(__file__).resolve().parents[2]
p=argparse.ArgumentParser();p.add_argument('--email',required=True);p.add_argument('--host',required=True);a=p.parse_args()
ipaddress.ip_address(a.host)
os.umask(0o077)
sec=root/'.secrets';sec.mkdir(exist_ok=True,mode=0o700)
settings=dict(x.split('=',1) for x in (root/'.env').read_text().splitlines() if x and not x.startswith('#'))
def write(path,data,mode=0o600):
 path.write_text(data);path.chmod(mode)
def run(args,**kwargs):
 subprocess.run(args,check=True,stdout=subprocess.DEVNULL,**kwargs)
ownerfile=sec/'owner.json'
if not ownerfile.exists():
 owner={'email':a.email,'password':secrets.token_urlsafe(32)}
 write(ownerfile,json.dumps(owner))
 write(sec/'owner-password.txt',owner['password'])
 env=dict(os.environ,ADMIN_EMAIL=a.email,ADMIN_NAME='Panudet',TENANT_NAME='Aether',ADMIN_PASSWORD_FILE=str(sec/'owner-password.txt'))
 run(['python3','infra/backend-local.py','bootstrap'],cwd=root,env=env)
owner=json.loads(ownerfile.read_text())
origin=settings.get('APP_ORIGIN','http://localhost:3000')
def api(path,data,token=None):
 headers={'Content-Type':'application/json','Origin':origin}
 if token:headers['Authorization']='Bearer '+token
 req=urllib.request.Request('http://127.0.0.1:'+settings.get('API_PORT','8080')+path,data=json.dumps(data).encode(),headers=headers)
 with urllib.request.urlopen(req,timeout=15) as r:return json.load(r)
login=api('/api/v1/auth/login',owner)
gfile=sec/'minew-gateway.json'
if not gfile.exists():
 g=api('/api/v1/gateways',{'name':'Minew MG3 — MHS','model':'minew-mg3'},login['access_token'])
 write(gfile,json.dumps(g))
g=json.loads(gfile.read_text())
base=sec/'mqtt'
if base.exists():raise SystemExit('MQTT configuration exists; preserving certificates and credentials')
base.mkdir();broker=base/'broker';broker.mkdir(mode=0o755);collector=base/'collector';collector.mkdir(mode=0o755)
user='mg3-ac233fc274eb';password=secrets.token_urlsafe(32);workerpass=secrets.token_urlsafe(32)
# Separate offline CA key from the broker mounts. RSA certificates fit MG3's 2KB upload limit.
run(['openssl','req','-x509','-newkey','rsa:2048','-nodes','-days','3650','-subj','/CN=Aether Local MQTT CA','-keyout',str(base/'ca.key'),'-out',str(broker/'ca.crt')],stderr=subprocess.DEVNULL)
run(['openssl','req','-new','-newkey','rsa:2048','-nodes','-subj','/CN=Aether MQTT','-keyout',str(broker/'server.key'),'-out',str(base/'server.csr')],stderr=subprocess.DEVNULL)
write(base/'server.ext',f'subjectAltName=IP:{a.host},DNS:mqtt\nbasicConstraints=CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n')
run(['openssl','x509','-req','-in',str(base/'server.csr'),'-CA',str(broker/'ca.crt'),'-CAkey',str(base/'ca.key'),'-CAcreateserial','-days','365','-sha256','-extfile',str(base/'server.ext'),'-out',str(broker/'server.crt')],stderr=subprocess.DEVNULL)
write(broker/'passwords',user+':'+password+'\naether-ingest:'+workerpass+'\n')
run(['docker','run','--rm','--user','0','--entrypoint','mosquitto_passwd','-v',str(broker)+':/config','eclipse-mosquitto:2.0.22','-U','/config/passwords'])
write(broker/'acl',f'user {user}\ntopic write /mg3/ac233fc274eb/status\ntopic write /mg3/ac233fc274eb/response\ntopic read /mg3/ac233fc274eb/action\n\nuser aether-ingest\ntopic read /mg3/ac233fc274eb/status\n')
write(broker/'mosquitto.conf','''listener 8883
allow_anonymous false
password_file /mosquitto/config/passwords
acl_file /mosquitto/config/acl
certfile /mosquitto/config/server.crt
keyfile /mosquitto/config/server.key
tls_version tlsv1.2
require_certificate false
persistence true
persistence_location /mosquitto/data/
autosave_interval 30
max_packet_size 1048576
max_queued_messages 1000
max_queued_bytes 16777216
max_inflight_messages 20
max_connections 20
memory_limit 67108864
log_dest stdout
log_type error
log_type warning
log_type notice
connection_messages true
''')
for f in broker.iterdir():f.chmod(0o444)
for name in ('passwords','acl','server.key'):(broker/name).chmod(0o400)
run(['docker','volume','create','aether_mqtt-data'])
run(['docker','run','--rm','--user','0','--entrypoint','chown','-v','aether_mqtt-data:/data','eclipse-mosquitto:2.0.22',f'{os.getuid()}:{os.getgid()}','/data'])
write(collector/'ca.crt',(broker/'ca.crt').read_text(),0o444)
write(collector/'collector.json',json.dumps({'broker_url':'ssl://mqtt:8883','username':'aether-ingest','password':workerpass,'client_id':'aether-ingest-mhs-v1','ca_file':'/run/mqtt/ca.crt','bindings':[{'topic':'/mg3/ac233fc274eb/status','gateway_id':g['gateway']['id'],'token':g['token']}]}),0o444)
write(base/'gateway-settings.json',json.dumps({'url':'mqtts://'+a.host+':8883','username':user,'password':password,'client_id':'ac233fc274eb','publish_topic':'/mg3/ac233fc274eb/status','subscribe_topic':'/mg3/ac233fc274eb/action','response_topic':'/mg3/ac233fc274eb/response','qos':1,'keepalive':120},indent=2))
with (root/'.env').open('a') as f:f.write(f'\nMQTT_BIND_IP={a.host}\nMQTT_UID={os.getuid()}\nMQTT_GID={os.getgid()}\n')
print('Owner, gateway, TLS certificates and MQTT ACL provisioned. Secrets saved under .secrets; no passwords printed.')
