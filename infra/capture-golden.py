#!/usr/bin/env python3
"""Turn what an MG3 gateway really heard into a permanent Go regression test.

Reads the authenticated raw-packet endpoint (GET /api/v1/gateways/{id}/packets, owner/admin only,
the latest 20 of the last 100 captured packets), extracts the JSON-LONG rows of one tag and writes
backend/internal/adapters/minew/testdata/real/<label>.json, which TestRealCaptures decodes.

Operator-facing text is Thai; code and comments are English. Credentials and tokens are never printed.
Standard library only.
"""
import argparse
import hashlib
import json
import os
import pathlib
import ssl
import sys
import time
import urllib.error
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[1]
TESTDATA = ROOT / "backend/internal/adapters/minew/testdata/real"

# Minew FFE1 0xA1 payload lengths, counted from the 0xa1 byte (see backend/.../minew/frames.go).
MINEW_MIN_LEN = {0x01: 13, 0x02: 10, 0x03: 15, 0x05: 11, 0x08: 9, 0x11: 11, 0x12: 11, 0x13: 11, 0x18: 14, 0x20: 10, 0x21: 10}

SCENARIOS = {
    "s1": [("idle", "วาง S1 นิ่ง ๆ ข้าง gateway อย่าจับ อย่าหายใจรด", 60)],
    "c10": [
        ("idle", "วางบัตร C10 นิ่ง ๆ บนโต๊ะ ห้ามขยับ", 30),
        ("press", "กดปุ่มบนบัตร C10 ค้างไว้ 3 วินาที แล้วถือนิ่ง", 20),
        ("release", "ปล่อยปุ่ม แล้ววางบัตรนิ่ง ๆ", 20),
    ],
    "b7": [
        ("idle", "วางสายรัดข้อมือ B7 นิ่ง ๆ", 30),
        ("press", "กดปุ่มบน B7 ค้างไว้ 3 วินาที", 20),
        ("release", "ปล่อยปุ่ม แล้ววาง B7 นิ่ง ๆ", 20),
    ],
    "b10": [
        ("idle", "ปล่อยนิ่ง 20 วินาที อย่าแตะปุ่ม", 20),
        ("press", "กดปุ่มค้าง 3 วินาที", 20),
        ("release", "ปล่อย แล้ววางนิ่ง", 20),
    ],
    "e8s": [
        ("still", "วาง E8S นิ่ง ๆ บนโต๊ะ 30 วินาที", 30),
        ("shake", "เขย่า E8S เบา ๆ ต่อเนื่องจนครบเวลา", 30),
    ],
    "mbt01": [
        ("attached", "ติด MBT01 กับพื้นผิวให้สนิท แล้วปล่อยนิ่ง", 30),
        ("removed", "ดึง MBT01 ออกจากพื้นผิว แล้วปล่อยนิ่ง", 30),
        ("reattached", "ติด MBT01 กลับเข้าที่เดิม แล้วปล่อยนิ่ง", 30),
    ],
    # MOS smart-office kit. S4: the combination-frame layout is not public; these captures are what a
    # decoder (and its golden test) will be written from, after "สอนสัญญาณ" has shown which byte moves.
    "s4": [
        ("closed", "ประกบ S4 กับแม่เหล็กให้สนิท (ประตูปิด) แล้วปล่อยนิ่ง", 30),
        ("open", "แยกแม่เหล็กออกจากตัว S4 (ประตูเปิด) แล้วค้างไว้", 30),
        ("closed-again", "ประกบกลับ (ประตูปิด) แล้วปล่อยนิ่ง", 30),
        ("tamper", "ถ้ามีปุ่ม/ฝากันงัด ให้เปิดฝาหลังค้างไว้ (ถ้าไม่มี ปล่อยนิ่ง)", 30),
    ],
    # MSP01: the real unit reported motion=0 in all 257 samples, so the PIR must be tested by walking past.
    "msp01": [
        ("empty", "ออกจากระยะมองของ MSP01 ให้หมด ห้องต้องไม่มีคนเคลื่อนไหว", 60),
        ("walk", "เดินผ่านหน้า MSP01 ไป-กลับ ห่างประมาณ 2 เมตร ต่อเนื่องจนครบเวลา", 30),
        ("empty-again", "ออกจากระยะมองอีกครั้ง แล้วรอ", 60),
    ],
}


def say(msg):
    print(msg, flush=True)


def normalise_mac(mac):
    m = mac.replace(":", "").replace("-", "").strip().lower()
    if len(m) != 12 or any(c not in "0123456789abcdef" for c in m):
        raise SystemExit("MAC ไม่ถูกต้อง: ต้องเป็น 12 hex เช่น AA:BB:CC:DD:EE:FF")
    return m


