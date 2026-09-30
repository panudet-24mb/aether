#!/usr/bin/env python3
"""Generate the frozen Tuya BLE test vectors for backend/internal/tuyable.

Developer-only. CI never runs this: the Go tests read the JSON it writes. Re-run it only when the protocol code is
being checked against a newer reference:

    python3 -m venv /tmp/tb && /tmp/tb/bin/pip install pycryptodome==3.23.0
    /tmp/tb/bin/python gen_vectors.py > vectors.json

Every random input is pinned (keys, srand, IV), so the output is byte-for-byte reproducible.

The pure functions below are copied from ha_tuya_ble (MIT):
  PlusPlus-ua/ha_tuya_ble   @ 6037ac5a04ceb23a36d1b88e2303aa1da7fdbe83
  ha-tuya-ble/ha_tuya_ble   @ 40899aeff5f1bcb63aca26fba4ee59b76e101681
    custom_components/tuya_ble/tuya_ble/{tuya_ble.py,security.py,const.py}

MIT License. Copyright (c) 2023 PlusPlus-ua, and other committers.
Permission is hereby granted, free of charge, to any person obtaining a copy of this software and associated
documentation files (the "Software"), to deal in the Software without restriction, including without limitation the
rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit
persons to whom the Software is furnished to do so, subject to the following conditions: The above copyright notice
and this permission notice shall be included in all copies or substantial portions of the Software.
THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND.

Only the bleak/Home Assistant plumbing is removed; the byte-level logic is unchanged. The one intended change is
that build_packets takes the IV (and the protocol nibble) as arguments instead of reading them from the device
object: the reference starts _protocol_version at 2, sets it from the advertisement's manufacturer data byte 1,
then from the device-info answer, and forces 2 for the FD50 device-info quirk.
"""
from __future__ import annotations
import hashlib
import json
from struct import pack, unpack

from Crypto.Cipher import AES

GATT_MTU = 20


# --- tuya_ble.py: TuyaBLEDevice._calc_crc16 -------------------------------------------------------------------------
def calc_crc16(data: bytes) -> int:
    crc = 0xFFFF
    for byte in data:
        crc ^= byte & 255
        for _ in range(8):
            tmp = crc & 1
            crc >>= 1
            if tmp != 0:
                crc ^= 0xA001
    return crc


# --- tuya_ble.py: _pack_int / _unpack_int ---------------------------------------------------------------------------
def pack_int(value: int) -> bytearray:
    result = bytearray()
    while True:
        curr_byte = value & 0x7F
        value >>= 7
        if value != 0:
            curr_byte |= 0x80
        result += pack(">B", curr_byte)
        if value == 0:
            break
    return result


def unpack_int(data: bytes, start_pos: int):
    result = 0
    offset = 0
    while offset < 5:
        pos = start_pos + offset
        if pos >= len(data):
            raise ValueError("format")
        curr_byte = data[pos]
        result |= (curr_byte & 0x7F) << (offset * 7)
        offset += 1
        if (curr_byte & 0x80) == 0:
            break
    if offset > 4:
        raise ValueError("format")
    return (result, start_pos + offset)


# --- security.py: TuyaBLESecurityMaterial ---------------------------------------------------------------------------
class Security:
    def __init__(self, local_key: str, sec_key: str | None = None):
        self.local_key = local_key
        self.sec_key = sec_key

    @property
    def protocol_v2(self):
        return bool(self.sec_key)

    @property
    def pairing_login_key(self) -> bytes:
        return self.local_key.encode("ascii")[:6]

    @property
    def _material(self) -> bytes:
        if self.protocol_v2:
            return f"{self.local_key}{self.sec_key}".encode("ascii")
        return self.pairing_login_key

    @property
    def login_key(self) -> bytes:
        return hashlib.md5(self._material).digest()

    def session_key(self, device_random: bytes) -> bytes:
        assert len(device_random) == 6
        return hashlib.md5(self._material + device_random).digest()

    @property
    def login_flag(self):
        return 14 if self.protocol_v2 else 4

    @property
    def session_flag(self):
        return 15 if self.protocol_v2 else 5


