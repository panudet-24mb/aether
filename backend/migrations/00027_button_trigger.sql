-- SOS press on the physical Minew B10, detected without teaching.
--
-- Measured on the real kit (2026-09-23, MG3 in production): the B10 broadcasts its A1-03 accelerometer,
-- A1-08 info and Eddystone-TLM slots all the time, but its iBeacon slot (and the Minew FFF1 frame next to
-- it) only for a while after the button is pressed. At rest there was no iBeacon for minutes; each press
-- started a dense burst that lasted well over a minute. The Eddystone-UID instance change the engine used
-- to rely on never happens on this unit (it has no UID slot at all).
--
-- A press is therefore the iBeacon slot reappearing after a quiet gap. `trigger_at` is the server time
-- the slot was last heard on this stream; the engine compares against it (alerts.ButtonTriggerQuiet).
--
-- +goose Up
SET ROLE aether_owner;
ALTER TABLE core.stream_state ADD COLUMN trigger_at timestamptz;
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
ALTER TABLE core.stream_state DROP COLUMN trigger_at;
RESET ROLE;
