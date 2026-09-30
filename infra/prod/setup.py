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
  <secrets>/mqtt/commander/ CA + commander.json for mqtt-commander (write-only on aether/z2m/+/+/set)
  <secrets>/pgbackrest/     pgbackrest.conf, with the repository encryption passphrase (PGBACKREST_CIPHER_PASS)
  <secrets>/ops/            webhook-url for backup/PITR alerts (only with --ops-webhook)
  <pitr-dir>                the pgBackRest repository: encrypted base backups and archived WAL (point-in-time recovery)
  docker volume             <project>_postgres-data, the database (external to compose), when it does not exist yet
  <env-file>                every setting the compose file interpolates (mode 0600)
"""
import argparse
import base64
import hashlib
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
# The postgres uid of the Alpine postgres image (infra/prod/postgres/Dockerfile): pgBackRest runs as it.
POSTGRES_UID = 70
# The data directory inside the postgres image (PGDATA of postgres:18).
PGDATA = "/var/lib/postgresql/18/docker"


def run(args, **kwargs):
    subprocess.run(args, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, **kwargs)


def write(path: pathlib.Path, data: str, mode: int = 0o600):
    if path.exists():
        path.chmod(0o600)  # generated files are read-only; re-running setup must still work
    # Created 0600 (never readable by others, whatever the umask), then given its final mode.
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(data)
    path.chmod(mode)


def copy(src: pathlib.Path, dst: pathlib.Path, mode: int = 0o444):
    if dst.exists():
        dst.chmod(0o600)
    shutil.copyfile(src, dst)
    dst.chmod(mode)



def mosquitto_hash(password: str) -> str:
    """A Mosquitto 2.x password hash ($7$: PBKDF2-HMAC-SHA512, 64-byte key), as mosquitto_passwd writes it."""
    salt = secrets.token_bytes(12)
    key = hashlib.pbkdf2_hmac("sha512", password.encode(), salt, 101, 64)
    return "$7$101$" + base64.b64encode(salt).decode() + "$" + base64.b64encode(key).decode()


def mosquitto_verify(encoded: str, password: str) -> bool:
    m = re.fullmatch(r"\$7\$(\d+)\$([A-Za-z0-9+/=]+)\$([A-Za-z0-9+/=]+)", encoded)
    if not m or not 1 <= int(m.group(1)) <= 10_000_000:
        return False
    key = hashlib.pbkdf2_hmac("sha512", password.encode(), base64.b64decode(m.group(2)), int(m.group(1)), 64)
    return secrets.compare_digest(key, base64.b64decode(m.group(3)))


def sync_password_line(path: pathlib.Path, user: str, password: str) -> str:
    """Make `user`'s line in a Mosquitto password file verify `password`, touching no other line. Returns
    kept | added | replaced. The file is rewritten in place so bind mounts of it keep seeing it."""
    lines = path.read_text().splitlines()
    mine = [i for i, line in enumerate(lines) if line.startswith(user + ":")]
    if len(mine) == 1 and mosquitto_verify(lines[mine[0]].split(":", 1)[1], password):
        return "kept"
    entry = user + ":" + mosquitto_hash(password)
    status = "replaced" if mine else "added"
    lines = [line for i, line in enumerate(lines) if i not in mine] + [entry]
    path.chmod(0o600)
    with open(path, "r+") as f:
        f.seek(0)
        f.write("\n".join(lines) + "\n")
        f.truncate()
    path.chmod(0o400)
    return status

def x25519_public(private_b64: str) -> str:
    """The X25519 public key (RFC 7748) of a base64 private key, in base64: pure Python, no dependency."""
    k = bytearray(base64.b64decode(private_b64))
    if len(k) != 32:
        raise ValueError("TUYA_CLOUD_PRIVATE_KEY must be 32 bytes")
    k[0] &= 248
    k[31] &= 127
    k[31] |= 64
    scalar = int.from_bytes(k, "little")
    p, a24 = 2**255 - 19, 121665
    x1, x2, z2, x3, z3, swap = 9, 1, 0, 9, 1, 0
    for t in reversed(range(255)):
        bit = (scalar >> t) & 1
        swap ^= bit
        if swap:
            x2, x3, z2, z3 = x3, x2, z3, z2
        swap = bit
        a, b = (x2 + z2) % p, (x2 - z2) % p
        aa, bb = a * a % p, b * b % p
        e = (aa - bb) % p
        c, d = (x3 + z3) % p, (x3 - z3) % p
        da, cb = d * a % p, c * b % p
        x3, z3 = (da + cb) ** 2 % p, x1 * (da - cb) ** 2 % p
        x2, z2 = aa * bb % p, e * (aa + a24 * e) % p
    if swap:
        x2, z2 = x3, z3
    return base64.b64encode((x2 * pow(z2, p - 2, p) % p).to_bytes(32, "little")).decode()


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
    p.add_argument("--project", default="aether-prod",
                   help="compose project name; the database volume is <project>_postgres-data (default aether-prod)")
    p.add_argument("--pitr-dir", default=None,
                   help="pgBackRest repository for point-in-time recovery (default: the value already in the env file, "
                        "else /var/lib/aether/pitr as root, else <repo>/pitr); outside the checkout so `git clean` cannot "
                        "remove it; room for ~2 full backups plus 1-2 weeks of WAL and PITR_QUEUE_MAX")
    p.add_argument("--pitr-keep-full", type=int, default=None,
                   help="full backups to keep (weekly fulls: 2 gives a 7-14 day recovery window; default 2 or the kept value)")
    p.add_argument("--pitr-queue-max", default=None,
                   help="archive-push-queue-max: WAL allowed to pile up while the repository is unreachable before "
                        "pgBackRest drops it (and alerts) instead of filling the disk; default 8GB or the kept value")
    p.add_argument("--pitr-backup-at", default=None, help="daily pgBackRest backup time HH:MM (default 03:30 or kept)")
    p.add_argument("--ops-webhook", default=None,
                   help="https URL that receives backup/PITR alerts as JSON (Slack/Discord-compatible text field), stored "
                        "in <secrets>/ops/webhook-url (0400), not the env file; '' to remove; kept when omitted")
    p.add_argument("--offsite", default=None,
                   help="rclone remote:path that receives the PITR repository and the dumps every 15 minutes (runs the "
                        "pitr-offsite service; needs <secrets>/rclone/rclone.conf); 'off' to stop; kept when omitted")
    p.add_argument("--log-dir", default=str(HERE / "logs"))
    p.add_argument("--https-port", type=int, default=443)
    p.add_argument("--http-port", type=int, default=80)
    p.add_argument("--mqtt-port", type=int, default=8883)
    p.add_argument("--mqtt-plaintext", action="store_true",
                   help="also open an unencrypted MQTT listener for gateways without TLS (passwords and ACLs still apply, but credentials and data cross the LAN in clear text; keep those gateways on an isolated network)")
    p.add_argument("--mqtt-plain-port", type=int, default=1883)
    p.add_argument("--shadow", choices=["true", "false"], default="true",
                   help="ALERTS_SHADOW: record events but send nothing (recommended for day one). NOTE: unlike "
                        "--automation-commands this is not preserved: every re-run sets it, true unless --shadow false "
                        "is given again")
    p.add_argument("--automation-commands", choices=["true", "false"], default=None,
                   help="AUTOMATION_COMMANDS: let enabled automations command devices (action.command). Off on a "
                        "fresh install; a re-run without this flag keeps the value already in the env file")
    p.add_argument("--privacy-notice-url", default=None,
                   help="PRIVACY_NOTICE_URL: the organisation's own privacy notice (https://...). Empty or unset: the "
                        "built-in Thai template at /privacy. A re-run without this flag keeps the value already set; "
                        "'off' clears it")
    p.add_argument("--access-log-days", type=int, default=None,
                   help="ACCESS_LOG_RETENTION_DAYS: how long the read-access log of personal data is kept (30-3650, "
                        "default 400); a re-run without this flag keeps the value already set")
    p.add_argument("--tuya-cloud", choices=["true", "false"], default=None,
                   help="TUYA_CLOUD: offer Tuya Cloud (zero-install) gateways and run the tuya-cloud worker. Off on a "
                        "fresh install; a re-run without this flag keeps the value already in the env file "
                        "(docs/platform/tuya-cloud.md)")
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
    if a.pitr_keep_full is not None and not 1 <= a.pitr_keep_full <= 52:
        return fail("--pitr-keep-full must be between 1 and 52")
    if a.pitr_queue_max is not None and not re.fullmatch(r"[1-9][0-9]{0,5}(MB|GB)", a.pitr_queue_max):
        return fail("--pitr-queue-max must look like 512MB or 8GB")
    if a.pitr_backup_at is not None and not re.fullmatch(r"([01][0-9]|2[0-3]):[0-5][0-9]", a.pitr_backup_at):
        return fail("--pitr-backup-at must be HH:MM")
    if a.ops_webhook and not re.fullmatch(r"https://[^\s'\"]{1,2000}", a.ops_webhook):
        return fail("--ops-webhook must be an https:// URL")
    if a.privacy_notice_url not in (None, "", "off") and not re.fullmatch(r"https://[^\s'\"<>]{3,500}", a.privacy_notice_url):
        return fail("--privacy-notice-url must be an https:// URL (or 'off')")
    if a.access_log_days is not None and not 30 <= a.access_log_days <= 3650:
        return fail("--access-log-days must be between 30 and 3650")
    if a.offsite and a.offsite != "off" and not re.fullmatch(r"[A-Za-z0-9_.\-]{1,64}:[A-Za-z0-9_./\-]{0,500}", a.offsite):
        return fail("--offsite must be an rclone remote:path, e.g. offsite-crypt:aether")
    if not re.fullmatch(r"[a-z0-9][a-z0-9_-]{0,62}", a.project):
        return fail("--project must be a compose project name (lowercase letters, digits, _ and -)")
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
    old_early = read_env(env_file)
    # Outside the checkout by default, so `git clean -fdx` cannot delete it; a value already in the env file wins.
    default_pitr = "/var/lib/aether/pitr" if os.geteuid() == 0 else str(ROOT / "pitr")
    pitr_dir = pathlib.Path(a.pitr_dir or old_early.get("AETHER_PITR_DIR") or default_pitr).resolve()
    # A new passphrase can never read an existing repository: refuse instead of silently orphaning its backups.
    if not old_early.get("PGBACKREST_CIPHER_PASS") and pitr_dir.is_dir() and any(pitr_dir.iterdir()):
        return fail("a pgBackRest repository already exists in " + str(pitr_dir) + " but " + str(env_file) + " has no "
                    "PGBACKREST_CIPHER_PASS. Restore the env file from its offline copy (a new passphrase could never "
                    "read those backups), or point --pitr-dir at an empty directory")
    for d in (sec, backup_dir, log_dir, pitr_dir):
        d.mkdir(parents=True, exist_ok=True)
    # The repository belongs to the postgres uid (archive-push runs in the server, backups in the pitr service).
    own(pitr_dir, POSTGRES_UID)
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
    # The API's login role (migration 00040): the only database login that can read a password hash.
    auth_db_password = secret("AUTH_DB_PASSWORD", hexkey)
    jwt_key = secret("JWT_SIGNING_KEY", b64key(48))
    # Sealed notification-channel secrets (LINE tokens, webhook headers) are unrecoverable without
    # this key. It is deliberately separate from JWT_SIGNING_KEY so the JWT key can be rotated.
    seal_key = secret("CHANNEL_SEAL_KEY", b64key(32))
    # Tuya Cloud project credentials are sealed by the API to the public key and opened only by the tuya-cloud
    # worker with the private key: the private key goes to that one container and nowhere else (compose, phase G5).
    # Generated once and kept; the public key is always derived from it.
    tuya_cloud_private = secret("TUYA_CLOUD_PRIVATE_KEY", b64key(32))
    tuya_cloud_public = x25519_public(tuya_cloud_private)
    # Encrypts every file in the pgBackRest repository (aes-256-cbc). Generated once and kept: LOSING IT MAKES EVERY
    # PITR BACKUP UNREADABLE, which is why it is in the env file that must be backed up offline.
    pitr_cipher = secret("PGBACKREST_CIPHER_PASS", lambda: secrets.token_urlsafe(48))

    # ---------------------------------------------------------------- PostgreSQL TLS
    pg = sec / "postgres"
    pg.mkdir(exist_ok=True)
    pg.chmod(0o711)
    dbca = sec / "db-ca"
    dbca.mkdir(exist_ok=True)
    dbca.chmod(0o711)
    # Optional root of a private web CA (AETHER_TLS=internal) for Aether Edge installers; empty unless the operator
    # exports Caddy's root into it (docs/production.md). Mounted read-only into the api container.
    webca = sec / "edge-web-ca"
    webca.mkdir(exist_ok=True)
    webca.chmod(0o755)
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
    commander = mq / "commander"
    for d in (mq, broker, collector, commander):
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
    copy(mq / "ca.crt", commander / "ca.crt")

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

    # mqtt-commander's account (write-only on aether/z2m/+/+/set, see the provisioner). Its line in the password
    # file is kept in step with MQTT_COMMANDER_PASSWORD: added when missing, replaced when it no longer verifies
    # (the password was regenerated), and left alone otherwise. Only that line is ever written; every other entry
    # stays byte for byte, and the file keeps its inode (the provisioner bind-mounts it).
    commander_password = secret("MQTT_COMMANDER_PASSWORD", lambda: secrets.token_urlsafe(32))
    status = sync_password_line(broker / "passwords", "aether-commander", commander_password)
    if status == "kept":
        kept.append("command publisher credentials")
    else:
        created.append("broker credentials for the command publisher (" + status + ")")

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
max_packet_size 1048576
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
    write(commander / "commander.json", json.dumps({
        "broker_url": "ssl://mqtt:8883",
        "username": "aether-commander",
        "password": commander_password,
        "client_id": "aether-commander-prod",
        "ca_file": "/run/mqtt/ca.crt",
        "bindings": [],
    }), 0o400)

    # Each private file goes to the single uid that reads it.
    own(broker / "server.key", mqtt_uid)
    own(broker / "passwords", mqtt_uid)
    own(broker / "acl", mqtt_uid)
    for name in ("passwords", "acl"):
        own(runtime / name, mqtt_uid)
    own(collector / "collector.json", BACKEND_UID)
    own(commander / "commander.json", BACKEND_UID)
    # postgres/server.key is read only by the root-run `prepare` service, which installs a copy
    # owned by uid 999 into the postgres-certs volume; it is never mounted into postgres itself.

    # ---------------------------------------------------------------- pgBackRest (point-in-time recovery)
    pitr_keep_full = str(a.pitr_keep_full) if a.pitr_keep_full is not None else old.get("PITR_KEEP_FULL", "2")
    pitr_queue_max = a.pitr_queue_max or old.get("PITR_QUEUE_MAX", "8GB")
    pgbr = sec / "pgbackrest"
    pgbr.mkdir(exist_ok=True)
    pgbr.chmod(0o711)
    # Mounted read-only into postgres (archive-push) and pitr (backups), both running as the postgres uid.
    write(pgbr / "pgbackrest.conf", f"""# Generated by infra/prod/setup.py from .env.prod; edits are overwritten on the next run.