# --- tuya_ble.py: _build_packets (IV injected instead of secrets.token_bytes) ---------------------------------------
def build_packets(seq_num, code, data, response_to, key, flag, protocol_version, iv):
    raw = bytearray()
    raw += pack(">IIHH", seq_num, response_to, code, len(data))
    raw += data
    crc = calc_crc16(raw)
    raw += pack(">H", crc)
    while len(raw) % 16 != 0:
        raw += b"\x00"
    cipher = AES.new(key, AES.MODE_CBC, iv)
    encrypted = pack(">B", flag) + iv + cipher.encrypt(raw)
    command = []
    packet_num = 0
    pos = 0
    length = len(encrypted)
    while pos < length:
        packet = bytearray()
        packet += pack_int(packet_num)
        if packet_num == 0:
            packet += pack_int(length)
            packet += pack(">B", protocol_version << 4)
        data_part = encrypted[pos:pos + GATT_MTU - len(packet)]
        packet += data_part
        command.append(bytes(packet))
        pos += len(data_part)
        packet_num += 1
    return bytes(raw), command


# --- tuya_ble.py: _notification_handler + _parse_input (errors raised instead of logged) ----------------------------
def reassemble(fragments):
    buf = None
    expected_num = 0
    expected_len = 0
    for data in fragments:
        pos = 0
        packet_num, pos = unpack_int(data, pos)
        if packet_num != expected_num:
            raise ValueError("sequence")
        if packet_num == 0:
            buf = bytearray()
            expected_len, pos = unpack_int(data, pos)
            pos += 1
        buf += data[pos:]
        expected_num += 1
        if len(buf) > expected_len:
            raise ValueError("length")
        if len(buf) == expected_len:
            return bytes(buf)
    raise ValueError("incomplete")


def parse_input(buf, keys):
    flag = buf[0]
    key = keys[flag]
    iv = buf[1:17]
    raw = AES.new(key, AES.MODE_CBC, iv).decrypt(buf[17:])
    seq_num, response_to, code, length = unpack(">IIHH", raw[:12])
    end = length + 12
    if len(raw) < end:
        raise ValueError("length")
    if len(raw) > end:
        if calc_crc16(raw[:end]) != unpack(">H", raw[end:end + 2])[0]:
            raise ValueError("crc")
    return dict(flag=flag, seq=seq_num, response_to=response_to, code=code, data=raw[12:end].hex())


# --- tuya_ble.py: TuyaBLEDataPoint._get_value + _encode_datapoints / _parse_datapoints --------------------------------
def dp_value_bytes(t, v):
    if t in (0, 5):
        return bytes.fromhex(v)
    if t == 1:
        return pack(">B", 1 if v else 0)
    if t == 2:
        return pack(">i", v)
    if t == 4:
        if v > 0xFFFF:
            return pack(">I", v)
        if v > 0xFF:
            return pack(">H", v)
        return pack(">B", v)
    if t == 3:
        return v.encode()


def encode_dps(dps, length_size):
    data = bytearray()
    for (i, t, v) in dps:
        value = dp_value_bytes(t, v)
        if length_size == 1:
            data += pack(">BBB", i, t, len(value))
        else:
            data += pack(">BBH", i, t, len(value))
        data += value
    return bytes(data)


def parse_dps(data, start_pos, length_size):
    out = []
    pos = start_pos
    header = 2 + length_size
    while len(data) - pos >= header:
        i = data[pos]
        pos += 1
        t = data[pos]
        if t > 5:
            raise ValueError("format")
        pos += 1
        n = int.from_bytes(data[pos:pos + length_size], "big")
        pos += length_size
        nxt = pos + n
        if nxt > len(data):
            raise ValueError("length")
        raw = data[pos:nxt]
        if t in (0, 5):
            v = raw.hex()
        elif t == 1:
            v = int.from_bytes(raw, "big") != 0
        elif t in (2, 4):
            v = int.from_bytes(raw, "big", signed=True)
        else:
            v = raw.decode()
        out.append(dict(id=i, type=t, value=v))
        pos = nxt
    return out, pos


# --- tuya_ble.py: _decode_advertisement_data -----------------------------------------------------------------------
def decode_adv(service_data: bytes, manufacturer_data: bytes):
    raw_product_id = service_data[1:] if len(service_data) > 1 and service_data[0] == 0 else None
    res = {}
    if len(manufacturer_data) > 6:
        res["bound"] = (manufacturer_data[0] & 0x80) != 0
        res["protocol"] = manufacturer_data[1]
        raw_uuid = manufacturer_data[6:]
        if raw_product_id:
            key = hashlib.md5(raw_product_id).digest()
            res["uuid"] = AES.new(key, AES.MODE_CBC, key).decrypt(raw_uuid).decode("utf-8")
    return res


