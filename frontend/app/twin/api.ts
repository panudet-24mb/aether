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
  /** Controllable devices: what they are, whether any output is on, a plug's watts. */
  class?: "switch" | "lighting" | "cover" | "climate" | "fan";
  on?: 0 | 1;
  power?: number;
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

// Replay (GET /api/v1/twin/sites/:id/timeline and /replay, backend domain/twin.go): a keyframe at `from` plus changes.

export type ReplayPeopleMode = PeopleMode | "off";

export type TwinSettings = {
  people_replay: ReplayPeopleMode;
  people_replay_days: number;
  display_people: "off" | "counts" | "tracks";
  updated_at?: string;
  stored: boolean;
  demo: boolean;
  history_days: number;
};

export type TwinMarker = { at: string; kind: "sos" | "hazard" | "alert"; severity: string; alert_id: string; gateway_id?: string; device_id?: string; label: string };

export type TwinTimeline = {
  site_id: string;
  from: string;
  to: string;
  people_mode: ReplayPeopleMode;
  available: { env_from: string | null; people_from: string | null; markers_from: string | null; now: string };
  markers: TwinMarker[];
  density: { bucket_sec: number; from: string; lanes: Record<"alerts" | "door" | "zone", number[]> };
};

export type TwinSeries = { t: (number | null)[]; h: (number | null)[]; motion: (number | null)[]; door: (number | null)[] };

export type TwinReplayAlert = {
  id: string;
  severity: string;
  event: string;
  sos: boolean;
  hazard: boolean;
  gateway_id: string;
  device_id?: string;
  title: string;
  opened_at: string;
  acked_at?: string;
  resolved_at?: string;
};

export type TwinReplay = {
  site_id: string;
  from: string;
  to: string;
  bucket_sec: number;
  generated_at: string;
  people_mode: ReplayPeopleMode;
  env?: { buckets: number; series: Record<string, TwinSeries> };
  people?: {
    mode: ReplayPeopleMode;
    key_counts?: Record<string, number>;
    count_deltas?: [number, string, number][];
    key?: Record<string, string | null>;
    names?: Record<string, string>;
    deltas?: [number, string, string | null][];
  };
  alerts?: { key: string[]; items: TwinReplayAlert[] };
};
