# Floor plans: buildings, floors, zones, 2D and 3D

Added 2026-09-20. The "ผังอาคาร" page is a drawing tool for the places where gateways and devices live. It shows live status on the plan and follows roaming wearables between zones.

## Model

- A **site** is a building. It can belong to a project. A site has floors.
- A **floor** has a level, a size in metres, a ceiling height and one drawing (`layout`): walls, zones and fixtures. All coordinates are metres from the floor's top-left corner, y pointing down.
- A **zone** is a named polygon with a kind (room, ward, corridor, restricted, storage, outdoor, other), a colour and the gateways that cover it.
- A **placement** puts a gateway or a registered device at x, y and a mounting height z. An asset stands in one place: placing it on another floor moves it.
- A floor can carry one scanned plan to trace over. The browser shrinks it to at most 170 KB before upload because the API accepts 256 KiB per request. The server detects the image type from the bytes.
- Saving sends metadata, drawing and placements together with the `revision` the editor loaded. A stale revision returns 409, so two people cannot overwrite each other silently. When a save takes an asset from another floor, that floor's revision is bumped too and returned as `bumped`, so a stale draft of it cannot put the asset back.

Tables are in migration `00015`: `core.sites`, `core.floors`, `core.floor_placements`, `core.floor_images`, all tenant-scoped under FORCE RLS. Limits: 100 sites, 60 floors per site, 500 walls, 200 zones, 500 fixtures and 500 placements per floor, 400 KB of drawing per floor.

## The editor

The chrome follows the lean pattern of the connect page: one 42 px toolbar, one 34 px row of floor chips, collapsible side panels, and messages that float over the stage.

| Tool | Key | What it does |
| --- | --- | --- |
| เลือก / ย้าย | V | move shapes, drag square handles to reshape, double-click an edge to add a point, Alt-click a point to remove it, Delete removes |
| เลื่อนผัง | H or Space | pan; the wheel zooms around the pointer |
| ผนัง | W | click point by point, Shift locks to horizontal or vertical, Enter or double-click ends |
| ห้องสี่เหลี่ยม | R | drag a rectangular zone |
| โซนหลายเหลี่ยม | Z | click corners, click the first point or Enter to close |
| วางของ | I | doors, windows, stairs, lifts, exits, beds, desks, racks, extinguishers, text labels |
| วัดระยะ | M | two clicks give a distance; use it to calibrate a scanned plan |

Also: undo and redo per floor (80 steps), snap from free to 1 m, a 1 m grid, a 10 m planning ring around gateways, fit to screen, Ctrl/⌘ S to save, 2 and 3 to switch views. Two buttons bring an existing plan in: one drops a scanned image behind the drawing to trace over, the other imports a CAD file (see below). Gateways and fixed devices are dragged from the left panel onto the plan or placed with a click.

## CAD import (DXF)

The toolbar's compass button ("นำเข้า CAD (DXF)") reads a `.dxf` file and turns it into walls, zones and text labels. Parsing happens entirely in the browser — nothing is uploaded, the server only ever receives the normal floor save.

**Supported.** ASCII DXF of any version, which every CAD tool can export: AutoCAD, BricsCAD, LibreCAD, SketchUp and Revit all have it under Save As / Export. From `HEADER` it reads `$INSUNITS` (in, ft, mm, cm, m) and `$EXTMIN` / `$EXTMAX`; from `TABLES` the layer names; from `ENTITIES` and `BLOCKS`: `LINE`, `LWPOLYLINE` (closed flag and bulges — arcs are approximated by segments), `POLYLINE` + `VERTEX` + `SEQEND`, `ARC`, `CIRCLE`, `ELLIPSE`, `SPLINE` (through its fit or control points), `INSERT` (blocks are expanded with their scale, rotation, position and column/row arrays, nested up to 4 deep), `TEXT` and `MTEXT` (formatting codes are stripped). Everything else — `HATCH`, `DIMENSION`, `3DSOLID`, meshes — is ignored and counted, and the dialog says how much it skipped.

**Not supported.** DWG is AutoCAD's closed binary format with no published specification, so it cannot be read in a browser: Save As / Export it to DXF first. Binary DXF (the file starts with `AutoCAD Binary DXF`) is rejected with the same advice. IFC and RVT are BIM models, not drawings, and are out of scope. 3D geometry is read as its x/y projection only; FLATTEN the drawing first if it has height.

