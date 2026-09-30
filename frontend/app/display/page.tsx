import type { Metadata } from "next";
import Kiosk from "./kiosk";

export const metadata: Metadata = {
  title: "จอแสดงผล · Aether",
  description: "จอแสดงผลห้องควบคุมของ Aether",
};

/** The wall display for a control room (docs/platform/display.md). Paired once with a code; no staff login. */
export default function DisplayPage() {
  return <Kiosk />;
}
