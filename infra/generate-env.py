#!/usr/bin/env python3
"""Generate local development secrets without printing them or overwriting existing configuration.

--add-missing keeps an existing .env and only appends settings added since it was made (e.g. AUTH_DB_PASSWORD).
"""
import base64
import os
import pathlib
import secrets
import sys
path = pathlib.Path(__file__).resolve().parents[1] / '.env'
settings = [
    ('POSTGRES_PASSWORD', lambda: secrets.token_hex(32)),
    ('APP_DB_PASSWORD', lambda: secrets.token_hex(32)),
    # The login role (migration 00040): the only database login that can read a password hash.
    ('AUTH_DB_PASSWORD', lambda: secrets.token_hex(32)),
    ('JWT_SIGNING_KEY', lambda: base64.b64encode(secrets.token_bytes(48)).decode()),
    ('DEPLOYMENT_MODE', lambda: 'onprem'),
    ('APP_ORIGIN', lambda: 'http://localhost:3000'),
    ('POSTGRES_PORT', lambda: '55432'),
    ('API_PORT', lambda: '8080'),
]
if sys.argv[1:] == ['--add-missing'] and path.exists():
    present = {line.split('=', 1)[0] for line in path.read_text().splitlines() if '=' in line}
    missing = [(k, g) for k, g in settings if k not in present]
    if missing:
        text = path.read_text()
        with path.open('a') as output:
            output.write(('' if text.endswith('\n') or not text else '\n') + ''.join(k + '=' + g() + '\n' for k, g in missing))
    print('Added ' + (', '.join(k for k, _ in missing) or 'nothing') + ' to .env. No credentials were printed.')
    raise SystemExit(0)
config = ''.join(k + '=' + g() + '\n' for k, g in settings)
fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, 'w') as output:
    output.write(config)
print('Created local .env with private permissions. No credentials were printed.')
