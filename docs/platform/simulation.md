# Development without physical hardware

> Update 2026-09-20: the simulator now publishes the whole MHS kit (four S1-style sensors plus C10, B7, B10, E8S and MBT01 scenarios). See [Minew kit](minew-kit.md). The gateway name below is historical.

Four virtual Minew temperature/humidity sensors publish changing, deterministic MG3-compatible BLE advertisements every five seconds. A dedicated `SIM · Virtual MG3 · 4 environmental sensors` gateway has its own backend token and MQTT username/topic. The physical gateway configuration is untouched.

Each generated observation carries `aether_source: simulated`; the live API reports `source: simulated` and labels the sensor SIM. This label is diagnostic provenance, not a trusted authorization claim. Future canonical storage must derive provenance from registered source configuration. The simulator does not emulate physical button/relay commands or prove compatibility with other Minew models.

## Run

First-time setup uses the existing owner credentials privately on this machine:

```sh
python3 infra/mqtt/provision-simulator.py
docker restart aether-mqtt-1 aether-mqtt-ingest-1
docker compose --env-file .env -f infra/compose.yaml -f infra/mqtt/compose.yaml -f infra/mqtt/compose.simulation.yaml build simulator
docker compose --env-file .env -f infra/compose.yaml -f infra/mqtt/compose.yaml -f infra/mqtt/compose.simulation.yaml up -d --no-deps api simulator
```

The simulator talks to `mqtt:8883` inside Docker, with CA/hostname verification. No connection to the office gateway or office LAN is needed. If restarting the broker on a different network, bind its host port to localhost using `MQTT_BIND_IP=127.0.0.1` when starting Compose; internal clients continue using `mqtt`.

Open http://localhost:3001/ and choose a sensor with an `F0:00:00:00:00:xx` identifier. The selected sensor name says SIM. Existing physical sensors may still appear while their last packets remain in the diagnostic window.

To test stale readings, stop only the simulator:

```sh
docker compose --env-file .env -f infra/compose.yaml -f infra/mqtt/compose.yaml -f infra/mqtt/compose.simulation.yaml stop simulator
```

Wait more than 60 seconds; Dashboard should mark those readings stale. Start the simulator again to resume. Stopping does not revoke credentials or delete saved observations. The service does not automatically start on Docker restart (`restart: no`).

## Acceptance

The generator's frames are tested against the actual Minew decoder. Runtime verification checks the authenticated live API for four simulated sensors, marked SIM, with receive timestamps advancing over two samples. Dashboard editing, marketplace and device command support are separate milestones in [the platform plan](dynamic-platform.md).

Verified 2026-09-12: live API returned four SIM sensors at 09:36:05Z and 09:36:10Z with changing measurements. No physical gateway was contacted. Focused simulator/decoder tests passed.
