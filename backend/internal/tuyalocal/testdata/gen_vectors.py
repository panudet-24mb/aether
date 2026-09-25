#!/usr/bin/env python3
"""Generate the frozen Tuya protocol test vectors for backend/internal/tuyalocal.

Developer-only. CI never runs this: the Go tests read the JSON it writes. Re-run it only when the protocol
code is being checked against a newer tinytuya:

    python3 -m venv /tmp/tt && /tmp/tt/bin/pip install tinytuya==1.20.0
    /tmp/tt/bin/python gen_vectors.py

Every random input is pinned (local key, device id, clock, negotiation nonces, GCM IVs) so the frames are
byte-for-byte reproducible. Client frames come from tinytuya's own encoder (XenonDevice._encode_message);
device frames (negotiation answers, status pushes, discovery broadcasts) are built with tinytuya's
pack_message and then checked by decoding them with tinytuya itself.
"""
import binascii
import hmac
import json
import logging
import os
import struct
import sys
import time
from hashlib import sha256

import tinytuya
from tinytuya.core import command_types as CT
from tinytuya.core import header as H
import importlib
from tinytuya.core.crypto_helper import AESCipher
from tinytuya.core.message_helper import TuyaMessage, pack_message, unpack_message
from tinytuya.core.udp_helper import decrypt_udp, udpkey

# With DEBUG enabled tinytuya uses the fixed GCM IV b'0123456789ab' (crypto_helper.get_encryption_iv).
logging.getLogger("tinytuya").setLevel(logging.DEBUG)
logging.getLogger("tinytuya").addHandler(logging.NullHandler())
logging.getLogger("tinytuya").propagate = False

NOW = 1700000000
time.time = lambda: NOW  # generate_payload() stamps "t" with int(time.time())
LOCAL_NONCE = b"0123456789abcdef"
REMOTE_NONCE = b"fedcba9876543210"
CLIENT_IV = b"0123456789ab"
DEVICE_IV = b"abcdefghijkl"
XD = importlib.import_module("tinytuya.core.XenonDevice")
XD.os.urandom = lambda n: LOCAL_NONCE[:n] if n == 16 else bytes(n)

DEV_ID = "eb1234567890abcdefgh"
KEY = "0123456789abcdef"
DPS = {"1": True, "2": 50}
PUSH_DPS = {"1": False, "18": 1234}

hx = lambda b: binascii.hexlify(b).decode()


def device_frame(version, seq, cmd, body, key):
    """A frame as the device sends it: return code 0, then the version rules for the payload."""
    if version == 3.5:
        msg = TuyaMessage(seq, cmd, 0, body, 0, True, H.PREFIX_6699_VALUE, DEVICE_IV)
        return pack_message(msg, hmac_key=key)
    if version == 3.4:
        ct = AESCipher(key).encrypt(body, False)
        msg = TuyaMessage(seq, cmd, 0, struct.pack(">I", 0) + ct, 0, True, H.PREFIX_55AA_VALUE, None)
        return pack_message(msg, hmac_key=key)
    msg = TuyaMessage(seq, cmd, 0, struct.pack(">I", 0) + body, 0, True, H.PREFIX_55AA_VALUE, None)
    return pack_message(msg)


def client_frame(d, cmd, data=None):
    seq = d.seqno
    mp = d.generate_payload(cmd, data)
    frame = d._encode_message(mp)
    return {"seq": seq, "cmd": mp.cmd, "payload": mp.payload.decode(), "frame": hx(frame)}


def decode_device(d, frame, key):
    msg = unpack_message(frame, hmac_key=key if d.version >= 3.4 else None, no_retcode=False)
    assert msg.crc_good, "tinytuya rejected the device frame"
    return d._decode_payload(msg.payload)


