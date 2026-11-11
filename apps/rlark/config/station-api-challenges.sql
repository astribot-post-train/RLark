-- Apply only to a dedicated Station API database with a migration owner.
CREATE SCHEMA station_api;
CREATE TABLE station_api.challenges (
  challenge_id text PRIMARY KEY,
  cluster_id text NOT NULL,
  pod_uid text NOT NULL,
  node_uid text NOT NULL,
  station_uid text NOT NULL,
  operation text NOT NULL,
  generation bigint NOT NULL CHECK (generation > 0),
  request_digest char(64) NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
  expires_at timestamptz NOT NULL,
  consumed_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX station_challenges_expiry_idx ON station_api.challenges (expires_at, challenge_id);
-- Provision separate roles outside this schema migration:
-- station-api runtime: USAGE on schema; SELECT, INSERT, UPDATE on challenges.
-- janitor: USAGE on schema; SELECT and DELETE on challenges. Do not grant
-- DELETE to the online API runtime role.
-- migration owner: CREATE/ALTER privileges. Runtime role must not own schema/table.
