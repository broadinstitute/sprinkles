#!/usr/bin/env bash
# Prepares a *target* GCP project (T) so that sprinkles, running in a separate
# control-plane project (C, set up by setup-control-project.sh), can run
# workpools there (workpool.projectID = T). It:
#   - enables the APIs Batch workloads need on T,
#   - creates a worker service account in T (the identity worker VMs run as; a
#     T-local SA avoids iam.disableCrossProjectServiceAccountUsage problems that
#     attaching C's service account to VMs in T would hit),
#   - grants C's sprinkles service account what it needs in T to create/manage
#     Batch jobs and write to GCS, and to act as the worker SA,
#   - grants the worker SA access back to the control plane in C (Firestore,
#     Pub/Sub, GCS), and
#   - lets T's Batch service agent publish state changes to C's topic.
#
# Run with an identity that has admin access on both projects.
#
# Usage: setup-target-project.sh --sprinkles-project <C> --target-project <T> [--service-account-name <name>] [--worker-service-account-name <name>]
set -euo pipefail

SERVICE_ACCOUNT_NAME="sprinkles"
WORKER_SERVICE_ACCOUNT_NAME="sprinkles-worker"
SPRINKLES_PROJECT=""
TARGET_PROJECT=""

USAGE="usage: setup-target-project.sh --sprinkles-project <C> --target-project <T> [--service-account-name <name>] [--worker-service-account-name <name>]"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --sprinkles-project) SPRINKLES_PROJECT="$2"; shift 2 ;;
    --target-project) TARGET_PROJECT="$2"; shift 2 ;;
    --service-account-name) SERVICE_ACCOUNT_NAME="$2"; shift 2 ;;
    --worker-service-account-name) WORKER_SERVICE_ACCOUNT_NAME="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 1 ;;
  esac
done

if [[ -z "${SPRINKLES_PROJECT}" || -z "${TARGET_PROJECT}" ]]; then
  echo "${USAGE}" >&2
  exit 1
fi

SA_EMAIL="${SERVICE_ACCOUNT_NAME}@${SPRINKLES_PROJECT}.iam.gserviceaccount.com"
WORKER_SA_EMAIL="${WORKER_SERVICE_ACCOUNT_NAME}@${TARGET_PROJECT}.iam.gserviceaccount.com"

# Retries a command a few times with backoff, tolerating IAM propagation lag
# for newly created service accounts and service agents.
retry() {
  local tries=8 count=0 wait=2
  until "$@"; do
    count=$((count + 1))
    if [[ "${count}" -ge "${tries}" ]]; then
      echo "    giving up after ${tries} attempts: $*" >&2
      return 1
    fi
    echo "    retrying in ${wait}s..."
    sleep "${wait}"
    wait=$((wait * 2))
  done
}

# Grants roles on a project to a member, e.g. grant_project_roles <project> <member> <role>...
grant_project_roles() {
  local project="$1" member="$2" role
  shift 2
  for role in "$@"; do
    retry gcloud projects add-iam-policy-binding "${project}" \
      --member="${member}" \
      --role="${role}" \
      --condition=None \
      >/dev/null
    echo "    granted ${role} on ${project}"
  done
}

echo "==> Checking that ${SA_EMAIL} exists..."
if ! gcloud iam service-accounts describe "${SA_EMAIL}" --project="${SPRINKLES_PROJECT}" >/dev/null 2>&1; then
  echo "    ${SA_EMAIL} not found; run setup-control-project.sh for ${SPRINKLES_PROJECT} first" >&2
  exit 1
fi

echo "==> Enabling required APIs on ${TARGET_PROJECT}..."
gcloud services enable \
  batch.googleapis.com \
  compute.googleapis.com \
  logging.googleapis.com \
  storage.googleapis.com \
  iam.googleapis.com \
  --project="${TARGET_PROJECT}"

# Batch never sets a network, so worker VMs need the "default" VPC (see
# setup-control-project.sh).
echo "==> Ensuring a default VPC network exists on ${TARGET_PROJECT}..."
if gcloud compute networks describe default --project="${TARGET_PROJECT}" >/dev/null 2>&1; then
  echo "    already exists"
else
  gcloud compute networks create default \
    --project="${TARGET_PROJECT}" \
    --subnet-mode=auto \
    --bgp-routing-mode=regional
fi

# SSH via IAP tunneling only; for debugging access (see setup-control-project.sh).
echo "==> Ensuring IAP-tunneled SSH firewall rule exists..."
if gcloud compute firewall-rules describe allow-ssh-from-iap --project="${TARGET_PROJECT}" >/dev/null 2>&1; then
  echo "    already exists"
