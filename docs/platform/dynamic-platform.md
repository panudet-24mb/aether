# Aether: configurable IoT workspace

User direction confirmed 2026-09-12. This document defines the next product scope; it does not claim these features are implemented.

## Customer journey

1. Cloud customer registers and verifies email, creates a workspace; on-premise owner provisions the first workspace locally. Both use the same services and data model.
2. Add gateway → choose protocol → receive endpoint, gateway-scoped credentials and exact publish/subscribe topics → see connection and first packet status. Customer never edits broker files.
3. Discover advertisements through the gateway. Customer explicitly adopts devices into their workspace and assigns a versioned device profile. Nearby BLE devices are not automatically enrolled.
4. Choose a dashboard preset or create a blank dashboard. Add and resize panels, bind each to devices/metrics, choose visual templates, save and publish a revision.
5. Create buildings, floors and a 2D plan; draw rooms/zones, import a bounded background image, place enrolled devices. Dashboard panels can reference that floor plan. 3D views derive from this model rather than a separate drawing.
6. Create a kiosk view for a wall-mounted browser display. Pair the screen with a short-lived code, then grant a revocable read-only session to selected dashboards. A copied URL must not grant account administration.
7. Install templates from a curated catalog; later submit community packages with a license, sample inputs and expected outputs. Workspace installs pin exact versions.

## Separate the reusable parts

| Object | Responsibility |
|---|---|
| Device profile | Brand/model, capabilities, metrics, commands, units and protocol requirements |
| Decoder version | Convert raw protocol payload into canonical telemetry/events; immutable version |
| Widget template | Allowed visual components, field bindings, formatting and interaction options |
| Dashboard | Tenant-owned saved arrangement of panel instances with a published revision |
| Panel | Widget version + selected device/metric + layout + options; many panels per dashboard |
| Floor plan | Building/floor, geometry, zones and device positions; same asset referenced by panels |
| Catalog package | Profile, compatible decoder/widget versions, examples, author, license and review status |
| Display session | Device-bound, expiring/revocable permission for selected published dashboards |

A Minew button's press event is distinct from a controllable relay. Profiles must declare the capabilities actually supported. A UI toggle cannot manufacture a command capability. Commands require their own authorized path, expiry, correlation ID and observed acknowledgement/state; never optimistically report hardware success.

## Schema-driven first

Start with validated JSON definitions for visual widgets and field decoders. No uploaded arbitrary HTML, SVG scripts, JavaScript eval, SQL or network URLs in templates. Offer numeric value, gauge, time series, state indicator, event list and floor-plan widgets with controlled colors/icons/layout.

Declarative decoder operations cover exact frame selectors, hex byte offsets, endianness, signed numbers, scaling, bit fields and field renaming. Each version contains positive/negative fixtures and resource bounds. Testing a decoder uses a captured or simulated packet and shows raw → decoded → validated output without updating live devices.

For protocols that require code, add an isolated worker/WASM runtime later with explicit memory/time/output budgets and no filesystem/network/credentials. A published decoder is pinned; changes do not silently affect existing installations.

## Tenant and publication boundaries

All customer dashboards, panels, plans, installations, display sessions and adopted devices have tenant_id and FORCE RLS. Private catalog drafts belong to a tenant; public approved catalog versions contain no tenant telemetry or credentials. Forking a community package creates a new version in the customer's namespace.

Saving a draft and publishing a revision are separate actions. Use optimistic concurrency to prevent two editors silently overwriting a dashboard. Deleting a template version that is in use is prohibited; deprecation does not remove existing installs. Imported packages have schema/size limits and migration rules.

Kiosk sessions expose selected readings and layouts only. Revoke, rotate and expire sessions; do not put an owner JWT or gateway secret in a URL. Default view-only. If the selected display supports a modern browser, fullscreen web rendering is suitable; the exact hardware capabilities in the supplied image have not been verified.

## Work sequence and acceptance criteria

### 1. Development without hardware

Dedicated simulated gateway identity, named SIM, publishes MG3-compatible advertisements through the normal broker → storage → decoder → UI path. Deterministic virtual devices support normal data and a stop/offline check. Simulation never impersonates the physical MG3 and never sends hardware commands. Restore fixtures without production credentials.

### 2. Persisted telemetry and dynamic profiles

Move beyond the current 20-packet diagnostic view. Store canonical telemetry with provenance (physical/simulated), profile/decoder version, tenant/device identity and deduplication key. Query bounded time ranges and aggregates. Test isolation, duplicate/reordered events, decoder failures and retention. Device discovery/adoption and multiple gateway credentials are required here.

### 3. Customer enrollment

Verified signup with abuse/rate controls and invitations, gateway onboarding UI, dynamic broker authentication/ACL and credential revocation for active connections. Existing static Mosquitto password files are a development bridge, not the customer onboarding design. Keep broker adapter replaceable; evaluate the originally specified EMQX HTTP auth/authorization against deployment/license requirements at implementation time.

### 4. Dashboard Studio

Multiple persisted dashboards and panels; create/edit/duplicate/delete, drag/resize, bind metrics, title/units/thresholds, save/reload and draft/publish. Numeric/state/chart widgets first. Acceptance includes two workspaces seeing only their dashboards, two editors detecting conflicts, unsupported bindings showing a useful error, and stale data marked stale.

### 5. Space Studio and display links

2D editor with floors/rooms/zones/device placement, saved and reloadable; dashboard floor-plan panel; fullscreen responsive kiosk renderer and revocable display pairing. Initially read-only displays. Add 3D after the 2D spatial model and coordinate system are stable.

### 6. Template Studio and catalog

Author device/widget/decoder templates, run test fixtures, version and install within a workspace. Then curated built-in catalog → community submission/review → publication. Import/export is not the same as an operational public marketplace.

## Current implementation boundary

Implemented before this expansion: tenant-aware auth/backend, MG3 ingestion, limited Minew temperature/humidity live projection and authenticated local dashboard. The spatial demo is simulated. Self-service cloud signup, dynamic broker provisioning, persisted panel/floor-plan editors, kiosk pairing and marketplace remain work to implement. Do not label the current app a completed SaaS platform.