def little_endian(mac):
    """Render a big-endian MAC string the way Minew frames carry it (reversed byte order)."""
    b = bytes.fromhex(mac)
    return b[::-1].hex()


class API:
    """Owner-authenticated client. Mirrors infra/mqtt/provision-simulator.py."""

    def __init__(self, base, insecure):
        env = {}
        envfile = ROOT / ".env"
        if envfile.exists():
            env = dict(x.split("=", 1) for x in envfile.read_text().splitlines() if x and not x.startswith("#") and "=" in x)
        self.origin = env.get("APP_ORIGIN", "http://localhost:3001")
        self.base = (base or "http://127.0.0.1:" + env.get("API_PORT", "8080")).rstrip("/")
        ctx = None
        if self.base.startswith("https://") and insecure:
            # A production box behind Caddy's internal CA: the operator opted out of verification.
            ctx = ssl.create_default_context()
            ctx.check_hostname = False
            ctx.verify_mode = ssl.CERT_NONE
            say("คำเตือน: --insecure ปิดการตรวจใบรับรอง ใช้เฉพาะกับเครื่องที่คุณควบคุมเอง")
        handlers = [urllib.request.ProxyHandler({})]
        if ctx is not None:
            handlers.append(urllib.request.HTTPSHandler(context=ctx))
        self.opener = urllib.request.build_opener(*handlers)
        self.token = None

    def call(self, path, data=None):
        headers = {"Content-Type": "application/json", "Origin": self.origin}
        if self.token:
            headers["Authorization"] = "Bearer " + self.token
        body = json.dumps(data).encode() if data is not None else None
        req = urllib.request.Request(self.base + path, data=body, headers=headers)
        try:
            with self.opener.open(req, timeout=20) as r:
                return json.load(r)
        except urllib.error.HTTPError as e:
            raise SystemExit("เรียก API ไม่สำเร็จ %s -> HTTP %s" % (path, e.code))
        except urllib.error.URLError as e:
            raise SystemExit("ต่อ API ไม่ได้ (%s): %s" % (self.base, e.reason))

    def login(self):
        owner = ROOT / ".secrets/owner.json"
        if not owner.exists():
            raise SystemExit("ไม่พบ .secrets/owner.json — ต้องรัน infra provisioning ก่อน")
        out = self.call("/api/v1/auth/login", json.loads(owner.read_text()))
        self.token = out["access_token"]  # never printed

    def gateways(self):
        return self.call("/api/v1/gateways").get("items", [])

    def packets(self, gateway_id):
        return self.call("/api/v1/gateways/%s/packets" % gateway_id).get("items", [])

    def live(self):
        return self.call("/api/v1/live?range=1h").get("gateways", [])


def rows_of(packets):
    """Yield (received_at, row) for every JSON-LONG row in the stored packets."""
    for packet in packets:
        payload = packet.get("payload")
        if isinstance(payload, str):
            try:
                payload = json.loads(payload)
            except ValueError:
                continue
        if not isinstance(payload, list):
            continue
        for row in payload:
            # Row by row, like the backend: an MG4 row with an odd field (ISO timestamp, numeric mac) skips only itself.
            if isinstance(row, dict) and row.get("type") != "Gateway" and isinstance(row.get("mac"), str) and row.get("mac"):
                yield packet.get("received_at", ""), row


def ad_structures(raw):
    """Walk an advertisement exactly as the Go decoder does: length byte, bounds, zero terminator."""
    try:
        data = bytes.fromhex(raw)
    except ValueError:
        return
    i = 0
    while i < len(data):
        n = data[i]
        if n == 0 or i + 1 + n > len(data):
            return
        yield data[i + 1 : i + 1 + n]
        i += n + 1


def frame_mac(raw):
    """The little-endian MAC a Minew FFE1 frame carries, or None when the advertisement has none."""
    for ad in ad_structures(raw):
        if len(ad) < 5 or ad[0] != 0x16 or ad[1] != 0xE1 or ad[2] != 0xFF or ad[3] != 0xA1:
            continue
        p = ad[3:]
        need = MINEW_MIN_LEN.get(p[1])
        if not need or len(p) < need:
            continue
        return (p[3:9] if p[1] == 0x08 else p[-6:]).hex()
    return None


def pseudonym(mac):
    """Stable pseudonymous address for one real MAC: locally administered, unicast, not reversible."""
    digest = bytearray(hashlib.sha256(b"aether-golden-anon" + bytes.fromhex(mac)).digest()[:6])
    digest[0] = (digest[0] | 0x02) & 0xFE
    return bytes(digest).hex()


