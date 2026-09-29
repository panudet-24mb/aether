# Developer-only: regenerates the POST (property issue) reference signature in client_test.go with tinytuya 1.20.0
# (pip install tinytuya==1.20.0). Frozen clock and fake credentials; requests are captured, nothing leaves the machine.
# content_type="" makes tinytuya sign no headers (no Signature-Headers), which is how Aether signs: Tuya verifies only
# the headers a request lists in Signature-Headers, so the Content-Type Aether sends is not part of the signature.
import json, time, requests
from tinytuya import Cloud as C
cap=[]
class R:
    def __init__(s,d): s.text=json.dumps(d); s.content=s.text.encode(); s.status_code=200
    def json(s): return json.loads(s.text)
def fake(url, headers=None, data=None, **kw):
    cap.append((url, dict(headers or {}), data))
    if '/token' in url:
        return R({"success":True,"result":{"access_token":"tok_fixed_000000000000000000000","expire_time":7200,"uid":"ay_uid_fixed"}})
    return R({"success":True,"result":True})
requests.get=fake; requests.post=fake
def freq(method, url, headers=None, data=None, **kw): return fake(url, headers=headers, data=data)
requests.request=freq
time.time=lambda: 1790000000.123
c=C(apiRegion="us", apiKey="testaccessid0000000", apiSecret="testsecret000000000000000000000a", initial_token=None)
body={"properties": json.dumps({"switch_1": True}, separators=(",",":"))}
c._tuyaplatform('cloud/thing/bf1111111111111111aa01/shadow/properties/issue', action='POST', post=body, ver='v2.0', content_type='')
for u,h,d in cap:
    print(json.dumps({"url":u,"sign":h.get("sign"),"t":h.get("t"),"token":h.get("access_token",""),"body":d if isinstance(d,str) else (d.decode() if d else None)}))
