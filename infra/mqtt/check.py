#!/usr/bin/env python3
"""MQTT 5 wire checks, TLS verification and database capture. Prints no secrets."""
import json,pathlib,socket,ssl,struct,time,urllib.request,uuid,sys
root=pathlib.Path(__file__).resolve().parents[2];base=root/'.secrets/mqtt'
g=json.loads((base/'gateway-settings.json').read_text())
host=g['url'].split('://')[1].split(':')[0]
def string(s):
 b=s.encode();return struct.pack('!H',len(b))+b
def varint(n):
 b=bytearray()
 while True:
  d=n%128;n//=128;b.append(d|(128 if n else 0))
  if not n:return bytes(b)
def send(s,header,body):s.sendall(bytes([header])+varint(len(body))+body)
def exact(s,n):
 out=b''
 while len(out)<n:
  part=s.recv(n-len(out))
  if not part:raise RuntimeError('Connection closed')
  out+=part
 return out
def packet(s):
 head=exact(s,1)[0];n=0;m=1
 while True:
  d=exact(s,1)[0];n+=(d&127)*m;m*=128
  if not d&128:break
 return head,exact(s,n)
ctx=ssl.create_default_context(cafile=str(base/'broker/ca.crt'))
def connect(password):
 s=socket.create_connection((host,1883 if '--plaintext' in sys.argv else 8883),timeout=5)
 if '--plaintext' not in sys.argv:s=ctx.wrap_socket(s,server_hostname=host)
 body=string('MQTT')+bytes([5,194])+struct.pack('!H',30)+b'\0'+string('aether-check-'+uuid.uuid4().hex[:8])+string(g['username'])+string(password)
 send(s,16,body);h,b=packet(s);assert h==32
 return s,b[1]
s,code=connect('deliberately-wrong-password');assert code!=0;s.close();print('PASS incorrect credentials rejected')
s,code=connect(g['password']);assert code==0
marker='synthetic-mqtt-check-'+uuid.uuid4().hex
for topic,mid,allowed in [(g['publish_topic'],1,True),('/mg3/not-your-gateway/status',2,False)]:
 send(s,50,string(topic)+struct.pack('!H',mid)+b'\0'+json.dumps({'aether_test':marker}).encode())
 h,b=packet(s);assert h==64
 reason=b[2] if len(b)>2 else 0
 assert (reason<128)==allowed,(topic,reason)
print('PASS allowed publish and cross-gateway publish ACL')
s.close()
owner=json.loads((root/'.secrets/owner.json').read_text());gateway=json.loads((root/'.secrets/minew-gateway.json').read_text())['gateway']['id']
settings=dict(x.split('=',1) for x in (root/'.env').read_text().splitlines() if x and not x.startswith('#'))
api='http://127.0.0.1:'+settings.get('API_PORT','8080')
req=urllib.request.Request(api+'/api/v1/auth/login',data=json.dumps(owner).encode(),headers={'Content-Type':'application/json','Origin':settings.get('APP_ORIGIN','http://localhost:3000')})
with urllib.request.urlopen(req) as r:token=json.load(r)['access_token']
for attempt in range(10):
 req=urllib.request.Request(api+'/api/v1/gateways/'+gateway+'/packets',headers={'Authorization':'Bearer '+token})
 with urllib.request.urlopen(req) as r:packets=json.load(r)['items']
 if any(x['payload'].get('aether_test')==marker for x in packets if isinstance(x['payload'],dict)):break
 time.sleep(1)
else:raise SystemExit('FAIL packet did not reach backend')
print('PASS synthetic MQTT packet stored through tenant-scoped backend')
