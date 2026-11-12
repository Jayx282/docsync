#!/usr/bin/env bash
# A docsync demo on your machine: SQL Server in Docker with an example stone
# inventory, and docsync Studio in your browser.
#
#   make demo          start everything and open Studio (Ctrl+C stops Studio)
#   make demo-change   change some data, to watch documents update
#   make demo-stop     stop the SQL Server container
#
# Needs Go and Docker (Docker Desktop, Colima or Lima). On Apple silicon, Docker must
# run amd64 images (SQL Server has no arm64 build); Rosetta handles that.
set -euo pipefail
cd "$(dirname "$0")/.."

DB=stones_studio
say() { printf '\n\033[1m%s\033[0m\n' "$*"; }
die() { printf '\n\033[31m%s\033[0m\n' "$*" >&2; exit 1; }

ensure_docker() {
  docker info >/dev/null 2>&1 && return
  if command -v limactl >/dev/null 2>&1 && limactl list -q 2>/dev/null | grep -qx docker; then
    say "Starting the Lima VM that runs Docker (limactl start docker)..."
    limactl start docker >/dev/null 2>&1 || true
    if ! docker info >/dev/null 2>&1 && docker context inspect lima-docker >/dev/null 2>&1; then
      export DOCKER_CONTEXT=lima-docker
    fi
  fi
  docker info >/dev/null 2>&1 || die "Docker is not running. Start Docker Desktop, Colima or Lima, then run this again."
}

# The SQL Server to use: the integration-test container if this checkout has one
# (.env.test), otherwise a demo container with a password generated into .env.demo.
load_server() {
  if [ -f .env.test ] && grep -q '^DOCSYNC_TEST_DSN=' .env.test && docker ps -a --format '{{.Names}}' | grep -qx docsync-mssql; then
    set -a; . ./.env.test; set +a
    CONTAINER=docsync-mssql
    MASTER_DSN=$DOCSYNC_TEST_DSN
  else
    if [ ! -f .env.demo ]; then
      (umask 077; printf "MSSQL_SA_PASSWORD='Demo-%s-A1'\n" "$(openssl rand -hex 10)" > .env.demo)
    fi
    set -a; . ./.env.demo; set +a
    CONTAINER=docsync-demo
    MASTER_DSN="sqlserver://sa:${MSSQL_SA_PASSWORD}@127.0.0.1:1433?database=master&encrypt=disable"
  fi
}

sql() { # sql DATABASE QUERY: run a query in the container, print rows without headers
  docker exec -e SQLCMDPASSWORD="$MSSQL_SA_PASSWORD" "$CONTAINER" /opt/mssql-tools18/bin/sqlcmd \
    -S localhost -U sa -C -d "$1" -h -1 -W -b -Q "SET NOCOUNT ON; $2"
}

start_sqlserver() {
  if ! docker ps --format '{{.Names}}' | grep -qx "$CONTAINER"; then
    if docker ps -a --format '{{.Names}}' | grep -qx "$CONTAINER"; then
      say "Starting SQL Server (container $CONTAINER)..."
      docker start "$CONTAINER" >/dev/null
    else
      say "Downloading and starting SQL Server 2022 in Docker (first run only, about 1.5 GB)..."
      docker run -d --name "$CONTAINER" --platform linux/amd64 -e ACCEPT_EULA=Y -e "MSSQL_SA_PASSWORD=$MSSQL_SA_PASSWORD" \
        -p 1433:1433 mcr.microsoft.com/mssql/server:2022-latest >/dev/null
    fi
  fi
  say "Waiting for SQL Server to accept connections..."
  for _ in $(seq 1 90); do
    sql master "SELECT 1" >/dev/null 2>&1 && return
    sleep 2
  done
  die "SQL Server did not start. See: docker logs $CONTAINER"
}

cmd_start() {
  command -v go >/dev/null 2>&1 || die "Go is not installed (https://go.dev/dl/)."
  ensure_docker
  load_server
  start_sqlserver
  if [ "$(sql master "SELECT COUNT(*) FROM sys.databases WHERE name = '$DB'")" = 0 ]; then
    say "Creating the demo database $DB: 2,000 stones with labs, inclusions, rates and orders..."
    DOCSYNC_DSN="$MASTER_DSN" go run ./examples/stones/stonegen --create-database "$DB" --stones 2000 | head -1
  fi
  say "Building docsync..."
  go build -o bin/docsync ./cmd/docsync
  export DOCSYNC_DSN="${MASTER_DSN/database=master/database=$DB}"
  say "Starting docsync Studio on the demo database. Follow docs/getting-started.md. Ctrl+C stops Studio."
  args=(studio --dir ./studio)
  [ -n "${NO_BROWSER:-}" ] && args+=(--no-browser)
  exec ./bin/docsync "${args[@]}"
}

cmd_change() {
  ensure_docker
  load_server
  sql "$DB" "SELECT 1" >/dev/null 2>&1 || die "The demo database is not running. Start it with: make demo"
  sql "$DB" "
DECLARE @lab int = (SELECT TOP 1 lab_id FROM dbo.lab ORDER BY NEWID());
UPDATE dbo.lab SET name = CONCAT('Lab ', lab_id, ' (renamed at ', FORMAT(SYSDATETIME(), 'HH:mm:ss'), ')') WHERE lab_id = @lab;
DECLARE @priced bigint = (SELECT TOP 1 stoneid FROM dbo.rate ORDER BY NEWID());
UPDATE dbo.rate SET amount = amount * 1.10 WHERE stoneid = @priced;
DECLARE @sold bigint = (SELECT TOP 1 stoneid FROM dbo.stone WHERE status <> 'sold' ORDER BY NEWID());
UPDATE dbo.stone SET status = 'sold' WHERE stoneid = @sold;
SELECT CONCAT('Renamed lab ', @lab, ': ', (SELECT COUNT(*) FROM dbo.stone s JOIN dbo.lab l ON l.code = s.lab_code WHERE l.lab_id = @lab), ' stones use it.');
SELECT CONCAT('Raised the prices of stone ', @priced, ' by 10%.');
SELECT CONCAT('Marked stone ', @sold, ' as sold.');"
  say "Watch Studio's Pipeline tab: the affected documents update within a few seconds."
}

cmd_stop() {
  ensure_docker
  load_server
  docker stop "$CONTAINER" >/dev/null 2>&1 && say "Stopped SQL Server (container $CONTAINER). Your designs stay in ./studio." || say "SQL Server was not running."
  if command -v limactl >/dev/null 2>&1 && limactl list -q 2>/dev/null | grep -qx docker; then
    echo "To stop the Lima VM as well: limactl stop docker"
  fi
}

case "${1:-start}" in
  start) cmd_start ;;
  change) cmd_change ;;
  stop) cmd_stop ;;
  *) die "usage: scripts/demo.sh [start|change|stop]" ;;
esac
