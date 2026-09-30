// Types of GET /api/v1/twin/sites/:id/state (docs/platform/digital-twin.md, backend domain/twin.go).

export type PeopleMode = "counts" | "tracks" | "named";

export type TwinDevice = {
  id: string;
  kind: "gateway" | "device";
  ext?: string;
  name: string;
  profile?: string;
  floor_id: string;
  x: number;
  y: number;
  z: number;
  online: boolean;
  last_at: string | null;
  t?: number;
  h?: number;
  battery?: number;
  door?: 0 | 1;
  motion_at?: string;
  alert: boolean;
  sos: boolean;
};

export type TwinPerson = {
  pid: string;
  name?: string;
  gateway_id: string | null;
  since: string | null;
  candidate_gateway_id?: string;
  last_at: string | null;
  sos: boolean;
};

export type TwinAlert = {
  id: string;
  severity: string;
  status: "open" | "acknowledged";
  event: string;
  sos: boolean;
  hazard: boolean;
  title: string;
  device_id?: string;
  pid?: string;
  opened_at: string;
  gateway_id: string;
};

export type TwinState = {
  server_time: string;
  site_id: string;
  demo: boolean;
  layout_revision: Record<string, number>;
  devices: TwinDevice[];
  presence: { mode: PeopleMode; counts: { gateway_id: string; n: number }[]; people?: TwinPerson[] };
  alerts: TwinAlert[];
};

export type Layer = "temperature" | "humidity" | "occupancy" | "people";
export const LAYERS: { id: Layer; label: string }[] = [
  { id: "temperature", label: "อุณหภูมิ" },
  { id: "humidity", label: "ความชื้น" },
  { id: "occupancy", label: "การใช้พื้นที่" },
  { id: "people", label: "คน" },
];

/** A device reading is fresh for 2 minutes; past 15 minutes it no longer counts in the heat map. */
export const ONLINE_MS = 120000;
