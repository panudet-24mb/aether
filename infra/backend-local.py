#!/usr/bin/env python3
"""Run backend commands with local generated credentials; never prints secrets."""
import os
import pathlib
import subprocess
import sys
root = pathlib.Path(__file__).resolve().parents[1]
config = dict(line.split('=', 1) for line in (root / '.env').read_text().splitlines() if line and not line.startswith('#'))
env = dict(os.environ)
env.update(config)
port = config.get('POSTGRES_PORT', '55432')
env['DATABASE_URL'] = f"postgresql://aether_app:{config['APP_DB_PASSWORD']}@127.0.0.1:{port}/aether?sslmode=disable"
env['MIGRATION_DATABASE_URL'] = f"postgresql://postgres:{config['POSTGRES_PASSWORD']}@127.0.0.1:{port}/aether?sslmode=disable"
env['TEST_DATABASE_URL'] = f"postgresql://aether_app:{config['APP_DB_PASSWORD']}@127.0.0.1:{port}/aether_test?sslmode=disable"
env['TEST_ADMIN_DATABASE_URL'] = f"postgresql://postgres:{config['POSTGRES_PASSWORD']}@127.0.0.1:{port}/aether_test?sslmode=disable"
env['APP_ENV'] = 'development'
env['ALLOW_REGISTRATION'] = 'false'
env['LISTEN_ADDR'] = '127.0.0.1:' + config.get('API_PORT', '8080')
commands = {
    'migrate': ['go', 'run', './cmd/migrate'],
    'api': ['go', 'run', './cmd/api'],
    'test': ['go', 'test', '-race', '-count=1', './...'],
    'bootstrap': ['go', 'run', './cmd/admin', 'bootstrap'],
}
if len(sys.argv) != 2 or sys.argv[1] not in commands:
    raise SystemExit('Usage: backend-local.py migrate|api|test|bootstrap')
# The API must never inherit migration or test-admin credentials.
if sys.argv[1] in ('api', 'bootstrap'):
    env.pop('MIGRATION_DATABASE_URL', None)
    env.pop('TEST_ADMIN_DATABASE_URL', None)
    env.pop('POSTGRES_PASSWORD', None)
result = subprocess.run(commands[sys.argv[1]], cwd=root / 'backend', env=env)
raise SystemExit(result.returncode)
