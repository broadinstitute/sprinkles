#!/usr/bin/env bash
#
# Exercises iam.disableCrossProjectServiceAccountUsage against Google Cloud Batch.
#
#   Project A: sparkles-v100            -> S@A = test-batch-caller
#   Project B: depmap-portal-pipeline   -> S@B = test-batch-runner
#
# The constraint is enforced on project A (the project that OWNS S@A).
#
# Tests (all run as S@A, via impersonation from your gcloud identity):
#   T1  Submit a Batch job in B with S@B attached          -> expect SUCCESS
#       (same-project attachment; S@A is only the caller)
#   T2  S@A mints an access token for S@B (generateAccessToken)
#                                                          -> expect SUCCESS
#       (impersonation is pure IAM, unaffected by the constraint)
#   T3  Submit a Batch job in B with S@A attached          -> expect BLOCKED
#       (cross-project attachment; this is what the constraint stops)
#
# Usage:
#   ./test_cross_project_batch.sh            # setup + run tests
#   ./test_cross_project_batch.sh setup
#   ./test_cross_project_batch.sh test
#   ./test_cross_project_batch.sh cleanup    # delete jobs, bindings, policy, service accounts
#
# Env overrides:
#   REGION=us-central1  PROPAGATION_WAIT=60  FORCE=1
#
# Requirements for the identity running this script (needs gcloud auth login):
#   - Permission to create service accounts and set IAM policy in both projects
#   - roles/orgpolicy.policyAdmin on project A (to set the constraint)
#   - Permission to enable APIs in both projects
#
set -uo pipefail

PROJECT_A="sparkles-v100"
PROJECT_B="depmap-portal-pipeline"
REGION="${REGION:-us-central1}"
PROPAGATION_WAIT="${PROPAGATION_WAIT:-60}"

SA_A_NAME="test-batch-caller"
SA_B_NAME="test-batch-runner"
SA_A="${SA_A_NAME}@${PROJECT_A}.iam.gserviceaccount.com"
SA_B="${SA_B_NAME}@${PROJECT_B}.iam.gserviceaccount.com"

CONSTRAINT="iam.disableCrossProjectServiceAccountUsage"
JOB_PREFIX="test-xproj"
RUN_ID="$(date +%Y%m%d-%H%M%S)"
POLICY_MARKER=".xproj-test-created-policy"

RESULTS=()
FAILED=0

