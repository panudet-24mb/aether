import { environmentReading, latestMeasurements } from "./measurements";
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
 console.log("PASS: alternating frames, missing values, valid zero");
}
