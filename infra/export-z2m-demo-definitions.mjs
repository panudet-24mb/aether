#!/usr/bin/env node
// Exports the real Zigbee2MQTT definitions (vendor, model, description, zigbee model ids and the full exposes, with
// their types, units, ranges, enum values and binary on/off values) of the devices the demo hospital simulates
// (cmd/demo-twin extend), from zigbee-herdsman-converters, into a JSON the backend embeds:
//
//   node infra/export-z2m-demo-definitions.mjs            # the release the device catalog uses (26.112.0)
//   node infra/export-z2m-demo-definitions.mjs 26.112.0
//
// Exposes that depend on the paired device (most Tuya switches and plugs) are evaluated for a device with the right
// number of endpoints, the way the zigbee2mqtt.io device pages show them. zigbee-herdsman-converters is MIT
// licensed (Copyright (c) Koen Kanters); its licence text sits next to the device catalog in
// backend/internal/z2mcatalog.
import { execFileSync } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const version = process.argv[2] || "26.112.0";
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const out = path.join(root, "backend", "internal", "simulation", "z2m_demo_definitions.json");

// vendor/model pairs, with the endpoint count a dynamic definition is evaluated for.
const models = [
  ["Tuya", "TS0001", 1], ["Tuya", "TS0002", 2], ["Tuya", "TS0003", 3], ["Tuya", "TS0004", 4],
  ["Tuya", "TS011F_plug_1", 1], ["Tuya", "TS0601_thermostat", 1], ["Tuya", "TS0601_cover_1", 1],
  ["Tuya", "TS0601_human_presence_sensor", 1], ["Tuya", "TS0601_air_quality_sensor", 1], ["Tuya", "TS0601_smoke_1", 1],
  ["Tuya", "TS0207_water_leak_detector", 1], ["Tuya", "TS0201", 1], ["Tuya", "WSD500A", 1], ["Tuya", "TS0203", 1], ["Tuya", "TS0202", 1],
  ["Aqara", "WSDCGQ11LM", 1], ["Aqara", "MCCGQ11LM", 1], ["Aqara", "RTCGQ11LM", 1], ["Aqara", "SJCGQ11LM", 1],
  ["IKEA", "LED1623G12", 1], ["IKEA", "ICTC-G-1", 1], ["IKEA", "E2001/E2002/E2313", 1], ["IKEA", "E2007", 1],
  ["Philips", "9290012607", 1],
  ["SONOFF", "SNZB-01", 1], ["SONOFF", "SNZB-02", 1], ["SONOFF", "SNZB-03", 1], ["SONOFF", "SNZB-04", 1],
  ["Heiman", "HS1SA-E", 1], ["Heiman", "HS1CG", 1], ["Heiman", "HS1CA-E", 1],
];

const work = mkdtempSync(path.join(tmpdir(), "aether-zhc-demo-"));
try {
  writeFileSync(path.join(work, "package.json"), '{"private":true}');
  execFileSync("npm", ["install", "--silent", "--no-audit", "--no-fund", "--ignore-scripts", `zigbee-herdsman-converters@${version}`], { cwd: work, stdio: "inherit" });
  const pkgDir = path.join(work, "node_modules", "zigbee-herdsman-converters");
  const require = createRequire(path.join(work, "index.js"));
  const zhc = require("zigbee-herdsman-converters");
  const definitions = require(path.join(pkgDir, "dist", "devices", "index.js")).default;
  const mock = (model, endpoints) => {
    const eps = [...Array(endpoints)].map((_, i) => ({ ID: i + 1, inputClusters: [0, 1, 6, 8, 1026, 1029, 1280, 1794, 2820], outputClusters: [], supportsInputCluster: () => true, supportsOutputCluster: () => false, getClusterAttributeValue: () => undefined }));
    return { modelID: model, manufacturerName: "_TZ3000_aether", endpoints: eps, getEndpoint: (id) => eps.find((e) => e.ID === id), isDevice: true, powerSource: "Mains (single phase)", softwareBuildID: "", type: "Router", meta: {}, customClusters: {} };
  };
  const found = [];
  for (const [vendor, model, endpoints] of models) {
    const raw = definitions.find((d) => d.model === model && (d.vendor === vendor || zhc.prepareDefinition(d).vendor === vendor));
    if (!raw) throw new Error(`not in zigbee-herdsman-converters ${version}: ${vendor} ${model}`);
    const d = zhc.prepareDefinition(raw);
    let exposes = d.exposes;
    if (typeof exposes === "function") exposes = exposes(mock((d.zigbeeModel || [model])[0], endpoints), {});
    exposes = JSON.parse(JSON.stringify(exposes));
    if (!exposes.length) throw new Error(`${vendor} ${model}: no exposes`);
    // Model ids and fingerprints as the device reports them in its Basic cluster (bridge/devices model_id and
    // manufacturer). A few zigbeeModel entries carry a trailing NUL some firmwares send; PostgreSQL cannot store it.
    const printable = (v) => typeof v === "string" && v.length > 0 && !/[\u0000-\u001f]/.test(v);
    const fingerprint = (raw.fingerprint || []).filter((f) => printable(f.modelID)).slice(0, 2).map((f) => ({ model_id: f.modelID, manufacturer: f.manufacturerName || "" }));
    found.push({ vendor: d.vendor, model: d.model, description: d.description, zigbee_model: (d.zigbeeModel || []).filter(printable).slice(0, 4), fingerprint, exposes });
  }
  writeFileSync(out, JSON.stringify({ source: "zigbee-herdsman-converters", version, license: "MIT", definitions: found }, null, 1) + "\n");
  console.log(`wrote ${found.length} definitions to ${path.relative(root, out)}`);
} finally {
  rmSync(work, { recursive: true, force: true });
}
