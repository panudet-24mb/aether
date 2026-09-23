#!/usr/bin/env python3
"""Point the confirmed MG3 at Aether. Saves original config privately first.
Uploads only the public CA; sends MQTT credentials to the device's documented
local management API. Run only on the trusted setup LAN.
"""
import json, os, pathlib, urllib.request, uuid
root=pathlib.Path(__file__).resolve().parents[2]
base=root/'.secrets/mqtt';os.umask(0o077)
opener=urllib.request.build_opener(urllib.request.ProxyHandler({}))
def call(path,body=None,kind='application/json'):
 req=urllib.request.Request('http://192.168.1.77'+path,data=body,headers={'Content-Type':kind})
 with opener.open(req,timeout=15) as r:return json.loads(r.read(65536))
hello=call('/hello')
if hello.get('mac','').lower()!='ac233fc274eb' or hello.get('model')!='mg3-a-lychee':raise SystemExit('Gateway identity mismatch; stopped')
backup=base/'original-gateway-config.json'
if not backup.exists():
 old=call('/set',json.dumps({'action':'GetConfig'}).encode())
 if old.get('code')!=200 or 'mqtt' not in old or 'common' not in old:raise SystemExit('Cannot save original config; stopped')
 with backup.open('x') as f:json.dump(old,f)
settings=json.loads((base/'gateway-settings.json').read_text())
ca=(base/'broker/ca.crt').read_bytes()
if len(ca)>=2048:raise SystemExit('CA exceeds documented MG3 upload limit')
r=call('/upload?type=ca.crt',ca,'application/octet-stream')
if r.get('code')!=200:raise SystemExit('CA upload failed; settings unchanged')
old=json.loads(backup.read_text())
mqtt=dict(old['mqtt'])
mqtt.update({'mqtt_url':settings['url'],'username':settings['username'],'password':settings['password'],'client_id':settings['client_id'],'publish_topic':settings['publish_topic'],'subscribe_topic':settings['subscribe_topic'],'response_topic':settings['response_topic'],'qos':1,'keepalive':120,'use_ssl':1})
common=dict(old['common']);common['protocol']='mqtt'
request={'action':'SetConfig','requestId':str(uuid.uuid4()),'mqtt':mqtt,'common':common}
r=call('/set',json.dumps(request).encode())
if r.get('code')!=200:raise SystemExit('Gateway rejected configuration; check locally')
check=call('/set',json.dumps({'action':'GetConfig'}).encode()).get('mqtt',{})
if any(check.get(k)!=mqtt[k] for k in ('mqtt_url','use_ssl','qos','client_id','publish_topic')):
 raise SystemExit('Gateway acknowledged SetConfig but read-back differs. Use Gateway Configurer; do not assume it is connected.')
print('MQTT configuration read-back verified; delivery still requires verification.')
