"use client";
import { useEffect, useState, type RefObject } from "react";
import { useLatest } from "./use-latest";

const API = import.meta.env.VITE_AETHER_API_ORIGIN ?? "";

export type SignalKind = "packet" | "event" | "alert" | "inventory";

/**
 * Packet and event signals arrive about once per second per busy gateway (every Minew tag advertises every
 * second). Refetching on each one sent a single tab past the API rate limit, so they are coalesced and
 * delivered at most once per this interval. `alert` signals are never delayed: an SOS must show at once.
 */
const SIGNAL_MIN_INTERVAL_MS = 3000;

function socketURL(): string {
  const base = API || location.origin;
  return base.replace(/^http/, "ws") + "/ws";
}

/**
 * Subscribes to the tenant's change signals. The socket never carries data: a signal only says
 * "refetch through the REST API". The access token is sent as the first message (never in the URL).
 * Reconnects with backoff; callers keep a slow poll as a safety net while `connected` is false.
 */
export function useSignals(handlers: RefObject<{ getToken: () => string; refresh: () => Promise<boolean> }>, onSignal: (kind: SignalKind, gatewayId: string | undefined) => void, enabled = true): boolean {
  const [connected, setConnected] = useState(false);
  const onSignalRef = useLatest(onSignal);

  useEffect(() => {
    if (!enabled || typeof WebSocket === "undefined") return;
    let stopped = false;
    let socket: WebSocket | null = null;
    let retry: ReturnType<typeof setTimeout> | undefined;
    let ping: ReturnType<typeof setInterval> | undefined;
    let delay = 1000;
    // Coalesced non-alert signals: kind -> gateway id (undefined once two gateways signalled the same kind).
    const queued = new Map<SignalKind, string | undefined>();
    let flushTimer: ReturnType<typeof setTimeout> | undefined;
    let lastFlush = 0;
    const flush = () => {
      flushTimer = undefined;
      lastFlush = Date.now();
      const batch = [...queued];
      queued.clear();
      for (const [kind, gatewayId] of batch) onSignalRef.current(kind, gatewayId);
    };
    const deliver = (kind: SignalKind, gatewayId: string | undefined) => {
      if (kind === "alert") {
        onSignalRef.current(kind, gatewayId);
        return;
      }
      queued.set(kind, queued.has(kind) && queued.get(kind) !== gatewayId ? undefined : gatewayId);
      flushTimer ??= setTimeout(flush, Math.max(0, lastFlush + SIGNAL_MIN_INTERVAL_MS - Date.now()));
    };

    const connect = () => {
      if (stopped) return;
      const ws = new WebSocket(socketURL());
      socket = ws;
      ws.onopen = () => ws.send(JSON.stringify({ type: "auth", token: handlers.current.getToken() }));
      ws.onmessage = (event) => {
        let msg: { type?: string; kind?: SignalKind; gateway_id?: string; error?: string };
        try {
          msg = JSON.parse(String(event.data));
        } catch {
          return;
        }
        if (msg.type === "ready") {
          delay = 1000;
          setConnected(true);
          clearInterval(ping);
          ping = setInterval(() => ws.readyState === WebSocket.OPEN && ws.send('{"type":"ping"}'), 30000);
        } else if (msg.type === "signal" && msg.kind) {
          deliver(msg.kind, msg.gateway_id || undefined);
        } else if (msg.type === "error" && msg.error === "too_many_connections") {
          // The workspace has too many open tabs. Polling keeps the page working; try again much later.
          delay = 120000;
        } else if (msg.type === "error" && msg.error === "unauthorized") {
          // The access token expired while we were away: refresh once, the close handler reconnects.
          void handlers.current.refresh().catch(() => false);
        }
      };
      ws.onclose = () => {
        clearInterval(ping);
        setConnected(false);
        if (stopped) return;
        retry = setTimeout(connect, delay);
        delay = Math.min(delay * 2, Math.max(delay, 30000));
      };
      ws.onerror = () => ws.close();
    };
    connect();
    return () => {
      stopped = true;
      clearTimeout(retry);
      clearTimeout(flushTimer);
      clearInterval(ping);
      socket?.close();
    };
  }, [enabled, handlers, onSignalRef]);

  return connected;
}