def vectors(version):
    d = tinytuya.Device(DEV_ID, address="127.0.0.1", local_key=KEY, version=version)
    out = {"tinytuya": tinytuya.__version__, "version": str(version), "device_id": DEV_ID, "local_key": KEY,
           "now": NOW, "client_iv": CLIENT_IV.decode(), "device_iv": DEVICE_IV.decode()}
    real = KEY.encode()

    if version >= 3.4:
        start = d._negotiate_session_key_generate_step_1()
        seq = d.seqno
        out["negotiation"] = {"local_nonce": LOCAL_NONCE.decode(), "remote_nonce": REMOTE_NONCE.decode(),
                              "start": {"seq": seq, "frame": hx(d._encode_message(start))}}
        resp_body = REMOTE_NONCE + hmac.new(real, LOCAL_NONCE, sha256).digest()
        resp = device_frame(version, 1, CT.SESS_KEY_NEG_RESP, resp_body, real)
        out["negotiation"]["resp"] = {"seq": 1, "frame": hx(resp)}
        rkey = unpack_message(resp, hmac_key=real, no_retcode=False)
        finish = d._negotiate_session_key_generate_step_3(rkey)
        assert finish, "tinytuya refused our negotiation answer"
        seq = d.seqno
        out["negotiation"]["finish"] = {"seq": seq, "frame": hx(d._encode_message(finish))}
        d._negotiate_session_key_generate_finalize()
        out["negotiation"]["session_key"] = hx(d.local_key)

    key = d.local_key
    out["dp_query"] = client_frame(d, CT.DP_QUERY)
    out["control"] = client_frame(d, CT.CONTROL, dict(DPS))
    out["heartbeat"] = client_frame(d, CT.HEART_BEAT)

    # A status push from the device, decoded by tinytuya to prove the construction.
    if version >= 3.4:
        push_json = json.dumps({"protocol": 4, "t": NOW, "data": {"dps": PUSH_DPS}}, separators=(",", ":"))
        body = H.PROTOCOL_34_HEADER if version == 3.4 else H.PROTOCOL_35_HEADER
        body += push_json.encode()
    else:
        push_json = json.dumps({"devId": DEV_ID, "dps": PUSH_DPS, "t": NOW}, separators=(",", ":"))
        body = H.PROTOCOL_33_HEADER + AESCipher(key).encrypt(push_json.encode(), False)
    push = device_frame(version, 7, CT.STATUS, body, key)
    decoded = decode_device(d, push, key)
    assert decoded["dps"] == PUSH_DPS, decoded
    out["status_push"] = {"seq": 7, "cmd": CT.STATUS, "json": push_json, "dps": PUSH_DPS, "frame": hx(push)}

    # The answer to a DP query.
    if version >= 3.4:
        reply_json = json.dumps({"dps": DPS, "t": NOW}, separators=(",", ":"))
        reply = device_frame(version, 8, CT.DP_QUERY_NEW, reply_json.encode(), key)
    else:
        reply_json = json.dumps({"devId": DEV_ID, "dps": DPS}, separators=(",", ":"))
        reply = device_frame(version, 8, CT.DP_QUERY, AESCipher(key).encrypt(reply_json.encode(), False), key)
    decoded = decode_device(d, reply, key)
    assert decoded["dps"] == DPS, decoded
    out["query_reply"] = {"seq": 8, "json": reply_json, "dps": DPS, "frame": hx(reply)}

    # A discovery broadcast in this version's format.
    body = {"ip": "192.168.1.50", "gwId": DEV_ID, "active": 2, "ability": 0, "mode": 0, "encrypt": True,
            "productKey": "keyabcdefghijklm", "version": str(version)}
    js = json.dumps(body, separators=(",", ":")).encode()
    if version == 3.5:
        bc = pack_message(TuyaMessage(0, CT.UDP_NEW, 0, js, 0, True, H.PREFIX_6699_VALUE, DEVICE_IV), hmac_key=udpkey)
    else:
        bc = pack_message(TuyaMessage(0, CT.UDP_NEW, 0, struct.pack(">I", 0) + AESCipher(udpkey).encrypt(js, False),
                                      0, True, H.PREFIX_55AA_VALUE, None))
    assert json.loads(decrypt_udp(bc)) == body
    out["broadcast"] = {"json": body, "frame": hx(bc)}
    return out


def vectors31():
    d = tinytuya.Device(DEV_ID, address="127.0.0.1", local_key=KEY, version=3.1)
    out = {"tinytuya": tinytuya.__version__, "version": "3.1", "device_id": DEV_ID, "local_key": KEY, "now": NOW}
    out["dp_query"] = client_frame(d, CT.DP_QUERY)
    out["control"] = client_frame(d, CT.CONTROL, dict(DPS))
    body = {"ip": "192.168.1.51", "gwId": DEV_ID, "active": 2, "ability": 0, "mode": 0, "encrypt": False,
            "productKey": "keyabcdefghijklm", "version": "3.1"}
    js = json.dumps(body, separators=(",", ":")).encode()
    bc = pack_message(TuyaMessage(0, CT.UDP_NEW, 0, struct.pack(">I", 0) + js, 0, True, H.PREFIX_55AA_VALUE, None))
    assert json.loads(decrypt_udp(bc)) == body
    out["broadcast"] = {"json": body, "frame": hx(bc)}
    return out


if __name__ == "__main__":
    here = os.path.dirname(os.path.abspath(__file__))
    for name, v in (("31", None), ("33", 3.3), ("34", 3.4), ("35", 3.5)):
        data = vectors31() if v is None else vectors(v)
        with open(os.path.join(here, "vectors_%s.json" % name), "w") as f:
            json.dump(data, f, indent=2, sort_keys=True)
            f.write("\n")
        print("wrote vectors_%s.json" % name, file=sys.stderr)
