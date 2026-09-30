-- Digital twin privacy (docs/platform/digital-twin.md, docs/platform/privacy.md): the movement history of 00046 is
-- personal data, so erasing a tag's history (core.erase_identity_data, 00039) deletes it too and counts it in the
-- ledger, and with it the tag's 5-minute buckets (core.sample_rollup holds only fixed sensors, but a tag erased as a
-- person's may have been rolled up while it was registered as one); a restore's reapply-erasures
-- (internal/privacyops) calls the same function, so it covers both as well. Both tables store the tag id in lower
-- case, so they match the registration's id in any case.
-- core.erasure_count lets the twin's replay cache notice an erasure: a cached window from before one is never served.

-- +goose Up
SET ROLE aether_owner;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION core.erase_identity_data(tenant uuid, external text, project uuid, tenant_wide boolean) RETURNS jsonb
 LANGUAGE plpgsql VOLATILE SET search_path = pg_catalog, pg_temp AS $$
DECLARE samples bigint; ble bigint; presence bigint; moves bigint; rollups bigint; events bigint; kept_events bigint; alerts bigint;
 notices bigint; runs bigint; learned bigint; sessions bigint; streams bigint; old_names bigint;
 gw uuid[];
 erased_name constant text := 'ผู้สวมใส่ (ลบข้อมูลแล้ว)';
