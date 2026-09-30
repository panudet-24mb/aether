import { createContext, useContext } from "react";

/**
 * A demo workspace (tenants.demo, cmd/demo-twin) is shown as a real site: its readings are simulated by design, so
 * the "SIM" / "simulated" markers that flag test data elsewhere stay off its screens. Real workspaces keep them.
 */
export const WorkspaceMode = createContext<{ demo: boolean; owner: boolean }>({ demo: false, owner: false });

/** Whether simulated-data markers should be shown on this workspace's screens. */
export function useShowSimulated(): boolean {
  return !useContext(WorkspaceMode).demo;
}