**What the dialog does.** It shows the file's unit with a unit picker (mm/cm/m/in/ft) and the resulting size in metres updating live — a unitless file gets a guess from the drawing's size, so check it. Every layer is listed with its entity counts and a target: ผนัง (walls), โซน (zones from closed polylines) or ข้าม (skip). Layers whose names look like annotation — DIM, ANNO, HATCH, TEXT, FURN, DEFPOINTS, NOTE, GRID, TITLE — start switched off; layers that look like rooms — ROOM, ZONE, AREA, SPACE, ห้อง, โซน — default to zones, and a zone takes its name from the text sitting inside it. An SVG preview shows exactly what will land on the floor. Importing is a single undo step, fits the view and leaves the floor dirty until you save.

**Clean-up on the way in.** Coordinates are converted to metres, the drawing's bounding box is moved so it starts 1 m from the floor's origin, and y is flipped (DXF's y grows up, the floor plan's grows down). Line segments that share an endpoint within 1 mm are chained into polylines, zero-length and duplicate segments are dropped, curves are thinned with Douglas–Peucker at a tolerance you can change (2 cm by default) and everything is rounded to the centimetre. Two options finish the job: "ปรับขนาดชั้นให้พอดีกับแบบ" sets the floor's width and depth to the drawing plus a 1 m margin, and you choose between replacing the floor's existing walls and adding to them.

**Limits.** A file may be up to 25 MB and 400,000 entities; a 10 MB file reads in well under a second and the reader yields to the browser as it goes, with a progress bar and a cancel button. The result must fit the API: 500 walls of 2–200 points each, 200 zones of 3–100 points, 500 fixtures and about 200 KB of drawing. When it does not fit, the import button is disabled and the dialog offers a stronger simplification and a minimum segment length — nothing is ever silently truncated.

**Tips for a clean import.** Export in metres or millimetres. PURGE unused layers and blocks, and FLATTEN the drawing to 2D first. Keep walls on their own layer, room outlines as closed polylines on a layer whose name contains ROOM, and room names as text inside those outlines. Explode anything you do not want treated as a block. `docs/platform/samples/ward-floor.dxf` is a small worked example: a 30 × 20 m ward in millimetres with an outer shell, a corridor wall, four rooms and a bed block, which imports to 8 walls, 4 zones and 1 label.

## 3D view

three.js renders every floor of the building stacked by level: slabs, extruded walls at ceiling height, translucent zones, fixtures as simple volumes, gateways and devices at their mounting height with a stem to the floor. A slider spreads the floors apart and a toggle isolates the active floor. Drag to orbit, wheel to zoom, right-drag to pan. Devices of the active floor can be dragged on the floor plane. Only the dragged marker moves during the drag and the draft changes once on release, as one undo step shared with the 2D view. A device of the active floor can be picked through the translucent floors above it. Clicking another floor activates it.

## Live data and wearables

Markers take their colour from live status: mint when data arrived in the last 60 s, amber when silent, coral when a tamper or leak flag is up. Fixed sensors show their latest values under the name.

Wearables are never pinned to the plan. A roaming wearable is drawn next to the gateway of its current zone, with a pulsing ring, in both views. Zones linked to that gateway light up and show a head count. The zone comes from the server (smoothed RSSI with hysteresis, see [wearables-roaming.md](wearables-roaming.md)), so the plan does not flicker. RSSI tells which gateway is nearest, not a coordinate: the marker's position around the gateway carries no meaning.

Updates arrive through the WebSocket signals. A zone change is a device event, which triggers a refetch. A 20 s poll is the safety net.

## API

`GET /sites`, `POST /sites`, `GET /sites/{id}`, `POST /sites/{id}/update`, `POST /sites/{id}/archive`, `POST /sites/{id}/floors`, `POST /floors/{id}/save`, `POST /floors/{id}/delete`, `POST /floors/{id}/image`, `GET /floors/{id}/image`. Mutations need owner or admin. See `backend/api/openapi.json`.

## Limits

- Walls are centre lines with a thickness. Doors and windows are markers and do not cut openings in the 3D walls.
- No indoor positioning. Wearables resolve to a gateway's zone only.
- The 10 m ring is a planning aid, not a measured coverage map.
- CAD import reads DXF only, and only its 2D geometry. No DWG, IFC or RVT, no wall thickness, no doors or windows as openings.
- Project access is enforced by RLS: restricted admin/operator/viewer members see only buildings in their assigned projects; unassigned buildings are hidden from them. Owners retain workspace-wide access. See [Team access](team-access.md).

## Project-specific plans (2026-09-22)

Select a project before opening or creating a building. Each project owns its buildings, floors, drawings and background images. Switching projects clears the previous drawing and selects a building from the new project; a project without buildings starts empty. Saves reject placements and zone gateways belonging to a different project. Legacy unassigned buildings remain in a separate selection.
