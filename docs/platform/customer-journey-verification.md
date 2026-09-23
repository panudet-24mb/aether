# Customer journey verification — 2026-09-20

Verified the current code after the topology, project access, alert and automation additions. This is API/database/sandbox verification, not a browser acceptance test or a production deployment approval.

## Results

- `python3 infra/backend-local.py test`: passed the full Go race suite, including the PostgreSQL integration tests (162.453 seconds for `backend/tests`). It used `aether_test`, not the live database. Existing tests cover project restrictions, password changes, cross-tenant access, MQTT enrollment/rotation, automation and notifications sent only to a local test webhook. Host-only runs skip QuickJS widget tests when the module is unavailable.
- `npm run build` in `frontend/`: passed. Vite reports chunks above 500 kB; performance on a tablet has not been measured.
- `python3 infra/test-customer-journey.py`: passed the new combined journey using compiled current Go code and the Linux QuickJS sandbox in `aether-backend:local`. The script mounts current migrations and sandbox code read-only and passes database credentials through environment variables without displaying them.
- `python3 infra/test-customer-journey.py --mqtt`: passed (9.50 seconds in the test binary). Disposable Mosquitto 2.0.22, the current compiled provisioner and collector used only `aether_test`. No host ports were published. Gateway traffic used TCP 1883; the collector connected over TLS 8883 with certificate verification. All temporary containers were removed afterwards.

The MQTT run verified successful and incorrect-password connections, own-topic publishing, denied status delivery after a wildcard subscription, denied cross-gateway publishing, asynchronous collection, the same Dashboard/Alert checks, password rotation and refusal of new connections after revocation. Rotation is tested on **new connections**, not forced termination of existing sessions.

## Combined journey

`backend/tests/customer_journey_test.go` creates isolated test tenants and follows the HTTP API:

1. Create a project and a viewer restricted to that project; enforce changing the initial password.
2. Create an MG3 gateway in that project and issue MQTT credentials. Refuse credential rotation from another tenant.
3. Submit a simulated Minew packet through authenticated **HTTP ingress**, then find the source in Studio discovery.
4. Save a dashboard with the official environment widget and confirm it appears in the persisted catalog.
5. Decode the archived bytes and render the real widget in QuickJS, checking `25.5` and the simulated-data marker.
6. Produce a temperature-threshold alert, visible to the project viewer along with live readings.
7. Refuse another tenant's attempt to render that source, and keep the alert out of their list.

Studio remains owner/admin-only: a viewer can read Live and Alerts but receives 403 from Studio rendering. The test preserves and verifies that current policy; it does not add viewer access to dashboards.

## Repeat

With the local PostgreSQL stack and `aether-backend:local` image available:

```sh
python3 infra/backend-local.py test
python3 infra/test-customer-journey.py
python3 infra/test-customer-journey.py --mqtt
```

The dedicated journey runner requires Docker and Go, uses the `aether_default` network, and creates a temporary Linux test binary removed after the run. MQTT mode also needs OpenSSL, the existing Mosquitto image and the provisioner credentials prepared in `.env`; run the full integration suite first to apply test migrations. It generates temporary credentials/certificates and removes its containers and files on exit. Test fixtures stay in `aether_test`, consistent with the existing integration suite. Linux is required for the production sandbox's resource limits; do not weaken those limits to run this test natively on macOS.

## Remaining acceptance checks

- Physical MG3 connectivity and gateway-side TLS remain unverified in this run. The disposable broker test does not change the user's live broker configuration.
- Browser interactions, drag/resize, tablet/kiosk layout and session-expiry UX. The live frontend returned HTTP 200 and backend readiness passed, but the selected browser tab was logged out; authenticated browser checks are pending user sign-in.
- Real Minew models beyond the captured S1 frames; see the hardware bring-up guide.
- Public signup/email verification and production backup/restore rehearsal are separate work.

No live-owner credentials were used and no notification was sent to a real recipient in this verification.
