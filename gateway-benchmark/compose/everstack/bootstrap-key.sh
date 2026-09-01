#!/usr/bin/env bash
# Insert the benchmark's API key into a running Everstack's database.
#
# Everstack has no CLI command to mint a key, and its /openai/v1 surface rejects
# unauthenticated requests, so a fresh instance cannot be benchmarked until a key
# row exists. The stored value is HMAC-SHA256(key, secret) in base64url with no
# padding (internal/lib/apikey/hash.go), which is reproducible without the Go
# code, so this script needs nothing but psql and openssl.
#
# Usage: ./bootstrap-key.sh [api-key] [hash-secret] [postgres-container]
set -euo pipefail

API_KEY="${1:-${EVERSTACK_BENCH_KEY:-bench-key}}"
SECRET="${2:-${EVS_API_KEY_HASH_SECRET:-bench-only-not-a-production-secret}}"
PG_CONTAINER="${3:-gwbench-everstack-db}"
# Must be a UUID: it becomes the request's tenant id downstream.
ORG_ID="${ORG_ID:-00000000-0000-0000-0000-0000000000aa}"
MODEL_PROVIDER="${MODEL_PROVIDER:-openai}"
MODEL_NAME="${MODEL_NAME:-gpt-4o-mini}"

# base64url, no padding, to match base64.RawURLEncoding.
hash_key() {
  printf '%s' "$1" \
    | openssl dgst -sha256 -hmac "$2" -binary \
    | openssl base64 -A \
    | tr '+/' '-_' | tr -d '='
}

HASH="$(hash_key "$API_KEY" "$SECRET")"
echo "api key    : ${API_KEY}"
echo "org id     : ${ORG_ID}"
echo "stored hash: ${HASH}"

# Everstack migrates into the everstack schema, not public, so the search
# path has to be set explicitly or the insert fails with "relation does not exist".
# NOTE: this heredoc is deliberately unquoted so ${HASH} and friends expand.
# That also means the shell would command-substitute any backtick inside it, so
# the SQL comments below use no backticks.
docker exec -i "$PG_CONTAINER" psql -U postgres -d everstack -v ON_ERROR_STOP=1 <<SQL
SET search_path TO everstack, public;
-- sensitive_id must not be NULL. The read model scans it into a plain string,
-- so a NULL there fails with "converting NULL to string is unsupported" and the
-- request is rejected as "Invalid API key" with nothing in the default logs to
-- explain why.
INSERT INTO api_keys (name, hash, type, org_id, sensitive_id)
SELECT 'gwbench', '${HASH}', 'secret', '${ORG_ID}', 'gwbench-sensitive'
WHERE NOT EXISTS (SELECT 1 FROM api_keys WHERE hash = '${HASH}');
UPDATE api_keys SET sensitive_id = COALESCE(sensitive_id, 'gwbench-sensitive') WHERE hash = '${HASH}';
-- Everstack resolves a request's model against its catalog and will not route
-- to a model that has no active status row, failing with "no provider found for
-- model" even when the YAML declares it. Activate the benchmark's model.
INSERT INTO provider_model_status (provider_name, model_name, status, freshness)
VALUES ('${MODEL_PROVIDER}', '${MODEL_NAME}', 'active', 'fresh')
ON CONFLICT DO NOTHING;

-- The gateway's rate limiter reads runtime_config in the database, NOT the
-- gateway.yaml gateway.rate_limit block, which is silently ignored. The
-- seeded default is 500 rpm, i.e. 8.3 rps, so a perf phase offering 50 rps has
-- 84% of its requests refused and the latency figures are computed from the
-- survivors. No other gateway in this benchmark has a limit active during the
-- perf phases; the rate-limit scenario (C5) tests limits deliberately and
-- separately.
UPDATE runtime_config
   SET config = jsonb_set(config::jsonb, '{enabled}', 'false'::jsonb)
 WHERE section = 'rate_limit';

SELECT id, name, org_id, sensitive_id, COALESCE(revoked, FALSE) AS revoked FROM api_keys WHERE hash = '${HASH}';
SELECT section, config::text FROM runtime_config WHERE section = 'rate_limit';
SELECT provider_name, model_name, status FROM provider_model_status;
SQL

echo
echo "Done. The 'everstack' target in targets.yaml authenticates with this key."
