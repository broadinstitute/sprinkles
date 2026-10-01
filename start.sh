#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT/cli"

PROJECT="${PROJECT:-sparkles-v100}"

NODE_MODULES="${ROOT}/dashboard/node_modules"
if [ ! -d "${NODE_MODULES}" ] \
  || [ "${ROOT}/dashboard/package.json" -nt "${NODE_MODULES}" ] \
  || [ "${ROOT}/dashboard/package-lock.json" -nt "${NODE_MODULES}" ]; then
  echo "Installing frontend dependencies..."
  ( cd "${ROOT}/dashboard" && npm ci )
fi

echo "Building sprinkles..."
mkdir -p ./bin
go build -o ./bin/sprinkles ./cmd/sprinkles

echo "Loading config"
./bin/sprinkles dev set-config sample-config.json --project "${PROJECT}"

SPRINKLES="$(pwd)/bin/sprinkles"

cat > /tmp/mprocs-sprinkles.yaml <<EOF
procs:
  # "serve" runs the monitor and the dashboard backend in one process, the
  # same way it's deployed. Run "dev monitor" / "dev dashboard-backend"
  # separately if you need to restart just one of them.
  serve:
    cmd: ["${SPRINKLES}", "serve", "--project", "${PROJECT}", "--verbose"]
    log: "monitor.log"
  frontend:
    cmd: ["bash", "-c", "cd ${ROOT}/dashboard && npm run dev"]
  shell:
    cmd: ["bash"]
    stop: "SIGKILL"
EOF

# GOOGLE_APPLICATION_CREDENTIALS=$HOME/.sprinkles-cache/service-keys/ts-i28btmv9nw4jok.json go run . --project ts-i28btmv9nw4jok --subscriber-sa sprinkles-dashboard-user@depmap-portal-pipeline.iam.gserviceaccount.com
exec mprocs --config /tmp/mprocs-sprinkles.yaml
