#!/usr/bin/env python3
"""Prepare a single-server Aether production deployment. Python 3 standard library only.

    python3 infra/prod/setup.py --host aether.hospital.local --tls internal --email ops@example.org

Idempotent: every secret and every certificate is created once and then left alone. Re-running with
different ports or a different hostname rewrites only the derived settings (env file, Caddy site,
MQTT public endpoint). Nothing secret is ever written to stdout.

What it creates
  <secrets>/postgres/       CA + server certificate (SAN "postgres") for the database TLS listener
  <secrets>/db-ca/ca.crt    the same CA, mounted read-only into every database client
  <secrets>/mqtt/ca.key     the MQTT certificate authority key — mounted into NO container
  <secrets>/mqtt/broker/    mosquitto.conf (no 1883 listener), broker certificate, base credentials
  <secrets>/mqtt/runtime/   the password/ACL files that mqtt-provisioner rewrites for each gateway
  <secrets>/mqtt/collector/ CA + collector.json for the ingest worker
  <env-file>                every setting the compose file interpolates (mode 0600)
"""
import argparse
import base64
import ipaddress
import json
import os
import pathlib
import re
import secrets
import shutil
import subprocess
import sys
import uuid

HERE = pathlib.Path(__file__).resolve().parent
ROOT = HERE.parents[1]
MOSQUITTO_IMAGE = "eclipse-mosquitto:2.0.22"
# The backend image (backend/Dockerfile) runs every binary as this uid.
BACKEND_UID = 10001


def run(args, **kwargs):
    subprocess.run(args, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, **kwargs)


def write(path: pathlib.Path, data: str, mode: int = 0o600):
    if path.exists():
        path.chmod(0o600)  # generated files are read-only; re-running setup must still work
    path.write_text(data)
    path.chmod(mode)


def copy(src: pathlib.Path, dst: pathlib.Path, mode: int = 0o444):
    if dst.exists():
        dst.chmod(0o600)
    shutil.copyfile(src, dst)
    dst.chmod(mode)


def own(path: pathlib.Path, uid: int):
    """Give a file to the container uid that must read it. Only possible as root, which is how the
    production server runs setup. Docker Desktop on macOS remaps bind-mount ownership by itself."""
    if os.geteuid() == 0:
        os.chown(path, uid, uid)


def san_entry(host: str) -> str:
    try:
        ipaddress.ip_address(host)
        return "IP:" + host
    except ValueError:
        return "DNS:" + host


def make_ca(key: pathlib.Path, crt: pathlib.Path, cn: str, days: int):
    if crt.exists() and key.exists():
        return False
    run([
        "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", str(days), "-sha256",
        "-subj", "/CN=" + cn, "-keyout", str(key), "-out", str(crt),
        "-addext", "basicConstraints=critical,CA:TRUE,pathlen:0",
        "-addext", "keyUsage=critical,keyCertSign,cRLSign",
    ])
    key.chmod(0o400)
    crt.chmod(0o444)
    return True


def issue_cert(ca_key: pathlib.Path, ca_crt: pathlib.Path, key: pathlib.Path, crt: pathlib.Path,
               cn: str, sans: str, days: int, workdir: pathlib.Path):
    """Issue (or re-issue) a server certificate from an existing CA. Shared with renew-mqtt-cert.py."""
    csr = workdir / (crt.stem + ".csr")
    ext = workdir / (crt.stem + ".ext")
    run(["openssl", "req", "-new", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=" + cn,
         "-keyout", str(key), "-out", str(csr)])
    write(ext, "subjectAltName=" + sans + "\nbasicConstraints=CA:FALSE\n"
               "keyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n")
    run(["openssl", "x509", "-req", "-in", str(csr), "-CA", str(ca_crt), "-CAkey", str(ca_key),
         "-CAcreateserial", "-days", str(days), "-sha256", "-extfile", str(ext), "-out", str(crt)])
    csr.unlink(missing_ok=True)
    ext.unlink(missing_ok=True)
    key.chmod(0o400)
    crt.chmod(0o444)


