> Current implementation: `/` is the authenticated live Minew dashboard; `/demo` preserves the original simulated spatial prototype. Live data comes from the Go backend, not Sites storage. See `../docs/live-dashboard.md`. Local API origin is configured in `.env.local`; no credentials are included in the client.

# Aether UI prototype

Interactive IoT workspace prototype based on `../requirement.md`.

## Run locally

```sh
npm ci
npm run dev
```

Open the Local URL printed by the server. Node 22.13+ is required.

## Included

- Overview with derived device counts, connectivity, sample telemetry and alerts.
- Devices: search, status filters, details, and CSV export.
- Three-step onboarding that adds a simulated device to the current session.
- Spatial view: 2D floor plan / isometric building diagram, floor selection, zoom, floor spacing, temperature overlays and clickable devices.
- Alert acknowledgement and resolution within the current session.
- English / Thai interface and a Cloud / On-premise deployment-label preview.
- Responsive sidebar and accessible Radix/shadcn dialogs, tabs, table and slider.

## Boundaries

This is a UI prototype, not a production IoT backend. All devices, readings and alerts are simulated. Device and alert changes reset when the page reloads; only the language preference is stored locally. No credentials are collected and no real notifications are sent.

The building view is an SVG isometric data diagram, not a Three.js renderer. It is not yet a floor-plan drawing editor. Cloud / On-premise selection changes the preview label only; neither provisions infrastructure nor implements offline deployment.

This preview uses the Sites Vinext/Vite runtime with React 19, Next-compatible App Router, TypeScript, Tailwind and shadcn. The production requirement calls for Next.js and a Go backend; migrating this presentation layer and implementing the backend remain separate work. This prototype uses an inline translation helper; production should adopt the required next-intl namespaces.

## Verification

- `npx tsc --noEmit`
- `npm run build`
- Local HTTP readiness check.

Browser interaction / screenshot QA has not been performed. Optional WebMCP navigation is feature-detected; a supported browser registry was not available for validation.

## Next production milestone

Tenant / RLS and authentication, then Generic MQTT ingestion → stored telemetry → WebSocket → device overview. Use one backend codebase with Cloud and On-premise configurations. Agree on offline operation and an upgrade/backup strategy before implementing deployment.
