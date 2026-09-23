#!/usr/bin/env python3
"""Re-issue the Mosquitto broker certificate from the existing MQTT CA.

    sudo python3 infra/prod/renew-mqtt-cert.py [--env-file .env.prod] [--days 825]

The CA is untouched, so every gateway keeps trusting the broker and nobody has to walk around with
a USB stick re-uploading certificates. Only the broker's own leaf certificate and key are replaced.
Run it well before the printed expiry date; gateways refuse an expired certificate and go silent.
"""
import argparse
import os
import pathlib
import shutil
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from setup import issue_cert, expiry, read_env, san_entry, own, BACKEND_UID  # noqa: E402

HERE = pathlib.Path(__file__).resolve().parent
ROOT = HERE.parents[1]


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--env-file", default=str(ROOT / ".env.prod"))
    p.add_argument("--days", type=int, default=825, help="leaf certificate lifetime (public clients cap this at 825)")
    p.add_argument("--host", default="", help="override the SAN host; defaults to AETHER_HOST from the env file")
    a = p.parse_args()

    env = read_env(pathlib.Path(a.env_file))
    if not env:
        print("cannot read " + a.env_file + "; run infra/prod/setup.py first", file=sys.stderr)
        return 1
    host = a.host or env.get("MQTT_PUBLIC_HOST") or env.get("AETHER_HOST", "")
    sec = pathlib.Path(env["AETHER_SECRETS_DIR"])
    mq, broker = sec / "mqtt", sec / "mqtt" / "broker"
    if not (mq / "ca.key").exists():
        print("MQTT CA key missing at " + str(mq / "ca.key"), file=sys.stderr)
        return 1

    os.umask(0o077)
    previous = expiry(broker / "server.crt") if (broker / "server.crt").exists() else "none"
    for name in ("server.key", "server.crt"):
        target = broker / name
        if target.exists():
            target.chmod(0o600)
            shutil.copyfile(target, broker / (name + ".previous"))
            (broker / (name + ".previous")).chmod(0o400)

    issue_cert(mq / "ca.key", mq / "ca.crt", broker / "server.key", broker / "server.crt",
               host, san_entry(host) + ",DNS:mqtt", a.days, mq)
    own(broker / "server.key", int(env.get("MQTT_UID", BACKEND_UID)))

    print("Broker certificate re-issued from the existing CA (gateways keep trusting it).")
    print("  host / SAN : " + host + ", mqtt")
    print("  was        : " + previous)
    print("  now        : " + expiry(broker / "server.crt"))
    print("  previous copies kept as server.crt.previous / server.key.previous")
    print("Apply it with:")
    print("  docker compose --env-file " + a.env_file + " -f infra/prod/compose.yaml kill -s HUP mqtt")
    print("If the broker does not pick it up (Mosquitto reloads TLS material only on some builds),")
    print("restart it instead: docker compose ... restart mqtt  — gateways reconnect on their own.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