def expiry(crt: pathlib.Path) -> str:
    out = subprocess.run(["openssl", "x509", "-enddate", "-noout", "-in", str(crt)],
                         check=True, capture_output=True, text=True).stdout
    return out.strip().split("=", 1)[1]


def read_env(path: pathlib.Path) -> dict:
    if not path.exists():
        return {}
    settings = {}
    for line in path.read_text().splitlines():
        if line and not line.startswith("#") and "=" in line:
            k, v = line.split("=", 1)
            settings[k.strip()] = v
    return settings


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--host", required=True,
                   help="the name or IP clients and gateways use: APP_ORIGIN, the Caddy site, the "
                        "MQTT certificate SAN and MQTT_PUBLIC_HOST all come from it")
    p.add_argument("--public-origin", default="",
                   help="the URL browsers use when another reverse proxy (nginx, Cloudflare) sits in front of "
                        "this stack, e.g. https://aether.example.com; becomes APP_ORIGIN (default: derived from --host)")
    p.add_argument("--mqtt-host", default="",
                   help="the name or IP gateways use for MQTT when it differs from --host, e.g. because the web "
                        "name is behind a CDN that cannot carry MQTT (default: --host)")
    p.add_argument("--upstream-proxy", default="",
                   help="space-separated CIDRs of a reverse proxy in front of Caddy; only then does Caddy believe "
                        "its X-Forwarded-For, so the API still sees each browser's own address")
    p.add_argument("--web-bind", default="0.0.0.0",
                   help="host interface for Caddy's HTTPS/HTTP ports; 127.0.0.1 when an outer proxy on this host fronts it")
    p.add_argument("--mqtt-bind", default="0.0.0.0", help="host interface for the 8883 listener")
    p.add_argument("--tls", choices=["internal", "acme"], default="internal")
    p.add_argument("--email", default="", help="ACME account address, and the owner account address")
    p.add_argument("--env-file", default=str(ROOT / ".env.prod"))
    p.add_argument("--secrets-dir", default=str(ROOT / ".secrets" / "prod"))
    p.add_argument("--backup-dir", default=str(ROOT / "backups"))
    p.add_argument("--log-dir", default=str(HERE / "logs"))
    p.add_argument("--https-port", type=int, default=443)
    p.add_argument("--http-port", type=int, default=80)
    p.add_argument("--mqtt-port", type=int, default=8883)
    p.add_argument("--mqtt-plaintext", action="store_true",
                   help="also open an unencrypted MQTT listener for gateways without TLS (passwords and ACLs still apply, but credentials and data cross the LAN in clear text; keep those gateways on an isolated network)")
    p.add_argument("--mqtt-plain-port", type=int, default=1883)
    p.add_argument("--shadow", choices=["true", "false"], default="true",
                   help="ALERTS_SHADOW: record events but send nothing (recommended for day one)")
    p.add_argument("--csp", choices=["enforce", "report-only"], default="enforce")
    p.add_argument("--subnet", default="172.29.7.0/24", help="fixed compose subnet, so TRUSTED_PROXIES can name the proxy exactly")
    p.add_argument("--image-tag", default="prod")
    p.add_argument("--mqtt-uid", type=int, default=None,
                   help="uid for mosquitto and the provisioner (default: 10002 as root, else your own uid)")
    p.add_argument("--sample-min-interval", type=int, default=30)
    p.add_argument("--retention-days", type=int, default=90)
    p.add_argument("--rate-limit", type=int, default=300)
    a = p.parse_args()

    if a.tls == "acme" and not a.email:
        return fail("--tls acme requires --email for the ACME account")
    if not re.fullmatch(r"[A-Za-z0-9._:\-]{1,253}", a.host):
        return fail("--host must be a DNS name or an IP address")
    mqtt_host = a.mqtt_host or a.host
    if not re.fullmatch(r"[A-Za-z0-9._:\-]{1,253}", mqtt_host):
        return fail("--mqtt-host must be a DNS name or an IP address")
    if a.public_origin and not re.fullmatch(r"https://[A-Za-z0-9.\-]{1,253}(:[0-9]{1,5})?", a.public_origin):
        return fail("--public-origin must look like https://name[:port] with no path")
    try:
        if ipaddress.ip_address(a.web_bind).version != 4:
            return fail("--web-bind must be an IPv4 address (compose port syntax)")
    except ValueError:
        return fail("--web-bind must be an IPv4 address")
    for cidr in a.upstream_proxy.split():
        try:
            ipaddress.ip_network(cidr)
        except ValueError:
            return fail("--upstream-proxy must be IP addresses or CIDRs: " + cidr)
    if shutil.which("openssl") is None:
        return fail("openssl is required")
    if shutil.which("docker") is None:
        return fail("docker is required (mosquitto_passwd runs inside the broker image)")

    os.umask(0o077)
    sec = pathlib.Path(a.secrets_dir).resolve()
    env_file = pathlib.Path(a.env_file).resolve()
    backup_dir = pathlib.Path(a.backup_dir).resolve()
    log_dir = pathlib.Path(a.log_dir).resolve()
    for d in (sec, backup_dir, log_dir):
        d.mkdir(parents=True, exist_ok=True)
    # Traversable but not listable: containers run as several different uids (999, 10001, the
    # mosquitto uid) and each must reach exactly the file it is given, nothing else.
    sec.chmod(0o711)

    mqtt_uid = a.mqtt_uid if a.mqtt_uid is not None else (10002 if os.geteuid() == 0 else os.getuid())
    mqtt_gid = mqtt_uid if os.geteuid() == 0 else os.getgid()

    old = read_env(env_file)
    created, kept = [], []

    def secret(name: str, generator) -> str:
        if name in old and old[name]:
            kept.append(name)
            return old[name]
        created.append(name)
        return generator()

    hexkey = lambda: secrets.token_hex(32)                       # noqa: E731
    b64key = lambda n: (lambda: base64.b64encode(secrets.token_bytes(n)).decode())  # noqa: E731

    postgres_password = secret("POSTGRES_PASSWORD", hexkey)
    app_db_password = secret("APP_DB_PASSWORD", hexkey)
    prov_db_password = secret("MQTT_PROVISION_DB_PASSWORD", hexkey)
    jwt_key = secret("JWT_SIGNING_KEY", b64key(48))
    # Sealed notification-channel secrets (LINE tokens, webhook headers) are unrecoverable without
    # this key. It is deliberately separate from JWT_SIGNING_KEY so the JWT key can be rotated.
    seal_key = secret("CHANNEL_SEAL_KEY", b64key(32))

    # ---------------------------------------------------------------- PostgreSQL TLS
    pg = sec / "postgres"
    pg.mkdir(exist_ok=True)
    pg.chmod(0o711)
    dbca = sec / "db-ca"
    dbca.mkdir(exist_ok=True)
    dbca.chmod(0o711)
    if make_ca(pg / "ca.key", pg / "ca.crt", "Aether PostgreSQL CA", 3650):
        created.append("postgres CA")
    if not (pg / "server.crt").exists():
        # sslmode=verify-full checks this SAN against the host in DATABASE_URL, which is "postgres".
        issue_cert(pg / "ca.key", pg / "ca.crt", pg / "server.key", pg / "server.crt",
                   "postgres", "DNS:postgres", 3650, pg)
        created.append("postgres server certificate")
    else:
        kept.append("postgres server certificate")
    copy(pg / "ca.crt", dbca / "ca.crt")

    # ---------------------------------------------------------------- MQTT
    mq = sec / "mqtt"
    broker = mq / "broker"
    runtime = mq / "runtime"
    collector = mq / "collector"
    for d in (mq, broker, collector):
        d.mkdir(exist_ok=True)
        d.chmod(0o711)
    runtime.mkdir(exist_ok=True)
    runtime.chmod(0o700)
    own(runtime, mqtt_uid)

    # The CA key stays here and is mounted into no container: compromising the broker must not let
    # anyone mint a certificate the gateways would trust.
    if make_ca(mq / "ca.key", mq / "ca.crt", "Aether MQTT CA", 3650):
        created.append("MQTT CA (10 years)")
    copy(mq / "ca.crt", broker / "ca.crt")
    copy(mq / "ca.crt", collector / "ca.crt")

    sans = san_entry(mqtt_host) + ",DNS:mqtt"
    if not (broker / "server.crt").exists():
        # 825 days is the longest a modern TLS client will accept for a leaf certificate.
        issue_cert(mq / "ca.key", mq / "ca.crt", broker / "server.key", broker / "server.crt",
                   mqtt_host, sans, 825, mq)
        created.append("MQTT broker certificate (825 days)")
    else:
        kept.append("MQTT broker certificate")
        # The certificate is never reissued automatically (gateways pin nothing, but a new key is still a
        # change to roll out). Say so loudly when the name gateways will be told no longer matches it.
        have = subprocess.run(["openssl", "x509", "-noout", "-ext", "subjectAltName", "-in", str(broker / "server.crt")],
                              capture_output=True, text=True).stdout
        want = san_entry(mqtt_host).replace("IP:", "IP Address:")
        if want not in have:
            print("WARNING: the MQTT broker certificate does not cover " + mqtt_host + " (--mqtt-host/--host). Gateways "
                  "that verify the server will refuse it. Reissue: move " + str(broker / "server.crt") + " away and re-run.",
                  file=sys.stderr)

    ingest_password = secret("MQTT_INGEST_PASSWORD", lambda: secrets.token_urlsafe(32))
    # The collector configuration format requires at least one explicit topic binding. Production
    # gateways are created in the web UI and publish under /aether/gateways/<id>/status, which the
    # collector subscribes to separately, so the binding here points at a reserved topic that no
    # broker account is ever allowed to publish to.
    reserved_gateway = secret("MQTT_RESERVED_GATEWAY_ID", lambda: str(uuid.uuid4()))
    reserved_token = secret("MQTT_RESERVED_TOKEN", lambda: secrets.token_urlsafe(32))
    reserved_topic = "/aether/reserved/" + reserved_gateway

    if not (broker / "passwords").exists():
        write(broker / "passwords", "aether-ingest:" + ingest_password + "\n", 0o600)
        run(["docker", "run", "--rm", "--user", "0", "--entrypoint", "mosquitto_passwd",
             "-v", str(broker) + ":/config", MOSQUITTO_IMAGE, "-U", "/config/passwords"])
        (broker / "passwords").chmod(0o400)
        created.append("broker credentials for the ingest worker")
    else:
        kept.append("broker credentials")

    # Base ACL. mqtt-provisioner appends one block per gateway created in the UI, plus the
    # collector's wildcard read on /aether/gateways/+/status.
    write(broker / "acl",
          "# Base ACL. mqtt-provisioner rewrites the runtime copy; do not edit that one by hand.\n"
          "user aether-ingest\n"
          "topic read " + reserved_topic + "\n", 0o400)

    plain_listener = ""
    if a.mqtt_plaintext:
        # Explicit opt-in (--mqtt-plaintext). per_listener_settings is false, so the password file and ACL above
        # apply to this listener exactly as they do to 8883.
        plain_listener = "\n# Opt-in: unencrypted listener for gateways without TLS support.\nlistener 1883\n"
    write(broker / "mosquitto.conf", f"""# Generated by infra/prod/setup.py. TLS on 8883; plaintext 1883 only with --mqtt-plaintext.
listener 8883
allow_anonymous false
password_file /mosquitto/runtime/passwords
acl_file /mosquitto/runtime/acl
certfile /mosquitto/config/server.crt
keyfile /mosquitto/config/server.key
tls_version tlsv1.2
require_certificate false
persistence true
persistence_location /mosquitto/data/
autosave_interval 30
max_packet_size 262144
max_queued_messages 1000
max_queued_bytes 16777216
max_inflight_messages 20
# Sized for hundreds of gateways plus the collector and short-lived reconnects.
max_connections 1000
memory_limit 268435456
sys_interval 10
log_dest stdout
log_type error
log_type warning
log_type notice
connection_messages true
{plain_listener}""", 0o444)

    # Seed the runtime files so the broker can start before the provisioner's first sync.
    for name in ("passwords", "acl"):
        target = runtime / name
        if not target.exists():
            target.write_bytes((broker / name).read_bytes())
            target.chmod(0o400)

    write(collector / "collector.json", json.dumps({
        "broker_url": "ssl://mqtt:8883",
        "username": "aether-ingest",
        "password": ingest_password,
        "client_id": "aether-ingest-prod",
        "ca_file": "/run/mqtt/ca.crt",
        "bindings": [{"topic": reserved_topic, "gateway_id": reserved_gateway, "token": reserved_token}],
    }), 0o400)

    # Each private file goes to the single uid that reads it.
    own(broker / "server.key", mqtt_uid)
    own(broker / "passwords", mqtt_uid)
    own(broker / "acl", mqtt_uid)
    for name in ("passwords", "acl"):
        own(runtime / name, mqtt_uid)
    own(collector / "collector.json", BACKEND_UID)
    # postgres/server.key is read only by the root-run `prepare` service, which installs a copy
    # owned by uid 999 into the postgres-certs volume; it is never mounted into postgres itself.

    # ---------------------------------------------------------------- derived settings
    port_suffix = "" if a.https_port == 443 else ":" + str(a.https_port)
    http_suffix = "" if a.http_port == 80 else ":" + str(a.http_port)
    origin = "https://" + a.host + port_suffix
    network = ipaddress.ip_network(a.subnet)
    proxy_ip = str(network.network_address + 10)

    settings = {
        "# Aether production settings. Mode 0600. Back this file up OFFLINE, separately from the": "",
        "# database dumps: CHANNEL_SEAL_KEY and JWT_SIGNING_KEY are not recoverable from a dump.": "",
        "AETHER_SECRETS_DIR": str(sec),
        "AETHER_BACKUP_DIR": str(backup_dir),
        "AETHER_CADDY_LOG_DIR": str(log_dir),
        "AETHER_BACKEND_IMAGE": "aether-backend:" + a.image_tag,
        "AETHER_WEB_IMAGE": "aether-web:" + a.image_tag,
        "AETHER_SUBNET": a.subnet,
        "AETHER_PROXY_IP": proxy_ip,
        "AETHER_HOST": a.host,
        "AETHER_SITE": origin,
        "AETHER_HTTP_SITE": "http://" + a.host + http_suffix,
        "AETHER_TLS": "internal" if a.tls == "internal" else a.email,
        "AETHER_CSP_HEADER": "Content-Security-Policy" if a.csp == "enforce" else "Content-Security-Policy-Report-Only",
        "AETHER_HTTPS_PORT": str(a.https_port),
        "AETHER_UPSTREAM_PROXIES": a.upstream_proxy,
        "AETHER_WEB_BIND": a.web_bind,
        "AETHER_HTTP_PORT": str(a.http_port),
        "AETHER_TZ": os.environ.get("TZ", "Asia/Bangkok"),
        "APP_ENV": "production",
        "APP_ORIGIN": a.public_origin or origin,
        "DEPLOYMENT_MODE": "onprem",
        "ALLOW_REGISTRATION": "false",
        "TRUSTED_PROXIES": proxy_ip + "/32",
        "API_RATE_LIMIT": str(a.rate_limit),
        "SAMPLE_RETENTION_DAYS": str(a.retention_days),
        "SAMPLE_MIN_INTERVAL_SEC": str(a.sample_min_interval),
        "BLE_HISTORY_HOURS": "24",
        "DISCOVERY_LIMIT": "100",
        "ALERTS_SHADOW": a.shadow,
        "WEBHOOK_ALLOWED_HOSTS": old.get("WEBHOOK_ALLOWED_HOSTS", ""),
        "SMTP_HOST": old.get("SMTP_HOST", ""),
        "SMTP_PORT": old.get("SMTP_PORT", ""),
        "SMTP_USERNAME": old.get("SMTP_USERNAME", ""),
        "SMTP_PASSWORD": old.get("SMTP_PASSWORD", ""),
        "SMTP_FROM": old.get("SMTP_FROM", ""),
        "MQTT_BIND_IP": a.mqtt_bind,
        "MQTT_PORT": str(a.mqtt_port),
        "MQTT_PUBLIC_HOST": mqtt_host,
        "MQTT_PUBLIC_PORT": str(a.mqtt_port),
        "MQTT_PUBLIC_SCHEME": "ssl",
        "MQTT_ALLOW_PLAINTEXT": "true" if a.mqtt_plaintext else "false",
        "MQTT_PLAIN_PORT": str(a.mqtt_plain_port),
        # Unpublished (loopback, and no listener behind it) unless --mqtt-plaintext.
        "MQTT_PLAIN_BIND": a.mqtt_bind if a.mqtt_plaintext else "127.0.0.1",
        "MQTT_UID": str(mqtt_uid),
        "MQTT_GID": str(mqtt_gid),
        "BACKUP_AT": old.get("BACKUP_AT", "02:30"),
        "BACKUP_KEEP_DAILY": old.get("BACKUP_KEEP_DAILY", "14"),
        "BACKUP_KEEP_WEEKLY": old.get("BACKUP_KEEP_WEEKLY", "8"),
        "POSTGRES_PASSWORD": postgres_password,
        "APP_DB_PASSWORD": app_db_password,
        "MQTT_PROVISION_DB_PASSWORD": prov_db_password,
        "JWT_SIGNING_KEY": jwt_key,
        "CHANNEL_SEAL_KEY": seal_key,
        "MQTT_INGEST_PASSWORD": ingest_password,
        "MQTT_RESERVED_GATEWAY_ID": reserved_gateway,
        "MQTT_RESERVED_TOKEN": reserved_token,
        "OWNER_EMAIL": a.email or old.get("OWNER_EMAIL", ""),
    }
    lines = []
    for key, value in settings.items():
        lines.append(key if key.startswith("#") else key + "=" + value)
    write(env_file, "\n".join(lines) + "\n", 0o600)

    print("Aether production configuration is ready.")
    print("  env file      : " + str(env_file) + " (0600)")
    print("  secrets       : " + str(sec) + " (0700)")
    print("  backups       : " + str(backup_dir))
    print("  access log    : " + str(log_dir) + "/access.log")
    print("  site          : " + origin + "   TLS mode: " + a.tls)
    if a.public_origin:
        print("  public origin : " + a.public_origin + "   (APP_ORIGIN, behind " + (a.upstream_proxy or "an outer proxy") + ")")
    print("  MQTT for MG3  : ssl://" + mqtt_host + ":" + str(a.mqtt_port) +
          "   CA to upload: " + str(broker / "ca.crt"))
    print("  broker cert   : expires " + expiry(broker / "server.crt"))
    if a.mqtt_plaintext:
        print("  MQTT no TLS   : tcp://" + mqtt_host + ":" + str(a.mqtt_plain_port) +
              "   WARNING: credentials and data are not encrypted; isolate these gateways")
    print("  proxy address : " + proxy_ip + " (TRUSTED_PROXIES)")
    print("  created       : " + (", ".join(created) if created else "nothing, everything existed"))
    if kept:
        print("  preserved     : " + ", ".join(sorted(set(kept))))
    print("No secret values were printed. Next: build the images, then `docker compose "
          "--env-file " + str(env_file) + " -f infra/prod/compose.yaml up -d`.")
    return 0


def fail(message: str) -> int:
    print("setup failed: " + message, file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
