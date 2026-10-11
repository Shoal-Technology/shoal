#!/bin/sh
# Demo app for hack/deploy-e2e.sh (ADR 0017 pass F).
#
# Shoal injects DATABASE_URL from the blog-db-credentials Secret. Every two
# seconds this script connects with psql, creates the hits table if needed,
# inserts one row and writes a status page. busybox httpd serves the page on
# port 3000. The page shows select=1 when the query works, and hits=N with the
# row count, so the e2e can check that rows survive a Postgres restart.
set -u

PORT=${PORT:-3000}
WWW=/tmp/www
mkdir -p "$WWW" /app/uploads
httpd -p "$PORT" -h "$WWW" || {
  echo "httpd failed" >&2
  exit 1
}

while :; do
  if out=$(psql "$DATABASE_URL" -X -q -t -A -v ON_ERROR_STOP=1 \
    -c 'CREATE TABLE IF NOT EXISTS hits (id bigserial PRIMARY KEY, at timestamptz NOT NULL DEFAULT now())' \
    -c 'INSERT INTO hits DEFAULT VALUES' \
    -c "SELECT 'select=' || 1 || ' hits=' || count(*) FROM hits" 2>&1); then
    page="connected to postgres: $(printf '%s\n' "$out" | tail -n 1)"
  else
    page="postgres error: $out"
  fi
  printf '%s\n' "$page" >"$WWW/index.html.tmp"
  mv "$WWW/index.html.tmp" "$WWW/index.html"
  sleep 2
done
