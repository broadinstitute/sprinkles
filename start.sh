#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT/cli"

PROJECT="${PROJECT:-sparkles-v100}"

# Run as the project's sprinkles service account (the key written by
# setup-control-project.sh) rather than the developer's own gcloud identity:
# only the service account is allowed to act as itself when creating Batch jobs.
export GOOGLE_APPLICATION_CREDENTIALS="${GOOGLE_APPLICATION_CREDENTIALS:-${ROOT}/sprinkles-sa-key.json}"
if [ ! -f "${GOOGLE_APPLICATION_CREDENTIALS}" ]; then
  echo "Service account key not found at ${GOOGLE_APPLICATION_CREDENTIALS}" >&2
  echo "Run setup-control-project.sh, or set GOOGLE_APPLICATION_CREDENTIALS to the key." >&2
  exit 1
fi

NODE_MODULES="${ROOT}/dashboard/node_modules"
if [ ! -d "${NODE_MODULES}" ] \
  || [ "${ROOT}/dashboard/package.json" -nt "${NODE_MODULES}" ] \
  || [ "${ROOT}/dashboard/package-lock.json" -nt "${NODE_MODULES}" ]; then
  echo "Installing frontend dependencies..."
  ( cd "${ROOT}/dashboard" && npm ci )
fi

echo "Building sprinkles..."
mkdir -p "${ROOT}/bin"
go build -o "${ROOT}/bin/sprinkles" ./cmd/sprinkles

echo "Loading config"
"${ROOT}/bin/sprinkles" dev set-config sample-config.json --project "${PROJECT}"

SPRINKLES="${ROOT}/bin/sprinkles"

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
