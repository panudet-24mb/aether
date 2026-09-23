/**
 * Self-test for the DXF reader. There is no JS test runner in this repo, so this file is a plain
 * module: `runDxfSelfTest()` returns the list of failures (empty means everything passed).
 *
 * Run it with the repo's own TypeScript, from `frontend/`:
 *   ./node_modules/.bin/tsc app/floorplan/dxf.ts app/floorplan/dxf.selftest.ts \
 *     --outDir /tmp/dxftest --module commonjs --target es2020 --skipLibCheck --strict
 *   node -e "require('/tmp/dxftest/dxf.selftest.js').runDxfSelfTest().then(f => { console.log(f.length ? f.join('\n') : 'PASS'); process.exit(f.length ? 1 : 0); })"
 */
import { buildImport, cleanText, isBinaryDxf, parseDxf, simplify, suggestTarget, type BuildOptions, type DxfDoc, type LayerTarget } from "./dxf";

const dxf = (...pairs: (string | number)[]): string => {
  const out: string[] = [];
  for (let i = 0; i < pairs.length; i += 2) out.push(String(pairs[i]), String(pairs[i + 1]));
  return out.join("\r\n") + "\r\n";
};
const header = (insunits: number) => dxf(0, "SECTION", 2, "HEADER", 9, "$INSUNITS", 70, insunits, 0, "ENDSEC");
const entities = (body: string) => dxf(0, "SECTION", 2, "ENTITIES") + body + dxf(0, "ENDSEC", 0, "EOF");
const line = (layer: string, x0: number, y0: number, x1: number, y1: number) => dxf(0, "LINE", 8, layer, 10, x0, 20, y0, 30, 0, 11, x1, 21, y1, 31, 0);
const NUL = String.fromCharCode(0);

/* ------------------------------------------------------------ the samples */

/** Four LINEs given out of order and back to front: the importer must chain them into one closed wall. */
const SAMPLE_CHAIN = header(6) + entities(
  line("WALLS", 0, 0, 10, 0) + line("WALLS", 10, 6, 0, 6) + line("WALLS", 10, 0, 10, 6) + line("WALLS", 0, 6, 0, 0),
);

/** A closed LWPOLYLINE where the (4,0) to (4,4) side is a half circle bulging to +x. */
const SAMPLE_BULGE = header(6) + entities(dxf(
  0, "LWPOLYLINE", 8, "WALLS", 90, 3, 70, 1,
  10, 0, 20, 0, 42, 0,
  10, 4, 20, 0, 42, 1,
  10, 4, 20, 4,
));

/** A 1x1 square block INSERTed at (10,5), rotated 90 degrees and scaled x2. */
const SAMPLE_INSERT = header(6)
  + dxf(0, "SECTION", 2, "BLOCKS", 0, "BLOCK", 2, "UNIT", 10, 0, 20, 0)
  + line("0", 0, 0, 1, 0) + line("0", 1, 0, 1, 1) + line("0", 1, 1, 0, 1) + line("0", 0, 1, 0, 0)
  + dxf(0, "ENDBLK", 0, "ENDSEC")
  + entities(dxf(0, "INSERT", 8, "WALLS", 2, "UNIT", 10, 10, 20, 5, 41, 2, 42, 2, 50, 90));

/** Quarter arc, centre (0,0), r = 5, from 0 to 90 degrees. */
const SAMPLE_ARC = header(6) + entities(dxf(0, "ARC", 8, "WALLS", 10, 0, 20, 0, 40, 5, 50, 0, 51, 90));

/** Millimetres, drawn away from the origin and below the x axis to exercise the move and the y flip. */
const SAMPLE_MM = header(4) + entities(
  line("WALLS", 5000, -3000, 35000, -3000) + line("WALLS", 35000, -3000, 35000, -23000),
);

