import { environmentReading, latestMeasurements, normalizeReading } from "./measurements";
import type { Reading } from "../topology/api";
export function runMeasurementSelfTest() {
 const environment: Reading = { received_at:"2026-09-21T12:00:00Z", kind:"environment", frames:["minew-ffe1-a101@1"],temperature:25.6,humidity:72,battery:90,rssi:-45 };
 const info: Reading = { received_at:"2026-09-21T12:00:01Z", kind:"beacon", frames:["ibeacon@1"],temperature:0,humidity:0,battery:0,rssi:-48 };
 const result=latestMeasurements({latest:info,history:[environment]});
 if(result.temperature!==25.6 || result.humidity!==72 || result.battery!==90) throw Error("alternating beacon erased measurement");
 if(environmentReading(info)) throw Error("beacon zero treated as environment sample");
 const missing=latestMeasurements({latest:info,history:[]});
 if(!Number.isNaN(missing.temperature)) throw Error("missing measurement shown as zero");
 const zero={...environment,temperature:0};
 if(latestMeasurements({latest:zero,history:[]}).temperature!==0) throw Error("valid zero dropped");
 // Zigbee2MQTT: measurements live in metrics, each message may carry a subset.
 const zTemp: Reading = { received_at:"2026-09-21T12:00:02Z", kind:"environment", frames:["z2m-state@1"],temperature:0,humidity:0,battery:91,rssi:null,metrics:{temperature:24.6,humidity:55} };
 const zBattery: Reading = { received_at:"2026-09-21T12:00:03Z", kind:"environment", frames:["z2m-state@1"],temperature:0,humidity:0,battery:90,rssi:null,metrics:{linkquality:80} };
 const z=latestMeasurements({latest:zBattery,history:[zTemp]});
 if(z.temperature!==24.6 || z.humidity!==55 || z.battery!==90) throw Error("zigbee subset erased measurement");
 if(environmentReading(zBattery)) throw Error("zigbee message without temperature treated as environment sample");
 if(!Number.isNaN(normalizeReading(zBattery).temperature) || normalizeReading(zTemp).temperature!==24.6) throw Error("zigbee normalisation");
 const zEmpty: Reading = { ...zBattery, received_at:"2026-09-21T12:00:04Z", battery:0, metrics:{battery:0} };
 if(latestMeasurements({latest:zEmpty,history:[zTemp]}).battery!==0) throw Error("zigbee 0% battery lost");
 console.log("PASS: alternating frames, missing values, valid zero, zigbee subsets, zigbee 0% battery");
}