log()  { printf '\n==> %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
quiet() { "$@" >/dev/null 2>&1; }

caller_member() {
  local me
  me="$(gcloud config get-value account 2>/dev/null)"
  if [[ "$me" == *gserviceaccount.com ]]; then
    echo "serviceAccount:${me}"
  else
    echo "user:${me}"
  fi
}

record() { # name, PASS|FAIL|INCONCLUSIVE, detail
  RESULTS+=("$2  $1  -  $3")
  [[ "$2" == "PASS" ]] || FAILED=1
}

# ---------------------------------------------------------------- setup ----

create_sa() { # project, name
  if quiet gcloud iam service-accounts describe "${2}@${1}.iam.gserviceaccount.com" --project="$1"; then
    note "exists: ${2}@${1}"
  else
    gcloud iam service-accounts create "$2" --project="$1" \
      --display-name="TEST cross-project batch (safe to delete)" --quiet
  fi
}

project_binding() { # project, member, role
  gcloud projects add-iam-policy-binding "$1" --member="$2" --role="$3" \
    --condition=None --quiet >/dev/null
  note "$1: $2 -> $3"
}

sa_binding() { # sa email, project, member, role
  gcloud iam service-accounts add-iam-policy-binding "$1" --project="$2" \
    --member="$3" --role="$4" --quiet >/dev/null
  note "$1: $3 -> $4"
}

enforce_constraint() {
  if gcloud org-policies describe "$CONSTRAINT" --project="$PROJECT_A" >/dev/null 2>&1; then
    if [[ ! -f "$POLICY_MARKER" && "${FORCE:-0}" != "1" ]]; then
      echo "ERROR: $PROJECT_A already has a project-level policy for $CONSTRAINT."
      echo "       Re-run with FORCE=1 to overwrite it (cleanup will then DELETE it,"
      echo "       reverting $PROJECT_A to inherited behavior)."
      exit 1
    fi
  fi
  local f; f="$(mktemp)"
  cat >"$f" <<EOF
name: projects/${PROJECT_A}/policies/${CONSTRAINT}
spec:
  rules:
  - enforce: true
EOF
  gcloud org-policies set-policy "$f" --quiet
  rm -f "$f"
  touch "$POLICY_MARKER"
}

do_setup() {
  local me; me="$(caller_member)"

  log "Enabling APIs"
  gcloud services enable iam.googleapis.com iamcredentials.googleapis.com \
    --project="$PROJECT_A" --quiet
  gcloud services enable batch.googleapis.com compute.googleapis.com \
    logging.googleapis.com iam.googleapis.com iamcredentials.googleapis.com \
    --project="$PROJECT_B" --quiet

  log "Creating service accounts"
  create_sa "$PROJECT_A" "$SA_A_NAME"
  create_sa "$PROJECT_B" "$SA_B_NAME"
  note "waiting 15s for service accounts to propagate"
  sleep 15

  log "Granting IAM"
  # You -> can impersonate S@A (so the script can act as S@A)
  sa_binding "$SA_A" "$PROJECT_A" "$me" roles/iam.serviceAccountTokenCreator
  # S@A -> create Batch jobs in B, bill API usage to B
  project_binding "$PROJECT_B" "serviceAccount:${SA_A}" roles/batch.jobsEditor
  project_binding "$PROJECT_B" "serviceAccount:${SA_A}" roles/serviceusage.serviceUsageConsumer
  # S@A -> actAs S@B (cross-project grant; required to attach S@B to a job)
  sa_binding "$SA_B" "$PROJECT_B" "serviceAccount:${SA_A}" roles/iam.serviceAccountUser
  # S@A -> mint tokens for S@B (T2)
  sa_binding "$SA_B" "$PROJECT_B" "serviceAccount:${SA_A}" roles/iam.serviceAccountTokenCreator
  # S@A -> actAs itself, so T3 fails because of the constraint and not a missing actAs
  sa_binding "$SA_A" "$PROJECT_A" "serviceAccount:${SA_A}" roles/iam.serviceAccountUser
  # Batch agent role for any service account attached to a job
  project_binding "$PROJECT_B" "serviceAccount:${SA_B}" roles/batch.agentReporter
  project_binding "$PROJECT_B" "serviceAccount:${SA_A}" roles/batch.agentReporter
  project_binding "$PROJECT_B" "serviceAccount:${SA_B}" roles/logging.logWriter

  log "Waiting ${PROPAGATION_WAIT}s for IAM / org policy propagation"
  sleep "$PROPAGATION_WAIT"
}

# ---------------------------------------------------------------- tests ----

submit_job() { # job name, service account email to attach
  local name="$1" job_sa="$2" cfg rc
  cfg="$(mktemp)"
  cat >"$cfg" <<EOF
{
  "taskGroups": [{
    "taskCount": 1,
    "taskSpec": {
      "runnables": [{"script": {"text": "echo hello from ${name}"}}],
      "computeResource": {"cpuMilli": 1000, "memoryMib": 512},
      "maxRetryCount": 0,
      "maxRunDuration": "120s"
    }
  }],
  "allocationPolicy": {
    "instances": [{"policy": {"machineType": "e2-small"}}],
    "serviceAccount": {"email": "${job_sa}"}
  },
  "logsPolicy": {"destination": "CLOUD_LOGGING"}
}
EOF
  gcloud batch jobs submit "$name" \
    --project="$PROJECT_B" --location="$REGION" --config="$cfg" \
    --impersonate-service-account="$SA_A" --billing-project="$PROJECT_B" 2>&1
  rc=$?
  rm -f "$cfg"
  return $rc
}

test_t1() {
  log "T1: S@A submits job in B with S@B attached (expect SUCCESS)"
  local out attempt
  for attempt in 1 2 3; do
    out="$(submit_job "${JOB_PREFIX}-t1-${RUN_ID}" "$SA_B")" && {
      record "T1 same-project attach (S@B in B)" PASS "job created"
      return
    }
    note "attempt ${attempt} failed; retrying in 30s (IAM may still be propagating)"
    sleep 30
  done
  note "$out"
  record "T1 same-project attach (S@B in B)" FAIL "job creation rejected (see output above)"
}

test_t2() {
  log "T2: S@A calls generateAccessToken on S@B (expect SUCCESS)"
  local tok resp attempt
  for attempt in 1 2 3; do
    tok="$(gcloud auth print-access-token --impersonate-service-account="$SA_A" 2>/dev/null)"
    resp="$(curl -sS -X POST \
      -H "Authorization: Bearer ${tok}" \
      -H "x-goog-user-project: ${PROJECT_B}" \
      -H "Content-Type: application/json" \
      -d '{"scope":["https://www.googleapis.com/auth/cloud-platform"]}' \
      "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/${SA_B}:generateAccessToken")"
    if grep -q '"accessToken"' <<<"$resp"; then
      record "T2 cross-project token minting (S@A -> S@B)" PASS "token issued"
      return
    fi
    note "attempt ${attempt} failed; retrying in 30s"
    sleep 30
  done
  note "$resp"
  record "T2 cross-project token minting (S@A -> S@B)" FAIL "token not issued (see output above)"
}