/** A room on a ROOMS layer named by an MTEXT full of formatting codes, plus a DIM layer to skip. */
const SAMPLE_ROOMS = header(6) + entities(
  dxf(0, "LWPOLYLINE", 8, "ROOMS", 90, 4, 70, 1, 10, 0, 20, 0, 10, 6, 20, 0, 10, 6, 20, 4, 10, 0, 20, 4)
  + dxf(0, "MTEXT", 8, "ROOMS", 10, 3, 20, 2, 40, 0.3, 1, "{\\fArial|b1|i0;\\H1.4x;ห้องพัก \\P 101}")
  + line("DIM", 0, -2, 6, -2)
  + dxf(0, "HATCH", 8, "ROOMS", 91, 0),
);

const BINARY = "AutoCAD Binary DXF\r\n" + NUL + NUL;

/* -------------------------------------------------------------- assertions */

const round = (v: number, n = 3) => Math.round(v * 10 ** n) / 10 ** n;

const allLayers = (doc: DxfDoc, target: LayerTarget): Record<string, LayerTarget> =>
  Object.fromEntries(doc.layers.map((l) => [l.name, target]));

const build = (doc: DxfDoc, targets: Record<string, LayerTarget>, over: Partial<BuildOptions> = {}) =>
  buildImport(doc, { unit: doc.unit, targets, thickness: 0.15, tolerance: 0.02, minLength: 0, margin: 1, ...over });

