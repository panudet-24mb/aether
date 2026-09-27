# Developer-only: regenerates the reference signatures in client_test.go with tinytuya 1.20.0 (pip install tinytuya==1.20.0).
# Frozen clock and fake credentials; requests.get is captured, nothing leaves the machine.
import json, time, tinytuya, requests
from tinytuya import Cloud as C
cap=[]
class R:
    def __init__(s, d): s.text=json.dumps(d); s.content=s.text.encode(); s.status_code=200
    def json(s): return json.loads(s.text)
def fake_get(url, headers=None, **kw):
    cap.append((url, dict(headers)))
    if '/token' in url:
        return R({"success":True,"result":{"access_token":"tok_fixed_000000000000000000000","expire_time":7200,"uid":"ay_uid_fixed"},"t":1})
    return R({"success":True,"result":{"devices":[],"has_more":False}})
requests.get=fake_get
time.time=lambda: 1790000000.123
c=C(apiRegion="us", apiKey="testaccessid0000000", apiSecret="testsecret000000000000000000000a", initial_token=None)
c._tuyaplatform('iot-01/associated-users/devices', query={'size':50,'last_row_key':'abc'})
out=[{"url":u,"sign":h.get("sign"),"t":h.get("t"),"access_token":h.get("access_token","")} for u,h in cap]
print(json.dumps(out, indent=1))
