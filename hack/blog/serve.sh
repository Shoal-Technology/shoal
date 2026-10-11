#!/bin/sh
# Demo app for hack/deploy-e2e.sh (ADR 0017 pass F).
#
# Shoal injects DATABASE_URL from the blog-db-credentials Secret. Every two
# seconds this script connects with psql, creates the hits table if needed,
# inserts one row and writes a status page. busybox httpd serves the page on
# port 3000. The page shows select=1 when the query works, and hits=N with the
# row count, so the e2e can check that rows survive a Postgres restart.
PORT=${PORT:-3000}
WWW=/tmp/www
mkdir -p "$WWW" /app/uploads
state=unset
[ -n "${DATABASE_URL:-}" ] && state=set
echo "serve: starting on port $PORT, DATABASE_URL $state"
# Serve the page. busybox httpd is the first choice. If it cannot start, the
# log says why, and a netcat loop serves the same page on the same port, so
# the health check and the Route still get an answer.
if command -v httpd >/dev/null 2>&1 && httpd -p "$PORT" -h "$WWW" 2>"$WWW/httpd.err"; then
  echo "serve: httpd listening on $PORT"
else
  echo "serve: httpd did not start: $(cat "$WWW/httpd.err" 2>/dev/null) (command: $(command -v httpd || echo missing))"
  echo "serve: falling back to nc on $PORT"
  (
    while :; do
      {
        printf 'HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\n'
        cat "$WWW/index.html" 2>/dev/null
      } | nc -l -p "$PORT" >/dev/null 2>&1
    done
  ) &
fi

while :; do
  if out=$(psql "${DATABASE_URL:-}" -X -q -t -A -v ON_ERROR_STOP=1 \
    -c 'CREATE TABLE IF NOT EXISTS hits (id bigserial PRIMARY KEY, at timestamptz NOT NULL DEFAULT now())' \
    -c 'INSERT INTO hits DEFAULT VALUES' \
    -c "SELECT 'select=' || 1 || ' hits=' || count(*) FROM hits" 2>&1); then
    page="connected to postgres: $(printf '%s\n' "$out" | tail -n 1)"
  else
    page="postgres error: $out"
  fi
  echo "serve: $page"
  printf '%s\n' "$page" >"$WWW/index.html.tmp"
  mv "$WWW/index.html.tmp" "$WWW/index.html"
  sleep 2
done