# --- tuya_ble.py: _build_pairing_request ---------------------------------------------------------------------------
def pairing_request(uuid, login6, device_id):
    r = bytearray()
    r += uuid.encode()
    r += login6
    r += device_id.encode()
    for _ in range(44 - len(r)):
        r += b"\x00"
    return bytes(r)


def main():
    v = {"source": {
        "PlusPlus-ua/ha_tuya_ble": "6037ac5a04ceb23a36d1b88e2303aa1da7fdbe83",
        "ha-tuya-ble/ha_tuya_ble": "40899aeff5f1bcb63aca26fba4ee59b76e101681"}}

    v["crc16"] = [dict(data=d.hex(), crc=calc_crc16(d)) for d in
                  [b"", b"\x00", b"123456789", bytes(range(32)), b"\xff" * 17]]
    v["varint"] = [dict(value=n, bytes=pack_int(n).hex()) for n in [0, 1, 127, 128, 255, 300, 16383, 16384, 2 ** 21, 2 ** 28 - 1]]

    srand = bytes.fromhex("a1b2c3d4e5f6")
    keys = []
    for lk, sk in [("0123456789abcdef", None), ("Zk8#pQ2!mN4$vB6&", None),
                   ("0123456789abcdef", "fedcba9876543210"), ("K3y!", None)]:
        if len(lk) < 6:
            continue
        s = Security(lk, sk)
        keys.append(dict(local_key=lk, sec_key=sk or "", pairing_login_key=s.pairing_login_key.hex(),
                         login_key=s.login_key.hex(), srand=srand.hex(), session_key=s.session_key(srand).hex(),
                         login_flag=s.login_flag, session_flag=s.session_flag))
    v["keys"] = keys

    s = Security("0123456789abcdef")
    login, session = s.login_key, s.session_key(srand)
    iv = bytes(range(16))
    frames = []
    cases = [
        ("device_info", 1, 0x0000, b"", 0, login, 4, 2),
        ("device_info_fd50", 1, 0x0000, b"\x00\xf3", 0, login, 4, 2),
        ("pair", 2, 0x0001, pairing_request("uuid-0000000000a1", s.pairing_login_key, "bf1234567890abcdef"), 0, session, 5, 3),
        ("status", 3, 0x0003, b"", 0, session, 5, 3),
        ("dps_v3", 4, 0x0002, encode_dps([(1, 1, True), (2, 2, 235)], 1), 0, session, 5, 3),
        ("dps_v4", 5, 0x0027, pack(">BI", 0, 5) + encode_dps([(1, 1, False), (3, 4, 2)], 2), 0, session, 5, 4),
        ("ack_receive_dp", 6, 0x8001, b"", 17, session, 5, 3),
        ("long_string", 7, 0x0002, encode_dps([(101, 3, "x" * 90)], 1), 0, session, 5, 3),
    ]
    # The first fragment carries the protocol from the advertisement before the device-info answer is known.
    cases.append(("device_info_advertised_v3", 1, 0x0000, b"", 0, login, 4, 3))
    cases.append(("device_info_advertised_v4", 1, 0x0000, b"", 0, login, 4, 4))
    sv2 = Security("0123456789abcdef", "fedcba9876543210")
    cases.append(("device_info_v2", 1, 0x0000, b"", 0, sv2.login_key, 14, 2))
    cases.append(("status_v2", 2, 0x0003, b"", 0, sv2.session_key(srand), 15, 4))
    for (name, seq, code, data, resp, key, flag, proto) in cases:
        raw, frags = build_packets(seq, code, data, resp, key, flag, proto, iv)
        buf = reassemble(frags)
        parsed = parse_input(buf, {flag: key})
        assert parsed["seq"] == seq and parsed["code"] == code and parsed["data"] == data.hex()
        frames.append(dict(name=name, seq=seq, response_to=resp, code=code, data=data.hex(), key=key.hex(), flag=flag,
                           protocol=proto, iv=iv.hex(), plaintext=raw.hex(), fragments=[f.hex() for f in frags]))
    v["frames"] = frames

    v["pairing_request"] = dict(uuid="uuid-0000000000a1", login6=s.pairing_login_key.hex(),
                                device_id="bf1234567890abcdef",
                                payload=pairing_request("uuid-0000000000a1", s.pairing_login_key, "bf1234567890abcdef").hex())

    dp_sets = [
        [(1, 1, True)],
        [(1, 1, False), (2, 2, 235), (3, 2, -40), (4, 4, 2), (5, 3, "hello"), (6, 0, "00ff10"), (7, 5, "0003")],
        [(8, 4, 300), (9, 4, 70000), (10, 2, 2147483647), (11, 2, -2147483648)],
        [(12, 3, "")],
    ]
    dps = []
    for ls in (1, 2):
        for sset in dp_sets:
            enc = encode_dps(sset, ls)
            parsed, end = parse_dps(enc, 0, ls)
            assert end == len(enc)
            dps.append(dict(length_size=ls, encoded=enc.hex(), decoded=parsed))
    v["dps"] = dps

    # Received-message layouts (FUN_RECEIVE_*), built from the parser's view of the payload.
    body3 = encode_dps([(1, 1, True), (2, 2, 215)], 1)
    body4 = encode_dps([(1, 1, True), (2, 2, 215)], 2)
    v["receive"] = [
        dict(code=0x8001, data=body3.hex(), dps_at=0, length_size=1, ack=""),
        dict(code=0x8003, data=(b"\x01" + pack(">I", 1767225600) + body3).hex(), dps_at=5, length_size=1, ack="",
             time=1767225600.0),
        dict(code=0x8003, data=(b"\x00" + b"1767225600123" + body3).hex(), dps_at=14, length_size=1, ack="",
             time=1767225600.123),
        dict(code=0x8006, data=(b"\x00" + pack(">I", 9) + b"\x00" + b"\x01" + body4).hex(), dps_at=7, length_size=2,
             ack=(b"\x00" + pack(">I", 9) + b"\x00" + b"\x01" + b"\x00").hex()),
        dict(code=0x8006, data=(b"\x00" + pack(">I", 10) + b"\x80" + b"\x01" + body4).hex(), dps_at=7, length_size=2,
             ack=None),
        dict(code=0x8007, data=(b"\x00" + pack(">I", 11) + b"\x00" + b"\x00" + b"\x01" + pack(">I", 1767225600) + body4).hex(),
             dps_at=12, length_size=2, ack=(b"\x00" + pack(">I", 11) + b"\x00\x00\x00").hex(), time=1767225600.0),
    ]
    for r in v["receive"]:
        parsed, end = parse_dps(bytes.fromhex(r["data"]), r["dps_at"], r["length_size"])
        r["decoded"] = parsed

    # FUN_RECEIVE_TIME1_REQ / TIME2_REQ answers for a fixed clock: 2026-01-01 07:00:00 +07:00 (UTC 00:00), Thursday.
    ms = 1767225600000
    tz = 700  # -int(time.timezone/36) for UTC+7 (time.timezone = -25200)
    v["time"] = dict(unix_ms=ms, tz_hundredths=tz,
                     time1=(str(ms).encode() + pack(">h", tz)).hex(),
                     time2=pack(">BBBBBBBh", 26, 1, 1, 7, 0, 0, 3, tz).hex(),
                     note="time2 weekday is Python tm_wday (Monday=0); 2026-01-01 is a Thursday")

    # Device info response (46 bytes): dev ver, proto ver, flags, bound, srand, hw ver, auth key.
    info = bytes([1, 2, 3, 0, 0x05, 1]) + srand + bytes([4, 1]) + bytes(range(0x40, 0x60))
    v["device_info"] = dict(data=info.hex(), device_version="1.2", protocol_version="3.0", protocol=3, flags=5,
                            bound=True, srand=srand.hex(), hardware_version="4.1", auth_key=bytes(range(0x40, 0x60)).hex())

    # Advertisement: service data A201 = 0x00 + product id; manufacturer 0x07D0 = flags, proto, 4 bytes, enc(uuid).
    advs = []
    for pid, uuid, bound, proto in [(b"gvygg3m8", "tuya5d8f2a3c9e1b", True, 3), (b"abcdefghijklmnop", "0123456789abcdef", False, 4)]:
        key = hashlib.md5(pid).digest()
        enc = AES.new(key, AES.MODE_CBC, key).encrypt(uuid.encode())
        mfr = bytes([0x80 if bound else 0x00, proto, 0, 0, 0, 0]) + enc
        sd = b"\x00" + pid
        dec = decode_adv(sd, mfr)
        assert dec["uuid"] == uuid
        advs.append(dict(service_data=sd.hex(), manufacturer_data=mfr.hex(), product_id=pid.decode(), uuid=uuid,
                         bound=bound, protocol=proto))
    v["adverts"] = advs

    print(json.dumps(v, indent=1))


if __name__ == "__main__":
    main()