def anonymize(rows, mac):
    """Rewrite the MAC in the row field AND inside the frame bytes, so the file still decodes.

    Both the big-endian form (as printed) and the little-endian form (as Minew frames carry it) are
    replaced with the same pseudonym, which keeps the trailing-MAC cross-check in the decoder happy.
    """
    fake = pseudonym(mac)
    before = [frame_mac(r["rawData"]) for r in rows]
    for row in rows:
        raw = row["rawData"].lower()
        raw = raw.replace(little_endian(mac), little_endian(fake)).replace(mac, fake)
        row["rawData"] = raw
        row["mac"] = fake
    after = [frame_mac(r["rawData"]) for r in rows]
    le_old, le_new = little_endian(mac), little_endian(fake)
    for old, new in zip(before, after):
        if old == le_old and new != le_new:
            raise SystemExit("anonymise ผิดพลาด: MAC ในเฟรมไม่ถูกแทนที่อย่างสม่ำเสมอ ยกเลิกการบันทึก")
    return fake


def cmd_list(api, args):
    live = {}
    try:
        for view in api.live():
            gid = (view.get("gateway") or {}).get("id")
            for sensor in view.get("sensors") or []:
                latest = sensor.get("latest") or {}
                live[(gid, sensor.get("id"))] = (latest.get("frames") or [], latest.get("unknown") or [])
    except SystemExit:
        say("อ่าน /api/v1/live ไม่ได้ จะแสดงเฉพาะข้อมูลจาก raw packets")
    gateways = api.gateways()
    if not gateways:
        say("ยังไม่มี gateway ในบัญชีนี้")
        return
    for g in gateways:
        say("")
        say("Gateway %s · %s · model=%s" % (g.get("id"), g.get("name"), g.get("model")))
        seen = {}
        for _, row in rows_of(api.packets(g.get("id"))):
            try:
                mac = normalise_mac(str(row["mac"]))
            except SystemExit:
                continue  # a row whose address is not a 6-byte MAC is not a BLE tag we can capture
            slot = seen.setdefault(mac, {"count": 0, "rssi": [], "raws": set()})
            slot["count"] += 1
            if isinstance(row.get("rssi"), int):
                slot["rssi"].append(row["rssi"])
            if row.get("rawData"):
                slot["raws"].add(row["rawData"].lower())
        if not seen:
            say("  ยังไม่มี raw packet ล่าสุดของ gateway นี้")
            continue
        say("  %-14s %6s %14s  %s" % ("MAC", "rows", "RSSI", "Aether อ่านได้ / ไม่เข้าใจ"))
        for mac, slot in sorted(seen.items(), key=lambda kv: -kv[1]["count"]):
            rssi = "n/a"
            if slot["rssi"]:
                rssi = "%d..%d dBm" % (min(slot["rssi"]), max(slot["rssi"]))
            frames, unknown = live.get((g.get("id"), mac), ([], []))
            note = ", ".join(frames) if frames else "ยังไม่มีผลถอดรหัส"
            if unknown:
                note += "  | ไม่เข้าใจ: " + ", ".join(unknown)
            say("  %-14s %6d %14s  %s" % (mac, slot["count"], rssi, note))
        say("  (ตัวเลือก: python3 infra/capture-golden.py capture --gateway %s --mac <MAC> --label <ชื่อ>)" % g.get("id"))


def countdown(seconds, prompt):
    say("")
    say("▶ " + prompt)
    for remaining in range(seconds, 0, -1):
        sys.stdout.write("\r   เหลือ %2d วินาที " % remaining)
        sys.stdout.flush()
        time.sleep(1)
    sys.stdout.write("\r   ครบเวลา          \n")
    sys.stdout.flush()


def collect(api, gateway, mac, seconds):
    """Poll the raw-packet endpoint for `seconds` and return de-duplicated rows of one MAC."""
    rows, seen = [], set()
    deadline = time.time() + seconds
    while True:
        for received_at, row in rows_of(api.packets(gateway)):
            raw = str(row.get("rawData", "")).lower()
            if not raw or normalise_mac(str(row["mac"])) != mac or raw in seen:
                continue
            seen.add(raw)
            rows.append({"mac": mac, "rawData": raw, "rssi": row.get("rssi"), "timestamp": row.get("timestamp") or received_at})
        remaining = deadline - time.time()
        if remaining <= 0:
            break
        sys.stdout.write("\r   เก็บได้ %d เฟรมไม่ซ้ำ · เหลือ %2d วินาที " % (len(rows), int(remaining) + 1))
        sys.stdout.flush()
        time.sleep(min(3, max(0.5, remaining)))
    sys.stdout.write("\r   เก็บได้ %d เฟรมไม่ซ้ำ                 \n" % len(rows))
    return rows


