"""Disposable MQTT processes for test-customer-journey.py --mqtt; no host ports."""
import base64
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import time
import uuid


@contextmanager
def mqtt_runtime(root, temporary, image, build_env, config, test_env):
    folder = Path(temporary)
    prefix = "aether-journey-" + uuid.uuid4().hex[:10]
    started = []

    def private(path, content):
        path.write_text(content)
        path.chmod(0o600)

    def start(suffix, entrypoint, mounts, environment=None, options=None, args=None):
        name = prefix + "-" + suffix
        command = ["docker", "run", "-d", "--name", name, "--network", "aether_default",
                   "--user", f"{os.getuid()}:{os.getgid()}", "--read-only",
                   "--cap-drop=ALL", "--security-opt=no-new-privileges"]
        for source, target, readonly in mounts:
            command += ["--mount", f"type=bind,src={source},dst={target}" + (",readonly" if readonly else "")]
        env = dict(os.environ)
        for key, value in (environment or {}).items():
            env[key] = value
            command += ["--env", key]
        command += options or []
        command += ["--entrypoint", entrypoint,
                    "eclipse-mosquitto:2.0.22" if suffix == "broker" else image]
        command += args or []
        subprocess.run(command, env=env, check=True, stdout=subprocess.DEVNULL)
        started.append(name)
        return name

    try:
        for name in ("mqtt", "mqtt-provisioner"):
            subprocess.run(["go", "build", "-o", str(folder / name), "./cmd/" + name],
                           cwd=root / "backend", env=build_env, check=True)
        base, runtime = folder / "base", folder / "runtime"
        base.mkdir(); runtime.mkdir()
        password = secrets.token_urlsafe(32)
        salt = secrets.token_bytes(12)
        hashed = hashlib.pbkdf2_hmac("sha512", password.encode(), salt, 100000, 64)
        row = "aether-ingest:$7$100000$" + base64.b64encode(salt).decode() + "$" + base64.b64encode(hashed).decode() + "\n"
        for directory in (base, runtime):
            private(directory / "passwords", row)
            private(directory / "acl", "user aether-ingest\ntopic read /aether/gateways/+/status\n")
        broker_name = prefix + "-broker"
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
                        "-keyout", str(folder / "server.key"), "-out", str(folder / "server.crt"),
                        "-days", "1", "-subj", "/CN=" + broker_name,
                        "-addext", "subjectAltName=DNS:" + broker_name],
                       check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        private(folder / "mosquitto.conf", "allow_anonymous false\npassword_file /run/mqtt-runtime/passwords\nacl_file /run/mqtt-runtime/acl\npersistence false\nlistener 1883\nlistener 8883\ncertfile /test/server.crt\nkeyfile /test/server.key\n")
        broker = start("broker", "/usr/sbin/mosquitto",
                       [(folder, "/test", True), (runtime, "/run/mqtt-runtime", True)],
                       args=["-c", "/test/mosquitto.conf"])
        provision_dsn = f"postgresql://aether_mqtt_provisioner:{config['MQTT_PROVISION_DB_PASSWORD']}@postgres:5432/aether_test?sslmode=disable"
        start("provisioner", "/test/mqtt-provisioner",
              [(folder, "/test", True), (base, "/run/mqtt-base", True), (runtime, "/run/mqtt-runtime", False)],
              {"DATABASE_URL": provision_dsn}, ["--pid", "container:" + broker])
        private(folder / "collector.json", json.dumps({
            "broker_url": "ssl://" + broker + ":8883", "username": "aether-ingest", "password": password,
            "client_id": prefix + "-collector", "ca_file": "/test/server.crt", "bindings": [],
        }))
        collector = start("collector", "/test/mqtt", [(folder, "/test", True)], {
            "DATABASE_URL": test_env["TEST_DATABASE_URL"], "APP_ENV": "test",
            "APP_ORIGIN": "http://localhost:3000", "DEPLOYMENT_MODE": "onprem",
            "JWT_SIGNING_KEY": base64.b64encode(secrets.token_bytes(32)).decode(),
            "MQTT_CONFIG_FILE": "/test/collector.json",
        })
        for _ in range(40):
            logs = subprocess.check_output(["docker", "logs", collector], stderr=subprocess.STDOUT, text=True)
            if "MQTT capture ready" in logs:
                break
            time.sleep(0.25)
        else:
            raise RuntimeError("Disposable MQTT collector did not become ready")
        yield "tcp://" + broker + ":1883"
    finally:
        for name in reversed(started):
            subprocess.run(["docker", "rm", "-f", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
