#!/usr/bin/env python3
"""Run the API customer journey in the Linux sandbox against aether_test only.

Requires the existing local PostgreSQL stack and aether-backend:local image.
Credentials are passed through the environment, never arguments or output.
"""
import json
import os
import pathlib
import subprocess
import tempfile
import sys
from contextlib import nullcontext
from mqtt_journey_runtime import mqtt_runtime

root = pathlib.Path(__file__).resolve().parents[1]
config = dict(
    line.split("=", 1) for line in (root / ".env").read_text().splitlines()
    if line and not line.startswith("#")
)
image = "aether-backend:local"
arch = subprocess.check_output(
    ["docker", "image", "inspect", image, "--format", "{{.Architecture}}"], text=True
).strip()
networks = json.loads(subprocess.check_output(
    ["docker", "inspect", "aether-postgres-1", "--format", "{{json .NetworkSettings.Networks}}"], text=True
))
if "aether_default" not in networks:
    raise SystemExit("Start the local aether PostgreSQL stack before running this test.")
env = dict(os.environ)
env["TEST_DATABASE_URL"] = (
    f"postgresql://aether_app:{config['APP_DB_PASSWORD']}@postgres:5432/aether_test?sslmode=disable"
)
env["TEST_ADMIN_DATABASE_URL"] = (
    f"postgresql://postgres:{config['POSTGRES_PASSWORD']}@postgres:5432/aether_test?sslmode=disable"
)
with tempfile.TemporaryDirectory(prefix="aether-journey-") as temporary:
    binary = pathlib.Path(temporary) / "journey.test"
    build_env = dict(os.environ, GOOS="linux", GOARCH=arch, CGO_ENABLED="0")
    subprocess.run(
        ["go", "test", "-c", "-o", str(binary), "./tests"],
        cwd=root / "backend", env=build_env, check=True,
    )
    command = [
        "docker", "run", "--rm", "--network", "aether_default", "--read-only",
        "--cap-drop=ALL", "--security-opt=no-new-privileges",
        "--env", "TEST_DATABASE_URL", "--env", "TEST_ADMIN_DATABASE_URL",
        "--env", "PYTHONPATH=/app/python",
        "--mount", f"type=bind,src={binary},dst=/app/journey.test,readonly",
        "--mount", f"type=bind,src={root / 'backend/migrations'},dst=/app/migrations,readonly",
        "--mount", f"type=bind,src={root / 'backend/sandbox'},dst=/app/sandbox,readonly",
        "--workdir", "/app/tests", "--entrypoint", "/app/journey.test", image,
        "-test.v", "-test.run=^TestCustomerJourneyFromGatewayToDashboardAndAlert$",
    ]
    mode = mqtt_runtime(root, temporary, image, build_env, config, env) if "--mqtt" in sys.argv else nullcontext(None)
    with mode as broker:
        if broker:
            env["MQTT_JOURNEY_BROKER"] = broker
            # Insert environment flags before the image, not among test-binary arguments.
            command[2:2] = ["--env", "MQTT_JOURNEY_BROKER"]
        raise SystemExit(subprocess.call(command, env=env))
