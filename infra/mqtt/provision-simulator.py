#!/usr/bin/env python3
"""Add a dedicated SIM gateway to the current owner workspace. Never prints secrets."""
import json,os,pathlib,secrets,subprocess,urllib.request
root=pathlib.Path(__file__).resolve().parents[2];os.umask(0o077)
sec=root/'.secrets';sim=sec/'simulator';sim.mkdir(exist_ok=True)
runtime=sim/'runtime';runtime.mkdir(exist_ok=True,mode=0o755)
settings=dict(x.split('=',1) for x in (root/'.env').read_text().splitlines() if x and not x.startswith('#'))
op=urllib.request.build_opener(urllib.request.ProxyHandler({}));base='http://127.0.0.1:'+settings.get('API_PORT','8080')
def api(path,data,token=None):
 headers={'Content-Type':'application/json','Origin':settings.get('APP_ORIGIN','http://localhost:3001')}
 if token:headers['Authorization']='Bearer '+token
 with op.open(urllib.request.Request(base+path,data=json.dumps(data).encode(),headers=headers),timeout=15) as r:return json.load(r)
login=api('/api/v1/auth/login',json.loads((sec/'owner.json').read_text()))
gfile=sim/'gateway.json'
if not gfile.exists():
 g=api('/api/v1/gateways',{'name':'SIM · Virtual MG3 · 4 environmental sensors','model':'minew-mg3'},login['access_token'])
 gfile.write_text(json.dumps(g))
g=json.loads(gfile.read_text());gid=g['gateway']['id'];topic='/aether/simulation/'+gid+'/status';username='sim-'+gid
config=runtime/'config.json'
if not config.exists():config.write_text(json.dumps({'url':'ssl://mqtt:8883','username':username,'password':secrets.token_urlsafe(32),'topic':topic,'ca':'/run/simulator/ca.crt'}));config.chmod(0o444)
cfg=json.loads(config.read_text());broker=sec/'mqtt/broker'
cafile=runtime/'ca.crt'
if cafile.exists():cafile.chmod(0o600)  # re-runs must be able to refresh the read-only copy
(runtime/'ca.crt').write_bytes((broker/'ca.crt').read_bytes());(runtime/'ca.crt').chmod(0o444)
def update(path,text):path.chmod(0o600);path.write_text(text);path.chmod(0o400)
passwords=broker/'passwords';current=passwords.read_text()
if username+':' not in current:
 hashdir=sim/'hash';hashdir.mkdir(exist_ok=True);p=hashdir/'password';p.write_text(username+':'+cfg['password']+'\n')
 subprocess.run(['docker','run','--rm','--user',str(os.getuid()),'--entrypoint','mosquitto_passwd','-v',str(hashdir)+':/config','eclipse-mosquitto:2.0.22','-U','/config/password'],check=True,stdout=subprocess.DEVNULL)
 update(passwords,current.rstrip()+'\n'+p.read_text());p.unlink();hashdir.rmdir()
acl=broker/'acl';current=acl.read_text()
if 'user '+username+'\n' not in current:update(acl,current.rstrip()+'\n\nuser '+username+'\ntopic write '+topic+'\n\nuser aether-ingest\ntopic read '+topic+'\n')
collector=sec/'mqtt/collector/collector.json';c=json.loads(collector.read_text())
if not any(b['topic']==topic for b in c['bindings']):
 c['bindings'].append({'topic':topic,'gateway_id':gid,'token':g['token']});collector.chmod(0o600);collector.write_text(json.dumps(c));collector.chmod(0o444)
# Zone B: a second virtual gateway that hears only the wearables, so roaming across gateways can be tried at home.
bfile=sim/'gateway-zone-b.json'
if not bfile.exists():
 gb=api('/api/v1/gateways',{'name':'SIM · Virtual MG3 · Zone B (wearables)','model':'minew-mg3'},login['access_token'])
 bfile.write_text(json.dumps(gb))
gb=json.loads(bfile.read_text());bid=gb['gateway']['id'];btopic='/aether/simulation/'+bid+'/status';buser='sim-'+bid
cfg=json.loads(config.read_text())
if 'zone_b' not in cfg:
 cfg['zone_b']={'username':buser,'password':secrets.token_urlsafe(32),'topic':btopic};config.chmod(0o600);config.write_text(json.dumps(cfg));config.chmod(0o444)
passwords=broker/'passwords';current=passwords.read_text()
if buser+':' not in current:
 hashdir=sim/'hash';hashdir.mkdir(exist_ok=True);p=hashdir/'password';p.write_text(buser+':'+cfg['zone_b']['password']+'\n')
 subprocess.run(['docker','run','--rm','--user',str(os.getuid()),'--entrypoint','mosquitto_passwd','-v',str(hashdir)+':/config','eclipse-mosquitto:2.0.22','-U','/config/password'],check=True,stdout=subprocess.DEVNULL)
 update(passwords,current.rstrip()+'\n'+p.read_text());p.unlink();hashdir.rmdir()
acl=broker/'acl';current=acl.read_text()
if 'user '+buser+'\n' not in current:update(acl,current.rstrip()+'\n\nuser '+buser+'\ntopic write '+btopic+'\n\nuser aether-ingest\ntopic read '+btopic+'\n')
c=json.loads(collector.read_text())
if not any(b['topic']==btopic for b in c['bindings']):
 c['bindings'].append({'topic':btopic,'gateway_id':bid,'token':gb['token']});collector.chmod(0o600);collector.write_text(json.dumps(c));collector.chmod(0o444)
print('Dedicated SIM gateway and broker credentials provisioned. Restart broker and collector, then start simulator.')