else
  gcloud compute firewall-rules create allow-ssh-from-iap \
    --project="${TARGET_PROJECT}" \
    --network=default \
    --direction=INGRESS \
    --action=ALLOW \
    --rules=tcp:22 \
    --source-ranges=35.235.240.0/20
fi

echo "==> Creating worker service account ${WORKER_SA_EMAIL}..."
if gcloud iam service-accounts describe "${WORKER_SA_EMAIL}" --project="${TARGET_PROJECT}" >/dev/null 2>&1; then
  echo "    already exists"
else
  gcloud iam service-accounts create "${WORKER_SERVICE_ACCOUNT_NAME}" \
    --project="${TARGET_PROJECT}" \
    --display-name="sprinkles worker"
fi

echo "==> Waiting for ${WORKER_SA_EMAIL} to become usable..."
for i in $(seq 1 30); do
  if gcloud iam service-accounts describe "${WORKER_SA_EMAIL}" --project="${TARGET_PROJECT}" >/dev/null 2>&1; then
    echo "    ready"
    break
  fi
  if [[ "${i}" -eq 30 ]]; then
    echo "    timed out waiting for ${WORKER_SA_EMAIL} to become usable" >&2
    exit 1
  fi
  sleep 2
done

# Mirrors the runtime permissions the control-plane SA uses in its own project
# (create/get/cancel Batch jobs, list/delete their VMs, fetch batch logs, GCS).
echo "==> Granting roles on ${TARGET_PROJECT} to ${SA_EMAIL}..."
grant_project_roles "${TARGET_PROJECT}" "serviceAccount:${SA_EMAIL}" \
  roles/batch.jobsEditor \
  roles/compute.instanceAdmin.v1 \
  roles/logging.viewer \
  roles/storage.admin

# Creating a Batch job with AllocationPolicy.ServiceAccount set requires
# "act as" permission on that service account.
echo "==> Granting roles/iam.serviceAccountUser on ${WORKER_SA_EMAIL} to ${SA_EMAIL}..."
retry gcloud iam service-accounts add-iam-policy-binding "${WORKER_SA_EMAIL}" \
  --project="${TARGET_PROJECT}" \
  --member="serviceAccount:${SA_EMAIL}" \
  --role="roles/iam.serviceAccountUser" \
  >/dev/null

echo "==> Granting roles on ${TARGET_PROJECT} to ${WORKER_SA_EMAIL}..."
grant_project_roles "${TARGET_PROJECT}" "serviceAccount:${WORKER_SA_EMAIL}" \
  roles/batch.agentReporter \
  roles/logging.logWriter \
  roles/storage.objectAdmin

# Workers claim tasks and report state against the control plane's Firestore
# and Pub/Sub, and write results/logs to GCS there.
echo "==> Granting control-plane roles on ${SPRINKLES_PROJECT} to ${WORKER_SA_EMAIL}..."
grant_project_roles "${SPRINKLES_PROJECT}" "serviceAccount:${WORKER_SA_EMAIL}" \
  roles/datastore.user \
  roles/pubsub.editor \
  roles/storage.objectAdmin

# Batch state-change notifications are published to the control plane's topic
# by the target project's Batch service agent. Without this the monitor falls
# back to (slower) timer polling.
echo "==> Granting T's Batch service agent publish rights on ${SPRINKLES_PROJECT}'s batch-api-notifications topic..."
gcloud beta services identity create --service=batch.googleapis.com --project="${TARGET_PROJECT}" >/dev/null
TARGET_PROJECT_NUMBER="$(gcloud projects describe "${TARGET_PROJECT}" --format='value(projectNumber)')"
BATCH_AGENT="service-${TARGET_PROJECT_NUMBER}@gcp-sa-cloudbatch.iam.gserviceaccount.com"
if gcloud pubsub topics describe batch-api-notifications --project="${SPRINKLES_PROJECT}" >/dev/null 2>&1; then
  retry gcloud pubsub topics add-iam-policy-binding batch-api-notifications \
    --project="${SPRINKLES_PROJECT}" \
    --member="serviceAccount:${BATCH_AGENT}" \
    --role="roles/pubsub.publisher" \
    >/dev/null
  echo "    granted roles/pubsub.publisher to ${BATCH_AGENT}"
else
  echo "    WARNING: topic batch-api-notifications not found in ${SPRINKLES_PROJECT}; run" >&2
  echo "    'sprinkles dev bootstrap-project' there, then re-run this script." >&2
fi

cat <<EOF

Done. To run a workpool in ${TARGET_PROJECT}, submit it with:

  workpool.projectID = ${TARGET_PROJECT}
  workpool.serviceAccount = ${WORKER_SA_EMAIL}
EOF
