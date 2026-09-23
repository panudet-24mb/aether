#!/usr/bin/env python3
"""Generate local development secrets without printing them or overwriting existing configuration."""
import base64
import os
import pathlib
import secrets
path = pathlib.Path(__file__).resolve().parents[1] / '.env'
config = '\n'.join([
    'POSTGRES_PASSWORD=' + secrets.token_hex(32),
    'APP_DB_PASSWORD=' + secrets.token_hex(32),
    'JWT_SIGNING_KEY=' + base64.b64encode(secrets.token_bytes(48)).decode(),
    'DEPLOYMENT_MODE=onprem',
    'APP_ORIGIN=http://localhost:3000',
    'POSTGRES_PORT=55432',
    'API_PORT=8080',
    '',
])
fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, 'w') as output:
    output.write(config)
print('Created local .env with private permissions. No credentials were printed.')
