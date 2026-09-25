#!/usr/bin/env python3
"""Prepare the local Mosquitto credential synchronizer without printing secrets."""
import pathlib,secrets,subprocess,os
root=pathlib.Path(__file__).resolve().parents[2];os.umask(0o077)
envfile=root/'.env';text=envfile.read_text();cfg=dict(x.split('=',1) for x in text.splitlines() if x and not x.startswith('#'))
key='MQTT_PROVISION_DB_PASSWORD'
if key not in cfg:
 cfg[key]=secrets.token_hex(32);envfile.write_text(text.rstrip()+'\n'+key+'='+cfg[key]+'\n');envfile.chmod(0o600)
password=cfg[key]
if len(password)!=64 or any(c not in '0123456789abcdef' for c in password):raise SystemExit('Unexpected provisioner credential format')
# Only the role password changes. No application/database superuser credential is logged.
sql="ALTER ROLE aether_mqtt_provisioner PASSWORD '"+password+"';\n"
subprocess.run(['docker','exec','-i','aether-postgres-1','psql','-U','postgres','-d','aether','-v','ON_ERROR_STOP=1'],input=sql.encode(),check=True,stdout=subprocess.DEVNULL)
broker=root/'.secrets/mqtt/broker';runtime=root/'.secrets/mqtt/runtime';runtime.mkdir(exist_ok=True,mode=0o700)
for name in ('passwords','acl'):
 p=runtime/name
 if not p.exists():p.write_bytes((broker/name).read_bytes());p.chmod(0o400)
p=broker/'mosquitto.conf';old=p.read_text();backup=root/'.secrets/mqtt/mosquitto-before-onboarding.conf'
if not backup.exists():backup.write_text(old);backup.chmod(0o600)
new=old.replace('password_file /mosquitto/config/passwords','password_file /mosquitto/runtime/passwords').replace('acl_file /mosquitto/config/acl','acl_file /mosquitto/runtime/acl')
if new!=old:p.chmod(0o600);p.write_text(new);p.chmod(0o400)
# mqtt-commander: its own broker account (the provisioner grants it write on aether/z2m/+/+/set only). Its line is
# kept in step with MQTT_COMMANDER_PASSWORD (added, or replaced when it no longer verifies); no other line is touched.
import json,sys
sys.path.insert(0,str(root/'infra/prod'))
from setup import sync_password_line
ckey='MQTT_COMMANDER_PASSWORD'
text=envfile.read_text()
if ckey not in cfg:
 cfg[ckey]=secrets.token_urlsafe(32);envfile.write_text(text.rstrip()+'\n'+ckey+'='+cfg[ckey]+'\n');envfile.chmod(0o600)
sync_password_line(broker/'passwords','aether-commander',cfg[ckey])
cdir=root/'.secrets/mqtt/commander';cdir.mkdir(exist_ok=True,mode=0o700)
ca=cdir/'ca.crt'
if ca.exists():ca.chmod(0o600)
ca.write_bytes((broker/'ca.crt').read_bytes());ca.chmod(0o444)
conf=cdir/'commander.json'
if conf.exists():conf.chmod(0o600)
conf.write_text(json.dumps({'broker_url':'ssl://mqtt:8883','username':'aether-commander','password':cfg[ckey],'client_id':'aether-commander-dev','ca_file':'/run/mqtt/ca.crt','bindings':[]}));conf.chmod(0o444)
print('Onboarding runtime prepared; legacy credentials preserved. Start with compose.onboarding.yaml.')
