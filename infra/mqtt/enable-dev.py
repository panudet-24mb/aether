#!/usr/bin/env python3
"""Explicitly enable authenticated plaintext MQTT on the local dev broker."""
import pathlib
root = pathlib.Path(__file__).resolve().parents[2]
p = root / '.secrets/mqtt/broker/mosquitto.conf'
s = p.read_text()
if 'listener 1883\n' not in s:
    p.chmod(0o600)
    p.write_text(s + '\n# Explicit dev opt-in. Global password/ACL settings also apply here.\nlistener 1883\nmax_connections 20\n')
    p.chmod(0o444)
print('Dev listener configured; start with compose.dev.yaml to publish LAN port 1883.')