BEGIN
 PERFORM set_config('app.tenant_id', tenant::text, true);
 PERFORM 1 FROM core.gateways g
  WHERE g.tenant_id = tenant AND (coalesce(tenant_wide, false) OR g.project_id IS NOT DISTINCT FROM project)
  ORDER BY g.id FOR UPDATE;
 SELECT coalesce(array_agg(g.id ORDER BY g.id), '{}') INTO gw FROM core.gateways g
  WHERE g.tenant_id = tenant AND (coalesce(tenant_wide, false) OR g.project_id IS NOT DISTINCT FROM project);
 DELETE FROM core.sensor_samples s WHERE s.tenant_id = tenant AND s.external_id = external AND s.gateway_id = ANY(gw);
 GET DIAGNOSTICS samples = ROW_COUNT;
 DELETE FROM core.ble_history b WHERE b.tenant_id = tenant AND b.external_id = external AND b.gateway_id = ANY(gw);
 GET DIAGNOSTICS ble = ROW_COUNT;
 -- One row per tag: it goes when its zone, or the zone it is about to move to, is in scope (or it has no zone).
 DELETE FROM core.presence_state p WHERE p.tenant_id = tenant AND p.external_id = external
   AND (p.gateway_id IS NULL OR p.gateway_id = ANY(gw) OR p.candidate_gateway_id = ANY(gw));
 GET DIAGNOSTICS presence = ROW_COUNT;
 -- Movement history (00046): a transition goes when either end is in scope, or it has neither (never heard).
 DELETE FROM core.presence_history h WHERE h.tenant_id = tenant AND h.external_id = lower(external)
   AND (h.gateway_id = ANY(gw) OR h.from_gateway_id = ANY(gw) OR (h.gateway_id IS NULL AND h.from_gateway_id IS NULL));
 GET DIAGNOSTICS moves = ROW_COUNT;
 DELETE FROM core.sample_rollup r WHERE r.tenant_id = tenant AND r.external_id = lower(external) AND r.gateway_id = ANY(gw);
 GET DIAGNOSTICS rollups = ROW_COUNT;
 -- Alerts are incident records: kept, under a generic title that names only the kind of event.
 UPDATE core.alerts a SET device_name = erased_name,
   title = CASE a.event_type WHEN 'button' THEN 'กดปุ่มฉุกเฉิน SOS' WHEN 'tamper' THEN 'ป้ายถูกถอด (tamper)' WHEN 'leak' THEN 'พบน้ำรั่ว'
     WHEN 'motion' THEN 'เริ่มเคลื่อนไหว' WHEN 'offline' THEN 'ขาดการติดต่อ' WHEN 'threshold' THEN 'ค่าเกินเกณฑ์'
     WHEN 'zone' THEN 'wearable เข้าโซน' ELSE 'การแจ้งเตือน' END || ' · ' || erased_name
  WHERE a.tenant_id = tenant AND a.external_id = external AND a.gateway_id = ANY(gw);
 GET DIAGNOSTICS alerts = ROW_COUNT;
 -- A delivery error can quote what the channel echoed back (the title, the name).
 UPDATE core.notifications n SET last_error = NULL
  WHERE n.tenant_id = tenant AND n.last_error IS NOT NULL
    AND n.alert_id IN (SELECT a.id FROM core.alerts a WHERE a.tenant_id = tenant AND a.external_id = external AND a.gateway_id = ANY(gw));
 GET DIAGNOSTICS notices = ROW_COUNT;
 UPDATE core.device_events e SET device_name = erased_name, detail = '{}'::jsonb
  WHERE e.tenant_id = tenant AND e.external_id = external AND e.gateway_id = ANY(gw)
    AND EXISTS(SELECT 1 FROM core.alerts a WHERE a.tenant_id = e.tenant_id AND a.event_id = e.id);
 GET DIAGNOSTICS kept_events = ROW_COUNT;
 DELETE FROM core.device_events e WHERE e.tenant_id = tenant AND e.external_id = external AND e.gateway_id = ANY(gw)
    AND NOT EXISTS(SELECT 1 FROM core.alerts a WHERE a.tenant_id = e.tenant_id AND a.event_id = e.id);
 GET DIAGNOSTICS events = ROW_COUNT;
 -- Flow runs the tag triggered: kept (the flow's history) without the tag or what it reported.
 UPDATE core.automation_runs r SET external_id = '', detail = '{}'::jsonb
  WHERE r.tenant_id = tenant AND r.external_id = external AND (r.gateway_id IS NULL OR r.gateway_id = ANY(gw));
 GET DIAGNOSTICS runs = ROW_COUNT;
 -- Learned signals and teaching sessions keep working; only their free-text labels go. Learned signals belong to
 -- the tag, not to a gateway, so they are scrubbed whatever the scope (a label, no data).
 UPDATE core.device_signals d SET description = '' WHERE d.tenant_id = tenant AND d.external_id = external AND d.description <> '';
 GET DIAGNOSTICS learned = ROW_COUNT;
 UPDATE core.signal_sessions s SET label = '' WHERE s.tenant_id = tenant AND s.external_id = external AND s.gateway_id = ANY(gw) AND s.label <> '';
 GET DIAGNOSTICS sessions = ROW_COUNT;
 UPDATE core.sensor_streams st SET name = upper(external)
  WHERE st.tenant_id = tenant AND st.external_id = external AND st.gateway_id = ANY(gw) AND st.name <> upper(external);
 GET DIAGNOSTICS streams = ROW_COUNT;
 -- Earlier (removed) registrations of the tag carry the names of earlier wearers. The live one is renamed by the
 -- caller when the owner gives a new name.
 UPDATE core.devices d SET name = erased_name
  WHERE d.tenant_id = tenant AND d.external_id = external AND d.removed_at IS NOT NULL AND d.gateway_id = ANY(gw) AND d.name <> erased_name;
 GET DIAGNOSTICS old_names = ROW_COUNT;
 RETURN jsonb_build_object('samples', samples, 'ble_history', ble, 'presence', presence, 'presence_history', moves, 'sample_rollup', rollups, 'events', events,
  'events_anonymised', kept_events, 'alerts_anonymised', alerts, 'notification_errors_cleared', notices,
  'flow_runs_anonymised', runs, 'signal_labels_cleared', learned + sessions, 'stream_names_cleared', streams,
  'removed_registrations_renamed', old_names, 'gateways_in_scope', cardinality(gw));
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION core.erasure_count() RETURNS bigint
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 SELECT count(*) FROM core.erasure_log e WHERE e.tenant_id = core.tenant_id()
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.erasure_count() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.erasure_count() TO aether_app;
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
DROP FUNCTION core.erasure_count();
-- erase_identity_data as 00039 defined it.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION core.erase_identity_data(tenant uuid, external text, project uuid, tenant_wide boolean) RETURNS jsonb
 LANGUAGE plpgsql VOLATILE SET search_path = pg_catalog, pg_temp AS $$
DECLARE samples bigint; ble bigint; presence bigint; events bigint; kept_events bigint; alerts bigint;
 notices bigint; runs bigint; learned bigint; sessions bigint; streams bigint; old_names bigint;
 gw uuid[];
 erased_name constant text := 'ผู้สวมใส่ (ลบข้อมูลแล้ว)';
BEGIN
 PERFORM set_config('app.tenant_id', tenant::text, true);
 PERFORM 1 FROM core.gateways g
  WHERE g.tenant_id = tenant AND (coalesce(tenant_wide, false) OR g.project_id IS NOT DISTINCT FROM project)
  ORDER BY g.id FOR UPDATE;
 SELECT coalesce(array_agg(g.id ORDER BY g.id), '{}') INTO gw FROM core.gateways g
  WHERE g.tenant_id = tenant AND (coalesce(tenant_wide, false) OR g.project_id IS NOT DISTINCT FROM project);
 DELETE FROM core.sensor_samples s WHERE s.tenant_id = tenant AND s.external_id = external AND s.gateway_id = ANY(gw);
 GET DIAGNOSTICS samples = ROW_COUNT;
 DELETE FROM core.ble_history b WHERE b.tenant_id = tenant AND b.external_id = external AND b.gateway_id = ANY(gw);
 GET DIAGNOSTICS ble = ROW_COUNT;
 -- One row per tag: it goes when its zone, or the zone it is about to move to, is in scope (or it has no zone).
 DELETE FROM core.presence_state p WHERE p.tenant_id = tenant AND p.external_id = external
   AND (p.gateway_id IS NULL OR p.gateway_id = ANY(gw) OR p.candidate_gateway_id = ANY(gw));
 GET DIAGNOSTICS presence = ROW_COUNT;
 -- Alerts are incident records: kept, under a generic title that names only the kind of event.
 UPDATE core.alerts a SET device_name = erased_name,
   title = CASE a.event_type WHEN 'button' THEN 'กดปุ่มฉุกเฉิน SOS' WHEN 'tamper' THEN 'ป้ายถูกถอด (tamper)' WHEN 'leak' THEN 'พบน้ำรั่ว'
     WHEN 'motion' THEN 'เริ่มเคลื่อนไหว' WHEN 'offline' THEN 'ขาดการติดต่อ' WHEN 'threshold' THEN 'ค่าเกินเกณฑ์'
     WHEN 'zone' THEN 'wearable เข้าโซน' ELSE 'การแจ้งเตือน' END || ' · ' || erased_name
  WHERE a.tenant_id = tenant AND a.external_id = external AND a.gateway_id = ANY(gw);
 GET DIAGNOSTICS alerts = ROW_COUNT;
 -- A delivery error can quote what the channel echoed back (the title, the name).
 UPDATE core.notifications n SET last_error = NULL
  WHERE n.tenant_id = tenant AND n.last_error IS NOT NULL
    AND n.alert_id IN (SELECT a.id FROM core.alerts a WHERE a.tenant_id = tenant AND a.external_id = external AND a.gateway_id = ANY(gw));
 GET DIAGNOSTICS notices = ROW_COUNT;
 UPDATE core.device_events e SET device_name = erased_name, detail = '{}'::jsonb
  WHERE e.tenant_id = tenant AND e.external_id = external AND e.gateway_id = ANY(gw)
    AND EXISTS(SELECT 1 FROM core.alerts a WHERE a.tenant_id = e.tenant_id AND a.event_id = e.id);
 GET DIAGNOSTICS kept_events = ROW_COUNT;
 DELETE FROM core.device_events e WHERE e.tenant_id = tenant AND e.external_id = external AND e.gateway_id = ANY(gw)
    AND NOT EXISTS(SELECT 1 FROM core.alerts a WHERE a.tenant_id = e.tenant_id AND a.event_id = e.id);
 GET DIAGNOSTICS events = ROW_COUNT;
 -- Flow runs the tag triggered: kept (the flow's history) without the tag or what it reported.
 UPDATE core.automation_runs r SET external_id = '', detail = '{}'::jsonb
  WHERE r.tenant_id = tenant AND r.external_id = external AND (r.gateway_id IS NULL OR r.gateway_id = ANY(gw));
 GET DIAGNOSTICS runs = ROW_COUNT;
 -- Learned signals and teaching sessions keep working; only their free-text labels go. Learned signals belong to
 -- the tag, not to a gateway, so they are scrubbed whatever the scope (a label, no data).
 UPDATE core.device_signals d SET description = '' WHERE d.tenant_id = tenant AND d.external_id = external AND d.description <> '';
 GET DIAGNOSTICS learned = ROW_COUNT;
 UPDATE core.signal_sessions s SET label = '' WHERE s.tenant_id = tenant AND s.external_id = external AND s.gateway_id = ANY(gw) AND s.label <> '';
 GET DIAGNOSTICS sessions = ROW_COUNT;
 UPDATE core.sensor_streams st SET name = upper(external)
  WHERE st.tenant_id = tenant AND st.external_id = external AND st.gateway_id = ANY(gw) AND st.name <> upper(external);
 GET DIAGNOSTICS streams = ROW_COUNT;
 -- Earlier (removed) registrations of the tag carry the names of earlier wearers. The live one is renamed by the
 -- caller when the owner gives a new name.
 UPDATE core.devices d SET name = erased_name
  WHERE d.tenant_id = tenant AND d.external_id = external AND d.removed_at IS NOT NULL AND d.gateway_id = ANY(gw) AND d.name <> erased_name;
 GET DIAGNOSTICS old_names = ROW_COUNT;
 RETURN jsonb_build_object('samples', samples, 'ble_history', ble, 'presence', presence, 'events', events,
  'events_anonymised', kept_events, 'alerts_anonymised', alerts, 'notification_errors_cleared', notices,
  'flow_runs_anonymised', runs, 'signal_labels_cleared', learned + sessions, 'stream_names_cleared', streams,
  'removed_registrations_renamed', old_names, 'gateways_in_scope', cardinality(gw));
END $$;
-- +goose StatementEnd
RESET ROLE;