[global]
repo1-path=/var/lib/pgbackrest
repo1-cipher-type=aes-256-cbc
repo1-cipher-pass={pitr_cipher}
repo1-retention-full-type=count
repo1-retention-full={pitr_keep_full}
repo1-bundle=y
repo1-block=y
compress-type=zst
compress-level=3
process-max=2
start-fast=y
archive-async=y
spool-path=/var/spool/pgbackrest
archive-push-queue-max={pitr_queue_max}
archive-timeout=120
lock-path=/tmp/pgbackrest
log-level-console=warn
log-level-stderr=off
log-level-file=off

[aether]
pg1-path={PGDATA}
pg1-socket-path=/var/run/postgresql
pg1-user=postgres
pg1-database=aether
""", 0o400)
    own(pgbr / "pgbackrest.conf", POSTGRES_UID)

    offsite = old.get("OFFSITE_TARGET", "") if a.offsite is None else ("" if a.offsite == "off" else a.offsite)
    if offsite:
        rc = sec / "rclone"
        rc.mkdir(exist_ok=True)
        rc.chmod(0o700)
        if not (rc / "rclone.conf").exists():
            print("WARNING: --offsite needs " + str(rc / "rclone.conf") + " (create it with `rclone config`; use a crypt "
                  "remote so the dumps are encrypted off-site). pitr-offsite will fail until it exists.", file=sys.stderr)
    ops = sec / "ops"
    ops.mkdir(exist_ok=True)
    ops.chmod(0o711)
    if a.ops_webhook is not None:
        if a.ops_webhook:
            write(ops / "webhook-url", a.ops_webhook, 0o400)
            own(ops / "webhook-url", POSTGRES_UID)
        else:
            (ops / "webhook-url").unlink(missing_ok=True)
    ops_webhook = (ops / "webhook-url").exists()

    # The database volume is external to compose (see compose.yaml): create it on a new install, with the labels
    # compose would have given it, and never touch an existing one.
    pg_volume = old.get("AETHER_PG_VOLUME") or a.project + "_postgres-data"
    if subprocess.run(["docker", "volume", "inspect", pg_volume], stdout=subprocess.DEVNULL,
                      stderr=subprocess.DEVNULL).returncode != 0:
        run(["docker", "volume", "create", "--label", "com.docker.compose.project=" + a.project,
             "--label", "com.docker.compose.volume=postgres-data", pg_volume])
        created.append("database volume " + pg_volume)

    # ---------------------------------------------------------------- derived settings
    port_suffix = "" if a.https_port == 443 else ":" + str(a.https_port)
    http_suffix = "" if a.http_port == 80 else ":" + str(a.http_port)
    origin = "https://" + a.host + port_suffix
    network = ipaddress.ip_network(a.subnet)
    proxy_ip = str(network.network_address + 10)

    settings = {
        "# Aether production settings. Mode 0600. Back this file up OFFLINE, separately from the": "",
        "# database dumps: CHANNEL_SEAL_KEY and JWT_SIGNING_KEY are not recoverable from a dump, and without": "",
        "# PGBACKREST_CIPHER_PASS no point-in-time backup can be read.": "",
        "AETHER_SECRETS_DIR": str(sec),
        "AETHER_BACKUP_DIR": str(backup_dir),
        "AETHER_PITR_DIR": str(pitr_dir),
        # The database volume. Only restore-pitr.sh changes it (cutover / rollback); kept across re-runs, as is the
        # volume it replaced last (the rollback target).
        "AETHER_PG_VOLUME": pg_volume,
        "AETHER_PG_VOLUME_PREVIOUS": old.get("AETHER_PG_VOLUME_PREVIOUS", ""),
        "AETHER_POSTGRES_IMAGE": "aether-postgres:" + a.image_tag,
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
        # Personal data (docs/platform/privacy.md): the read-access log's retention and the privacy notice link.
        "ACCESS_LOG_RETENTION_DAYS": str(a.access_log_days) if a.access_log_days is not None else old.get("ACCESS_LOG_RETENTION_DAYS", "400"),
        "PRIVACY_NOTICE_URL": ("" if a.privacy_notice_url == "off" else a.privacy_notice_url) if a.privacy_notice_url is not None else old.get("PRIVACY_NOTICE_URL", ""),
        "DISCOVERY_LIMIT": "100",
        "ALERTS_SHADOW": a.shadow,
        # Kept across re-runs unless given: switching device commands on or off must always be a deliberate act.
        "AUTOMATION_COMMANDS": a.automation_commands or old.get("AUTOMATION_COMMANDS", "false"),
        # Tuya Cloud mode stays off until switched on deliberately (--tuya-cloud true), and a re-run keeps it.
        "TUYA_CLOUD": a.tuya_cloud or old.get("TUYA_CLOUD", "false"),
        "TUYA_CLOUD_MAX_LINKS": old.get("TUYA_CLOUD_MAX_LINKS", "50"),
        "TUYA_CLOUD_MAX_LINKS_PER_TENANT": old.get("TUYA_CLOUD_MAX_LINKS_PER_TENANT", "2"),
        # Monthly allowances of one linked project (placeholders until set to the project's real plan; 0 = no guard).
        "TUYA_CLOUD_EVENT_BUDGET": old.get("TUYA_CLOUD_EVENT_BUDGET", "68000"),
        "TUYA_CLOUD_API_BUDGET": old.get("TUYA_CLOUD_API_BUDGET", "26000"),
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
        "PITR_BACKUP_AT": a.pitr_backup_at or old.get("PITR_BACKUP_AT", "03:30"),
        "PITR_FULL_DAY": old.get("PITR_FULL_DAY", "7"),
        "PITR_KEEP_FULL": pitr_keep_full,
        "PITR_QUEUE_MAX": pitr_queue_max,
        "OFFSITE_TARGET": offsite,
        "OFFSITE_EVERY": old.get("OFFSITE_EVERY", "900"),
        # Starts the optional pitr-offsite service only when an off-site target is configured.
        "COMPOSE_PROFILES": "offsite" if offsite else "",
        "POSTGRES_PASSWORD": postgres_password,
        "APP_DB_PASSWORD": app_db_password,
        "MQTT_PROVISION_DB_PASSWORD": prov_db_password,
        "AUTH_DB_PASSWORD": auth_db_password,
        "JWT_SIGNING_KEY": jwt_key,
        "CHANNEL_SEAL_KEY": seal_key,
        "TUYA_CLOUD_PUBLIC_KEY": tuya_cloud_public,
        "TUYA_CLOUD_PRIVATE_KEY": tuya_cloud_private,
        "PGBACKREST_CIPHER_PASS": pitr_cipher,
        "MQTT_INGEST_PASSWORD": ingest_password,
        "MQTT_COMMANDER_PASSWORD": commander_password,
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
    print("  PITR repo     : " + str(pitr_dir) + "   (" + pitr_keep_full + " full backups, queue max " + pitr_queue_max +
          (", off-site " + offsite if offsite else ", no off-site copy") +
          (", alerts to the ops webhook" if ops_webhook else ", alerts in the pitr log only") + ")")
    print("  database vol  : " + pg_volume)
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