def write_capture(rows, mac, label, gateway_model, note, anonymise):
    if not rows:
        say("ไม่ได้เฟรมเลย: ตรวจว่า tag เปิดอยู่ อยู่ใกล้ gateway และ scan filter ไม่ได้กรองมันทิ้ง")
        return None
    stored_mac = mac
    mismatched = sum(1 for r in rows if frame_mac(r["rawData"]) not in (None, little_endian(mac)))
    if mismatched:
        say("หมายเหตุ: %d เฟรมมี MAC ภายในไม่ตรงกับ MAC ที่ gateway รายงาน (บันทึกไว้ให้ตรวจ)" % mismatched)
    if anonymise:
        stored_mac = anonymize(rows, mac)
        say("แทนที่ MAC ด้วยนามแฝงแล้ว (ทั้งในช่อง mac และในไบต์ของเฟรม) ไฟล์ยังถอดรหัสได้เหมือนเดิม")
    TESTDATA.mkdir(parents=True, exist_ok=True)
    path = TESTDATA / (label + ".json")
    doc = {
        "label": label,
        "captured_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "gateway_model": gateway_model,
        "note": note,
        "anonymized": bool(anonymise),
        "mac": stored_mac,
        "rows": rows,
    }
    path.write_text(json.dumps(doc, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    say("บันทึก %s (%d เฟรม)" % (path.relative_to(ROOT), len(rows)))
    say("ขั้นต่อไป: cd backend && go test ./internal/adapters/minew/ -run TestRealCaptures -v")
    say("แล้วคัดลอกบล็อก expect ที่เทสต์แนะนำมาใส่ไฟล์นี้ หลังตรวจด้วยตาว่าค่าตรงกับที่อุปกรณ์แสดง")
    return path


def cmd_capture(api, args):
    mac = normalise_mac(args.mac)
    gateway_model = "unknown"
    for g in api.gateways():
        if g.get("id") == args.gateway:
            gateway_model = g.get("model") or "unknown"
            break
    else:
        raise SystemExit("ไม่พบ gateway id นี้ในบัญชี ลองรัน: capture-golden.py list")
    steps = SCENARIOS.get(args.scenario) if args.scenario else None
    if steps is None:
        if not args.label:
            raise SystemExit("ต้องระบุ --label หรือ --scenario")
        steps = [(None, "เริ่มเก็บแพ็กเก็ตของ %s" % mac, args.seconds)]
    else:
        say("สถานการณ์ %s: %d ขั้นตอน ทำตามคำสั่งทีละข้อ" % (args.scenario, len(steps)))
    for name, prompt, seconds in steps:
        label = args.label or args.scenario
        if name:
            label = "%s-%s" % (args.label or args.scenario, name)
            countdown(3, prompt + " (เริ่มใน 3 วินาที)")
        say("กำลังเก็บ %d วินาที..." % seconds)
        rows = collect(api, args.gateway, mac, seconds)
        write_capture(rows, mac, label, gateway_model, args.note, not args.no_anonymize)


def main():
    ap = argparse.ArgumentParser(description="เก็บ raw BLE frame จริงจาก MG3 มาเป็น golden test")
    ap.add_argument("--api", help="ฐาน URL ของ API เช่น https://aether.example (ค่าเริ่มต้น: 127.0.0.1 ตาม .env)")
    ap.add_argument("--insecure", action="store_true", help="ไม่ตรวจใบรับรอง TLS (เครื่อง production ที่ใช้ internal CA ของ Caddy)")
    sub = ap.add_subparsers(dest="cmd", required=True)

    lst = sub.add_parser("list", help="แสดง gateway และ MAC ที่เพิ่งได้ยิน พร้อมผลถอดรหัสของ Aether")
    lst.set_defaults(func=cmd_list)

    cap = sub.add_parser("capture", help="เก็บเฟรมของอุปกรณ์หนึ่งตัวเป็นไฟล์ golden")
    cap.add_argument("--gateway", required=True, help="gateway id (ดูจากคำสั่ง list)")
    cap.add_argument("--mac", required=True, help="MAC ของ tag เช่น AA:BB:CC:DD:EE:FF")
    cap.add_argument("--label", help="ชื่อไฟล์ เช่น c10-idle")
    cap.add_argument("--seconds", type=int, default=60, help="ระยะเวลาเก็บต่อขั้น (ค่าเริ่มต้น 60)")
    cap.add_argument("--note", default="", help="บันทึกอิสระ เช่น firmware ของ gateway/tag")
    cap.add_argument("--scenario", choices=sorted(SCENARIOS), help="เดินตามขั้นตอนของอุปกรณ์ แล้วบันทึกไฟล์ละขั้น")
    cap.add_argument("--no-anonymize", action="store_true", help="เก็บ MAC จริง (ค่าเริ่มต้นคือแทนที่ด้วยนามแฝง)")
    cap.set_defaults(func=cmd_capture)

    args = ap.parse_args()
    os.umask(0o077)
    api = API(args.api, args.insecure)
    api.login()
    args.func(api, args)


if __name__ == "__main__":
    main()
