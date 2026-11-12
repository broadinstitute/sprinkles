#!/usr/bin/env bash
# Builds the sprinkles binary (via build.sh) and uploads it to the fixed GCS
# path worker VMs bootstrap from -- see the "sprinklesWorkerGCSPath" field in
# SprinklesConfig/workpool specs (sample-config.json, sample-workpool.json)
# and monitor/batch_api.go, which `gcloud storage cp`s this exact path onto
# each worker VM at startup.
#
# Usage: upload-worker-binary.sh [--gcs-prefix <prefix>] [--update-dev-cds-team] [version] [gcs-path]
#   --update-dev-cds-team additionally installs the binary on dev.cds.team
#     (via scp/ssh) and repoints /usr/local/bin/sprinkles at it; without the
#     flag, only the GCS upload happens.
#   version defaults to `git describe --tags --always --dirty`.
#   gcs-path defaults to:
#     - the explicit [gcs-path] argument, if given
#     - "<prefix>/sprinkles-linux-amd64-<version>", if --gcs-prefix was given
#       (e.g. the "gs://<bucket>/bin" prefix "sprinkles dev bootstrap-project"
#       prints)
#     - $GCS_PATH, or the sample config's path, otherwise
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

GCS_PREFIX=""
UPDATE_DEV_CDS_TEAM=0
ARGS=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --gcs-prefix) GCS_PREFIX="$2"; shift 2 ;;
    --update-dev-cds-team) UPDATE_DEV_CDS_TEAM=1; shift ;;
    *) ARGS+=("$1"); shift ;;
  esac
done
set -- "${ARGS[@]+"${ARGS[@]}"}"

VERSION=${1:-$(git -C "${REPO_ROOT}" describe --tags --always --dirty)}
if [[ -n "${GCS_PREFIX}" ]]; then
  DEFAULT_GCS_PATH="${GCS_PREFIX%/}/sprinkles-linux-amd64-${VERSION}"
else
  DEFAULT_GCS_PATH=${GCS_PATH:-gs://sparkles-v100/bin/sprinkles-linux-amd64-${VERSION}}
fi
GCS_PATH=${2:-${DEFAULT_GCS_PATH}}

"${REPO_ROOT}/build.sh" "${VERSION}"

BIN_NAME="sprinkles-linux-amd64-${VERSION}"
OUTPUT="${REPO_ROOT}/bin/${BIN_NAME}"
echo "Uploading ${OUTPUT} to ${GCS_PATH}..."
gcloud storage cp "${OUTPUT}" "${GCS_PATH}"

if [[ "${UPDATE_DEV_CDS_TEAM}" == 1 ]]; then
  echo "uploading to dev.cds.team"
  scp -C "${OUTPUT}" "ubuntu@dev.cds.team:/tmp/${BIN_NAME}"
  ssh ubuntu@dev.cds.team sudo cp "/tmp/${BIN_NAME}" "/usr/local/bin/${BIN_NAME}"
  ssh ubuntu@dev.cds.team sudo rm -f /usr/local/bin/sprinkles
  ssh ubuntu@dev.cds.team sudo ln -s "/usr/local/bin/${BIN_NAME}" /usr/local/bin/sprinkles
fi
