# Aether MQTT — MG3 installation

## Current hardware connection (2026-09-21)

The Mac currently uses `192.168.1.144`; the physical MG3 (`ac233fc274eb`) was found at `192.168.1.152`. MQTT ports 1883 and 8883 are bound to the current Mac address. The server certificate covers that address and retains the existing CA and `mqtt` DNS name.

Use the canonical topics displayed by the web UI for gateway `a831ad42-cb50-4dc1-9e6f-dc7ba869221a`:

- Publish: `/aether/gateways/a831ad42-cb50-4dc1-9e6f-dc7ba869221a/status`
- Subscribe: `/aether/gateways/a831ad42-cb50-4dc1-9e6f-dc7ba869221a/action`
- Reply: `/aether/gateways/a831ad42-cb50-4dc1-9e6f-dc7ba869221a/response`

The physical gateway still authenticates with its legacy `mg3-ac233fc274eb` account. Its base ACL now also permits these exact three topics for this gateway only; the provisioning worker carries those rules into the runtime ACL. The web UI's `gw-<gateway-id>` account is a separate credential for the same gateway. Do not mix one account's username with the other account's password. The old `/mg3/...` topics remain compatible, but are not the values to copy from the current web UI.

The MG3 configuration API can return success without persisting changes; always read settings back. On this device topic changes through that API did not persist. Historical IPs and initial setup instructions below are retained for reference.

## Current installation (2026-09-11)

- Broker: Mosquitto 2.0.22, TLS at **192.168.1.64:8883** and explicit dev plaintext at **192.168.1.64:1883**, bound to this LAN IP only.
- Gateway: MG3 `mg3-a-lychee`, firmware `v1.4.4`, IP `192.168.1.77`.
- Owner provisioned with the user's supplied email; password in `.secrets/owner-password.txt`. Login metadata in `.secrets/owner.json`. Keep these private.
- Gateway credentials and complete MQTT fields: `.secrets/mqtt/gateway-settings.json` (private).
- Public CA: `.secrets/mqtt/broker/ca.crt`. The documented upload endpoint acknowledged this CA upload; actual hardware TLS validation remains unverified.
- Original gateway configuration: `.secrets/mqtt/original-gateway-config.json` (private, can contain Wi-Fi credentials).
- **Hardware cutover verified (2026-09-11 15:35 Bangkok):** after iOS configuration, MG3 connected to `mqtt://192.168.1.64:1883`, SSL off, QoS 1. Broker confirmed the physical gateway client ID; the authenticated packet API increased from 13 to 15 packets across an 8-second observation, with non-synthetic array payloads. S1 decoding remains pending.

## iOS settings

Select MQTT Config and a TLS scheme (the API calls this `mqtts://`). Host `192.168.1.64`, port `8883`. Username `mg3-ac233fc274eb`; copy password from the private settings file locally. Do not paste it into chat.

| Field | Value |
|---|---|
| Client ID | `ac233fc274eb` |
| Post topic | `/mg3/ac233fc274eb/status` |
| Subscribe topic | `/mg3/ac233fc274eb/action` |
| Reply topic | `/mg3/ac233fc274eb/response` |
| QoS | 1 |
| Keep Alive | 120 seconds |

Use the Aether CA via Upload Certificate if requested. Keep scan/filter settings as they are for the first capture. The broker does not require a client certificate. Do not disable server certificate verification. This Mac and Docker must stay running. Reserve `192.168.1.64` in DHCP before longer-term use; a changed IP needs a new certificate and gateway endpoint. This is a local installation, not an externally hosted cloud endpoint.

## Operations

From the repository root:

```sh
docker compose --env-file .env -f infra/compose.yaml -f infra/mqtt/compose.yaml up -d --build mqtt mqtt-ingest
python3 infra/mqtt/check.py
```

The second command performs a clearly marked synthetic test: trusted TLS connection, rejected wrong password, accepted own-topic publish, rejected cross-gateway publish, and confirmation through the authenticated backend packet API. It does not prove physical gateway delivery.

`infra/mqtt/provision.py --email ADDRESS --host LAN_IP` is the installation-specific first-MG3 provisioner. It preserves existing credentials/certificates and refuses to overwrite MQTT configuration. For another installation supply its own owner and IP, certificates and secrets; this helper's MG3 MAC/topics must be adapted before provisioning another device. It is not a general multi-gateway enrollment UI.

## Security and delivery behavior

- Anonymous access disabled. Gateway account may publish only its status/response and read only its action topic. Collector may read only this gateway's status; it has no command publish access.
- Collector validates the broker CA/hostname and requires TLS 1.2 or newer. Broker has packet, queue, connection and memory bounds, persistent volume, plaintext only via explicit dev opt-in, non-root UID and dropped capabilities.
- Exact topic-to-gateway mapping is server-owned. Each message revalidates the registered gateway credential and tenant activity through the existing backend before a tenant-scoped database transaction. Payload tenant IDs are never used for routing.
- Backend gateway revocation immediately prevents storage of new packets. **Broker login revocation is separate:** remove its broker password/ACL and restart the broker to disconnect existing sessions. Static broker credentials are not automatically synchronized to the database.
- Persistent MQTT subscriber, QoS 1 subscription, manual ACK after database commit. Transient database failures restart the worker without ACK. Invalid/revoked packets are rejected and acknowledged so they do not loop forever. QoS 1 permits duplicate raw packets. Broker persistence flushes every 30 seconds; this is not a zero-loss guarantee during host failure.
- Capture retains the latest 100 diagnostic packets per gateway. **No verified S1 decoder or permanent sensor history from MQTT yet.** UI remains demo data. Logging records packet IDs, never payloads or passwords.
- Local gateway management API itself uses HTTP as documented by Minew; provision only on a trusted setup LAN. The telemetry connection uses TLS. Cloud production requires stable DNS/certificates, production database TLS, backup/monitoring and credential lifecycle management; use the same backend image with production configuration.

## Validation

Backend race tests including MQTT routing/revocation and fail-closed configuration passed. Live broker tests passed wrong password, cross-gateway publish denial and synthetic packet → PostgreSQL → authenticated API. Physical MG3 connection and raw packet storage verified after iOS dev cutover.

References: [Mosquitto configuration](https://mosquitto.org/man/mosquitto-conf-5.html), [Minew MG3 configuration API](https://docs.minew.com/iOS/esp32c3_wifi_gw_http_api_user.html).

## Dev without SSL (explicit user opt-in)

Enabled on this installation. In iOS use `tcp://`, host `192.168.1.64`, port `1883`, SSL/certificate off. Keep username/password/client ID/topics unchanged. The gateway was changed to port 1883 in iOS and hardware packet delivery is confirmed.

```sh
python3 infra/mqtt/enable-dev.py
docker compose --env-file .env -f infra/compose.yaml -f infra/mqtt/compose.yaml -f infra/mqtt/compose.dev.yaml up -d mqtt
python3 infra/mqtt/check.py --plaintext
```

Authentication failure, cross-gateway publish denial and synthetic packet storage passed on both listeners after enabling dev mode. The collector still connects over TLS. Plaintext credentials/data are visible on the network; use only the trusted development LAN without router port forwarding. Remove the dev override and its `listener 1883` stanza to disable it. Production uses TLS by default; gateways without TLS can use an explicit `--mqtt-plaintext` listener described in [production.md §4.5](production.md).

## Live dashboard update

Temperature/humidity decoding is now available as a read projection in the authenticated [live dashboard](live-dashboard.md). MQTT raw storage still reports decoded:false; permanent canonical sensor history remains pending.