export async function runDxfSelfTest(): Promise<string[]> {
  const fails: string[] = [];
  const check = (name: string, ok: boolean, detail = "") => { if (!ok) fails.push(`FAIL ${name}${detail ? ` — ${detail}` : ""}`); };
  const near = (name: string, got: number, want: number, eps = 0.02) => check(name, Math.abs(got - want) <= eps, `got ${round(got)}, want ${want}`);

  // 1. LINE chain joining, y flip, 1 m margin --------------------------------
  {
    const doc = await parseDxf(SAMPLE_CHAIN);
    check("chain: unit from header", doc.unit === "m" && doc.unitFromHeader, `unit=${doc.unit}`);
    check("chain: 4 line shapes", doc.shapes.length === 4, `got ${doc.shapes.length}`);
    const r = build(doc, { WALLS: "wall" });
    check("chain: joins into one wall", r.walls.length === 1, `got ${r.walls.length} walls`);
    const w = r.walls[0];
    check("chain: closed ring of 4 points", !!w && w.closed === true && w.points.length === 4, `closed=${w?.closed} points=${w?.points.length}`);
    near("chain: width", r.width, 10);
    near("chain: depth", r.depth, 6);
    const xs = w.points.map((p) => p[0]), ys = w.points.map((p) => p[1]);
    near("chain: min x at the margin", Math.min(...xs), 1);
    near("chain: max x", Math.max(...xs), 11);
    near("chain: min y at the margin", Math.min(...ys), 1);
    near("chain: max y", Math.max(...ys), 7);
    check("chain: thickness", w.thickness === 0.15, String(w.thickness));
  }

  // 2. Closed LWPOLYLINE with a bulge ----------------------------------------
  {
    const doc = await parseDxf(SAMPLE_BULGE);
    const shape = doc.shapes[0];
    check("bulge: one closed poly", doc.shapes.length === 1 && shape.k === "poly" && shape.closed, JSON.stringify(doc.shapes.map((s) => s.k)));
    if (shape.k === "poly") {
      check("bulge: arc expanded to many points", shape.pts.length > 10, `got ${shape.pts.length}`);
      const maxX = Math.max(...shape.pts.map((p) => p[0]));
      near("bulge: half circle reaches x = 6", maxX, 6, 0.05);
      const onArc = shape.pts.filter((p) => p[0] > 4.001);
      check("bulge: arc points lie on r = 2 about (4,2)", onArc.length > 5 && onArc.every((p) => Math.abs(Math.hypot(p[0] - 4, p[1] - 2) - 2) < 0.02), `${onArc.length} arc points`);
    }
    const r = build(doc, { WALLS: "wall" }, { tolerance: 0.001 });
    check("bulge: one wall", r.walls.length === 1, `got ${r.walls.length}`);
    check("bulge: stays closed", r.walls[0]?.closed === true);
    near("bulge: width 0..6", r.width, 6, 0.05);
  }

  // 3. INSERT with rotation and scale ----------------------------------------
  {
    const doc = await parseDxf(SAMPLE_INSERT);
    check("insert: block expanded to 4 lines", doc.shapes.length === 4, `got ${doc.shapes.length}`);
    check("insert: entities inherit the INSERT's layer", doc.shapes.every((s) => s.layer === "WALLS"), doc.shapes.map((s) => s.layer).join(","));
    const pts = doc.shapes.flatMap((s) => (s.k === "poly" ? s.pts : []));
    // A unit square scaled x2 then rotated +90 degrees about the insertion point (10,5): x in [8,10], y in [5,7].
    near("insert: min x", Math.min(...pts.map((p) => p[0])), 8);
    near("insert: max x", Math.max(...pts.map((p) => p[0])), 10);
    near("insert: min y", Math.min(...pts.map((p) => p[1])), 5);
    near("insert: max y", Math.max(...pts.map((p) => p[1])), 7);
    const r = build(doc, { WALLS: "wall" });
    check("insert: joins to one closed wall", r.walls.length === 1 && r.walls[0].closed === true, `${r.walls.length} walls`);
  }

  // 4. ARC -------------------------------------------------------------------
  {
    const doc = await parseDxf(SAMPLE_ARC);
    const s = doc.shapes[0];
    check("arc: one open poly", doc.shapes.length === 1 && s.k === "poly" && !s.closed);
    if (s.k === "poly") {
      check("arc: every point at r = 5", s.pts.every((p) => Math.abs(Math.hypot(p[0], p[1]) - 5) < 1e-6), `${s.pts.length} points`);
      near("arc: starts at (5,0)", s.pts[0][0], 5, 1e-6);
      near("arc: ends at (0,5)", s.pts[s.pts.length - 1][1], 5, 1e-6);
      check("arc: sampled, not a straight line", s.pts.length >= 8, `got ${s.pts.length}`);
    }
    const r = build(doc, { WALLS: "wall" }, { tolerance: 0.02 });
    check("arc: simplify keeps a curve", !!r.walls[0] && r.walls[0].points.length >= 4 && r.walls[0].points.length < 20, `got ${r.walls[0]?.points.length}`);
  }

  // 5. mm to m conversion and the y flip -------------------------------------
  {
    const doc = await parseDxf(SAMPLE_MM);
    check("mm: $INSUNITS 4 gives mm", doc.unit === "mm" && doc.unitFromHeader, doc.unit);
    const r = build(doc, { WALLS: "wall" });
    near("mm: 30 m wide", r.width, 30);
    near("mm: 20 m deep", r.depth, 20);
    const pts = r.walls.flatMap((w) => w.points);
    near("mm: origin moved to the margin", Math.min(...pts.map((p) => p[0])), 1);
    near("mm: y flipped, top edge at the margin", Math.min(...pts.map((p) => p[1])), 1);
    near("mm: bottom edge", Math.max(...pts.map((p) => p[1])), 21);
    // The long horizontal run has the largest DXF y, so after the flip it must be the top edge.
    const top = pts.filter((p) => Math.abs(p[1] - 1) < 0.01);
    check("mm: the y = -3000 run became the top edge", top.length >= 2, `${top.length} points on the top edge`);
  }

  // 6. Rooms to zones, MTEXT cleanup, skipped layers -------------------------
  {
    const doc = await parseDxf(SAMPLE_ROOMS);
    check("rooms: HATCH counted as skipped", doc.skipped.HATCH === 1, JSON.stringify(doc.skipped));
    const text = doc.shapes.find((s) => s.k === "text");
    check("rooms: MTEXT formatting stripped", text?.k === "text" && text.text === "ห้องพัก 101", `got ${text?.k === "text" ? text.text : "none"}`);
    const rooms = doc.layers.find((l) => l.name === "ROOMS");
    check("rooms: ROOMS layer suggested as zone", !!rooms && suggestTarget(rooms) === "zone", rooms ? suggestTarget(rooms) : "no layer");
    const dim = doc.layers.find((l) => l.name === "DIM");
    check("rooms: DIM layer suggested as skip", !!dim && suggestTarget(dim) === "skip", dim ? suggestTarget(dim) : "no layer");
    const r = build(doc, { ROOMS: "zone", DIM: "skip" });
    check("rooms: one zone", r.zones.length === 1, `got ${r.zones.length}`);
    check("rooms: zone named from the text inside it", r.zones[0]?.name === "ห้องพัก 101", r.zones[0]?.name);
    check("rooms: zone has 4 corners", r.zones[0]?.points.length === 4, String(r.zones[0]?.points.length));
    check("rooms: the zone's own text is not duplicated as a label", r.items.length === 0, `got ${r.items.length} items`);
    check("rooms: the DIM line is not imported", r.walls.length === 0, `got ${r.walls.length} walls`);
    const withDim = build(doc, { ROOMS: "zone", DIM: "wall" });
    check("rooms: turning DIM on does import it", withDim.walls.length === 1, `got ${withDim.walls.length}`);
  }

  // 7. Binary DXF and DWG rejection ------------------------------------------
  {
    check("binary: sentinel detected", isBinaryDxf(BINARY));
    check("binary: a renamed .dwg detected", isBinaryDxf("AC1027" + NUL + NUL));
    check("binary: ASCII DXF not flagged", !isBinaryDxf(SAMPLE_CHAIN));
    let message = "";
    try { await parseDxf(BINARY); } catch (e) { message = e instanceof Error ? e.message : String(e); }
    check("binary: parse refuses with a clear message", message.includes("binary") && message.includes("DWG"), message || "no error thrown");
    let junk = "";
    try { await parseDxf("hello, this is not a drawing"); } catch (e) { junk = e instanceof Error ? e.message : String(e); }
    check("binary: junk refused too", junk.includes("DXF"), junk || "no error thrown");
  }

  // 8. MTEXT and TEXT escape handling ----------------------------------------
  {
    check("text: backslash-P becomes a space", cleanText("A\\PB") === "A B", cleanText("A\\PB"));
    check("text: font block stripped", cleanText("{\\fArial|b0|i0|c222|p34;ICU}") === "ICU", cleanText("{\\fArial|b0|i0|c222|p34;ICU}"));
    check("text: height and colour codes stripped", cleanText("\\H2.5x;\\C1;OR") === "OR", cleanText("\\H2.5x;\\C1;OR"));
    check("text: escaped braces kept", cleanText("a\\{b\\}c") === "a{b}c", cleanText("a\\{b\\}c"));
    check("text: degree escape", cleanText("25%%dC") === "25°C", cleanText("25%%dC"));
    check("text: runs of whitespace squeezed", cleanText("  a \t b  ") === "a b", JSON.stringify(cleanText("  a \t b  ")));
  }

  // 9. Unitless files, simplification and the caps ---------------------------
  {
    const unitless = await parseDxf(header(0) + entities(line("W", 0, 0, 30000, 0) + line("W", 30000, 0, 30000, 20000)));
    check("guess: big unitless numbers read as mm", unitless.unit === "mm" && !unitless.unitFromHeader, unitless.unit);
    const small = await parseDxf(header(0) + entities(line("W", 0, 0, 30, 0)));
    check("guess: small unitless numbers read as m", small.unit === "m", small.unit);

    const zig: [number, number][] = [];
    for (let i = 0; i <= 200; i++) zig.push([i * 0.1, (i % 2) * 0.005]);
    check("simplify: a 5 mm zigzag collapses at 2 cm", simplify(zig, 0.02).length === 2, String(simplify(zig, 0.02).length));
    check("simplify: the same zigzag survives at 1 mm", simplify(zig, 0.001).length > 100, String(simplify(zig, 0.001).length));

    let body = "";
    for (let i = 0; i < 700; i++) body += line("W", i * 2, 0, i * 2 + 1, 3); // 700 unconnected sticks
    const many = await parseDxf(header(6) + entities(body));
    const r = build(many, allLayers(many, "wall"));
    check("caps: over 500 walls is reported, not truncated", r.walls.length === 700 && r.overLimit && r.warnings.some((w) => w.includes("500")), `${r.walls.length} walls, warnings=${JSON.stringify(r.warnings)}`);
    const shorter = build(many, allLayers(many, "wall"), { minLength: 5 });
    check("caps: a minimum length drops the short runs", shorter.walls.length === 0 && shorter.dropped.short === 700, `${shorter.walls.length} walls, dropped ${shorter.dropped.short}`);
  }

  // 10. Progress and cancel ---------------------------------------------------
  {
    let body = "";
    for (let i = 0; i < 4000; i++) body += line("W", i, 0, i + 1, 1);
    const text = header(6) + entities(body);
    const seen: number[] = [];
    await parseDxf(text, { chunk: 500, onProgress: (p) => seen.push(p) });
    check("progress: reported while reading", seen.length > 3 && seen[seen.length - 1] === 1, JSON.stringify(seen.slice(0, 3)));
    check("progress: monotonic", seen.every((p, i) => i === 0 || p >= seen[i - 1]));
    let cancelled = "";
    try { await parseDxf(text, { chunk: 500, shouldCancel: () => true }); } catch (e) { cancelled = e instanceof Error ? e.name : String(e); }
    check("cancel: stops the parse", cancelled === "DxfCancelled", cancelled || "not cancelled");
  }

  // 11. POLYLINE / VERTEX / SEQEND, and CRLF vs LF vs leading spaces ----------
  {
    const poly = header(6) + entities(
      dxf(0, "POLYLINE", 8, "WALLS", 66, 1, 70, 1)
      + dxf(0, "VERTEX", 8, "WALLS", 10, 0, 20, 0) + dxf(0, "VERTEX", 8, "WALLS", 10, 8, 20, 0)
      + dxf(0, "VERTEX", 8, "WALLS", 10, 8, 20, 5) + dxf(0, "VERTEX", 8, "WALLS", 10, 0, 20, 5)
      + dxf(0, "SEQEND", 8, "WALLS") + line("WALLS", 0, 0, 8, 5),
    );
    const variants: [string, string][] = [
      ["CRLF", poly],
      ["LF", poly.split("\r\n").join("\n")],
      ["leading spaces", poly.split("\r\n").map((l) => (l ? "  " + l : l)).join("\r\n")],
    ];
    for (const [label, text] of variants) {
      const doc = await parseDxf(text);
      const ring = doc.shapes.find((s) => s.k === "poly" && s.closed);
      check(`polyline (${label}): closed ring of 4 vertices`, ring?.k === "poly" && ring.pts.length === 4, `got ${ring?.k === "poly" ? ring.pts.length : "none"}`);
      check(`polyline (${label}): the trailing LINE stays separate`, doc.shapes.length === 2, `got ${doc.shapes.length}`);
    }
  }

  // 12. Negative and scientific notation --------------------------------------
  {
    const doc = await parseDxf(header(6) + entities(dxf(0, "LINE", 8, "W", 10, "-1.5E+1", 20, "-2.0e0", 11, "1.5E1", 21, "2.0")));
    const s = doc.shapes[0];
    check("numbers: scientific and negative parsed", s.k === "poly" && s.pts[0][0] === -15 && s.pts[0][1] === -2 && s.pts[1][0] === 15, JSON.stringify(s.k === "poly" ? s.pts : s));
  }

  return fails;
}