test_t3() {
  log "T3: S@A attached to a job in B (expect BLOCKED by ${CONSTRAINT})"
  local out
  if out="$(submit_job "${JOB_PREFIX}-t3-${RUN_ID}" "$SA_A")"; then
    note "$out"
    record "T3 cross-project attach (S@A in B)" FAIL \
      "job was CREATED; constraint not in force yet or not effective. Wait a few minutes and run: $0 test"
  elif grep -qiE "disableCrossProjectServiceAccountUsage|cross-project|different project|constraints/iam" <<<"$out"; then
    note "$(head -n 6 <<<"$out")"
    record "T3 cross-project attach (S@A in B)" PASS "blocked by org policy constraint"
  else
    note "$out"
    record "T3 cross-project attach (S@A in B)" INCONCLUSIVE \
      "rejected, but the error doesn't mention the constraint; inspect the output above"
    FAILED=1
  fi
}

do_test() {
  log "Effective policy for ${CONSTRAINT} on ${PROJECT_A}"
  gcloud org-policies describe "$CONSTRAINT" --effective --project="$PROJECT_A" 2>&1 | sed 's/^/    /'

  test_t1
  test_t2
  test_t3

  log "Summary"
  printf '    %s\n' "${RESULTS[@]}"
  echo
  note "Jobs created are named ${JOB_PREFIX}-*; service accounts are test-*."
  note "Remove everything with: $0 cleanup"
  exit "$FAILED"
}

# -------------------------------------------------------------- cleanup ----

do_cleanup() {
  log "Deleting Batch jobs (${JOB_PREFIX}-*) in ${PROJECT_B}"
  local j
  for j in $(gcloud batch jobs list --project="$PROJECT_B" --location="$REGION" \
              --filter="name~${JOB_PREFIX}-" --format='value(name)' 2>/dev/null); do
    gcloud batch jobs delete "$(basename "$j")" --project="$PROJECT_B" \
      --location="$REGION" --quiet >/dev/null 2>&1 && note "deleted $(basename "$j")"
  done

  if [[ -f "$POLICY_MARKER" ]]; then
    log "Removing project-level ${CONSTRAINT} policy on ${PROJECT_A}"
    gcloud org-policies delete "$CONSTRAINT" --project="$PROJECT_A" --quiet || true
    rm -f "$POLICY_MARKER"
  else
    note "no policy marker found; leaving org policy on ${PROJECT_A} untouched"
  fi

  log "Removing project IAM bindings in ${PROJECT_B}"
  local role
  for role in roles/batch.jobsEditor roles/serviceusage.serviceUsageConsumer roles/batch.agentReporter; do
    gcloud projects remove-iam-policy-binding "$PROJECT_B" \
      --member="serviceAccount:${SA_A}" --role="$role" --condition=None --quiet >/dev/null 2>&1 || true
  done
  for role in roles/batch.agentReporter roles/logging.logWriter; do
    gcloud projects remove-iam-policy-binding "$PROJECT_B" \
      --member="serviceAccount:${SA_B}" --role="$role" --condition=None --quiet >/dev/null 2>&1 || true
  done

  log "Deleting service accounts"
  gcloud iam service-accounts delete "$SA_A" --project="$PROJECT_A" --quiet 2>&1 | sed 's/^/    /' || true
  gcloud iam service-accounts delete "$SA_B" --project="$PROJECT_B" --quiet 2>&1 | sed 's/^/    /' || true

  log "Cleanup done"
}

# ----------------------------------------------------------------- main ----

case "${1:-all}" in
  setup)   do_setup ;;
  test)    do_test ;;
  cleanup) do_cleanup ;;
  all)     do_setup; do_test ;;
  *)       echo "usage: $0 [setup|test|cleanup|all]"; exit 2 ;;
esac
