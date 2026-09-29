# Developer-only: regenerates testdata/mq_vectors.json, the reference values mq_test.go checks the Go message-queue
# code against. Requires tuya-connector-python 0.1.2 (identical openpulsar.py to github.com/tuya/tuya-connector-python
# at 82487205) and pycryptodome:
#
#   python3 -m venv venv && ./venv/bin/pip install tuya-connector-python==0.1.2 pycryptodome websocket-client
#   ./venv/bin/python gen_mq_vectors.py > mq_vectors.json
#
# Fake credentials, fixed nonces; nothing leaves the machine. The password and the AES-ECB decryption are produced
# by the connector's own private methods (TuyaOpenPulsar.__gen_pwd / __decrypt_by_aes), so the Go code is checked
# against Tuya's reference implementation, not against a re-implementation. AES-GCM follows tuya-pulsar-sdk-go
# pkg/tyutils/aes.go (at 2506ea44): key = secret[8:24], data = 12-byte nonce || ciphertext || 16-byte tag.
import base64, hashlib, json
from Crypto.Cipher import AES
from Crypto.Util.Padding import pad
from tuya_connector.openpulsar import TuyaOpenPulsar

ACCESS_ID = "testaccessid00000000"
SECRET = "testsecret000000000000000000000a"
KEY = SECRET[8:24].encode()

p = TuyaOpenPulsar(ACCESS_ID, SECRET, "wss://mqe.tuyaus.com:8285/", "event")
password = p._TuyaOpenPulsar__gen_pwd()
url = p._TuyaOpenPulsar__get_topic_url()
decrypt_ecb = TuyaOpenPulsar._TuyaOpenPulsar__decrypt_by_aes

# Synthetic business messages shaped per Tuya "Message Types" (developer.tuya.com/en/docs/iot/message-type).
plain = {
    "status4": {"dataId": "AAXI3c1i6xxx0001", "devId": "bf1111111111111111aa01", "productKey": "awgmk9pixxx00001",
                "status": [{"code": "switch_1", "value": True, "t": 1790000000123, "1": "true"},
                           {"code": "cur_power", "value": 1234, "t": 1790000000123, "19": "1234"}]},
    "online20": {"devId": "bf1111111111111111aa01", "productKey": "awgmk9pixxx00001", "bizCode": "online",
                 "bizData": {"time": 1790000000}, "ts": 1790000000456},
}

ecb = {}
for name, msg in plain.items():
    text = json.dumps(msg, separators=(",", ":"))
    data = base64.b64encode(AES.new(KEY, AES.MODE_ECB).encrypt(pad(text.encode(), 16))).decode()
    assert decrypt_ecb(data, SECRET) == text  # the connector decrypts it back to the same text
    ecb[name] = {"plaintext": text, "data": data}

gcm = {}
for i, (name, msg) in enumerate(plain.items()):
    text = json.dumps(msg, separators=(",", ":"))
    nonce = bytes([0x10 + i]) * 12
    c = AES.new(KEY, AES.MODE_GCM, nonce=nonce)
    ct, tag = c.encrypt_and_digest(text.encode())
    gcm[name] = {"plaintext": text, "data": base64.b64encode(nonce + ct + tag).decode()}

print(json.dumps({
    "access_id": ACCESS_ID, "secret": SECRET,
    "password": password,
    "password_formula": "md5hex(access_id + md5hex(secret))[8:24]",
    "check_password": hashlib.md5((ACCESS_ID + hashlib.md5(SECRET.encode()).hexdigest()).encode()).hexdigest()[8:24],
    "consumer_url": url,
    "ecb": ecb, "gcm": gcm,
}, indent=1))
