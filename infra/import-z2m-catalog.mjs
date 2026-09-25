#!/usr/bin/env node
// Imports the Zigbee2MQTT device catalog (every device zigbee-herdsman-converters supports) into Aether as a
// compact, gzipped JSON that the backend embeds and serves at GET /api/v1/catalog/zigbee.
//
//   node infra/import-z2m-catalog.mjs            # latest release
//   node infra/import-z2m-catalog.mjs 26.112.0   # a specific release
//
// zigbee-herdsman-converters is MIT licensed (Copyright (c) Koen Kanters); its licence text is copied next to the
// artifact. The catalog is for browsing and labelling only: ingest never depends on it, it reads each paired
// device's own definition from the bridge.
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, writeFileSync, copyFileSync, rmSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { gzipSync } from "node:zlib";

const version = process.argv[2] || "latest";
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const outDir = path.join(root, "backend", "internal", "z2mcatalog");
const work = mkdtempSync(path.join(tmpdir(), "aether-zhc-"));

try {
  writeFileSync(path.join(work, "package.json"), '{"private":true}');
  execFileSync("npm", ["install", "--silent", "--no-audit", "--no-fund", "--ignore-scripts", `zigbee-herdsman-converters@${version}`], { cwd: work, stdio: "inherit" });
  const pkgDir = path.join(work, "node_modules", "zigbee-herdsman-converters");
  const pkg = JSON.parse(readFileSync(path.join(pkgDir, "package.json"), "utf8"));
  const require = createRequire(path.join(work, "index.js"));
  const zhc = require("zigbee-herdsman-converters");
  // The package does not export its definition list; the devices index is its source of truth.
  const definitions = require(path.join(pkgDir, "dist", "devices", "index.js")).default;

  const specific = new Set(["light", "switch", "fan", "cover", "lock", "climate"]);
  const sosActions = new Set(["emergency", "sos", "panic"]);
  const sosBinaries = new Set(["sos", "sos_alarm"]);
  // Mirrors zigbee2mqtt.featureKey: these names mean an alarm / door / occupancy only for a binary feature; a
  // numeric of the same name (a gas meter, a vibration strength) is kept as <name>_value.
  const semanticBinaries = new Set(["smoke", "gas", "carbon_monoxide", "sos", "sos_alarm", "tamper", "water_leak", "contact", "occupancy", "presence", "vibration", "battery_low"]);
  const featureKey = (e) => (semanticBinaries.has(e.name) && e.type !== "binary" ? `${e.name}_value` : e.name);
  const clip = (s, n) => String(s ?? "").replace(/\0/g, "").trim().slice(0, n);

  // Mirrors zigbee2mqtt.Summarize in the backend: feature names, the specific types they sit in, and whether the
  // definition can call for help. The backend derives the category from exactly these.
  function summarize(exposes) {
    const names = new Set(), groups = new Set();
    let sos = false;
    const walk = (list, group, depth) => {
      if (depth > 4) return;
      for (const e of list || []) {
        if (specific.has(e.type)) { walk(e.features, e.type, depth + 1); continue; }
        if (!e.name) continue;
        names.add(featureKey(e));
        if (group) groups.add(group);
        if (e.name === "action" && e.type === "enum" && (e.values || []).some((v) => sosActions.has(String(v).toLowerCase()))) sos = true;
        if (e.type === "binary" && sosBinaries.has(e.name) && !((e.access ?? 0) & 2)) sos = true;
      }
    };
    walk(exposes, "", 0);
    // Every name is kept (the backend derives the category from the full set; a truncated list could drop the
    // one feature that decides it), bounded only against a runaway definition.
    return { names: [...names].sort().slice(0, 200), groups: [...groups].sort(), sos };
  }

  const devices = [];
  let dynamic = 0, failed = 0;
  for (const raw of definitions) {
    let d;
    try { d = zhc.prepareDefinition(raw); } catch { failed++; continue; }
    let exposes = d.exposes, isDynamic = false;
    if (typeof exposes === "function") {
      isDynamic = true;
      dynamic++;
      // Exposes that depend on the paired device: evaluate them for "no device" like the Zigbee2MQTT docs do.
      try { exposes = exposes(undefined, {}); } catch { exposes = []; }
    }
    const s = summarize(Array.isArray(exposes) ? exposes : []);
    const entry = { v: clip(d.vendor, 64), m: clip(d.model, 64), d: clip(d.description, 160) };
    const zigbeeModel = [...new Set((d.zigbeeModel || []).map((z) => clip(z, 64)).filter(Boolean))].slice(0, 8);
    if (zigbeeModel.length) entry.z = zigbeeModel;
    const whiteLabel = (d.whiteLabel || []).map((w) => clip(`${w.vendor ?? d.vendor} ${w.model}`, 96)).filter(Boolean).slice(0, 8);
    if (whiteLabel.length) entry.w = whiteLabel;
    if (s.names.length) entry.f = s.names;
    if (s.groups.length) entry.g = s.groups;
    if (s.sos) entry.s = 1;
    if (isDynamic) entry.x = 1;
    if (entry.v && entry.m) devices.push(entry);
  }
  devices.sort((a, b) => a.v.localeCompare(b.v) || a.m.localeCompare(b.m));

  const doc = { source: "zigbee-herdsman-converters", version: pkg.version, license: pkg.license, homepage: "https://github.com/Koenkk/zigbee-herdsman-converters", devices };
  const gz = gzipSync(Buffer.from(JSON.stringify(doc)), { level: 9 });
  writeFileSync(path.join(outDir, "catalog.json.gz"), gz);
  copyFileSync(path.join(pkgDir, "LICENSE"), path.join(outDir, "LICENSE.zigbee-herdsman-converters"));
  console.log(`zigbee-herdsman-converters ${pkg.version} (${pkg.license}): ${devices.length} devices, ${dynamic} with device-dependent exposes, ${failed} skipped; ${(gz.length / 1024).toFixed(0)} KiB gzipped`);
} finally {
  rmSync(work, { recursive: true, force: true });
}
