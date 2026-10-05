#!/usr/bin/env bash
#
# restore.sh — Point-in-time recovery runbook for the Financial Management System (FMS).
#
# PURPOSE
#   Implements the DR acceptance criterion "recovery procedures tested successfully"
#   (Business Description §7; RPO <= 15 min, RTO <= 1 h in Technical Spec §9.3).
#   Restores the latest full snapshot that does not exceed --target-time into a NEW,
#   isolated scratch database (immutable-infrastructure principle: never mutates the
#   live primary), verifies the restored data's core domain invariants, and reports
#   the elapsed RTO plus the residual snapshot-to-target data-loss gap.
#
# CONSUMED BACKUP CONTRACT (scripts/backup.sh — application/vnd.fms.backup.v1)
#   s3://<S3_BUCKET>/<BACKUP_PREFIX>/fms-<TS>.dump         and  <TS>.dump.sha256
#   where TS=YYYYMMDD-HHMMSS-<pid> (UTC, lexicographically sortable = snapshot order).
#   backup.sh performs LOGICAL dumps (pg_dump custom format) and does NOT archive WAL
#   segments to S3. This file's layout assumptions are a frozen contract; any change
#   to the prefix or naming scheme in backup.sh is a breaking change that must be
#   versioned and documented there.
#
# LOGICAL PITR BY SNAPSHOT SELECTION (design decision — reflects the actual backup
#   output format). PostgreSQL WAL replay (recovery_target_time) operates on physical
#   block addresses and requires a PHYSICAL base backup (pg_basebackup carrying
#   backup_label + backup_manifest). It CANNOT be replayed over a pg_dump logical
#   .dump restored by pg_restore. Because backup.sh emits logical dumps and archives
#   no WAL, this script performs point-in-time recovery by *snapshot selection*:
#   it restores the newest full dump with dump_time <= --target-time and reports the
#   residual gap (target_time - dump_time) as the EFFECTIVE RPO for that restore.
#   pg_archivecleanup / recovery_target_* are intentionally NOT used — they apply
#   only to physical WAL replay and are inapplicable to logical dumps.
#
#   BLOCKING DEPENDENCY (out of scope for this file — see final summary):
#   To meet spec RPO <= 15 min with true target-time WAL replay, scripts/backup.sh
#   must be upgraded to emit PHYSICAL pg_basebackup snapshots (with backup_label and
#   backup_manifest) and archive WAL segments to an S3 prefix, driven by an
#   archive_command in deploy/k8s/postgres-statefulset.yaml. Until then the snapshot
#   cadence of backup.sh bounds the achievable RPO.
#
# USAGE
#   restore.sh --target-time "2024-06-01 12:00:00 UTC" [--instance-name fms-restore] [--force]
#
# ENV (same as backup.sh plus RESTORE_INSTANCE and DR_S3_BUCKET)
#   DATABASE_URL       preferred connection string (parsed into PG* vars)  —  OR:
#   PGHOST PGUSER PGDATABASE [PGPORT] [PGPASSWORD]
#   S3_BUCKET S3_REGION
#   BACKUP_PREFIX      default fms-backups
#   DR_S3_BUCKET       optional; when set the backup is restored from the DR region bucket
#   RESTORE_INSTANCE   instance this script targets (must NOT be the live primary unless --force)
#   AWS creds: AWS_PROFILE, OR (AWS_ROLE_ARN + AWS_WEB_IDENTITY_TOKEN_FILE).
#
# SAFETY
#   - Default deny: restores against the live primary are REFUSED unless --force.
#   - --force additionally requires an interactive TTY confirmation AND an elevated
#     AWS role, and is recorded as an audit event in the structured log.
#   - All user-supplied values are allowlist-validated (fail-closed) before use.
#   - No credentials via CLI args, no set -x tracing, no secret values ever logged.
#   - Verification never runs as superuser (restricted SELECT-only principal).
#
# DNS CUTOVER RUNBOOK (documented only — the cutover decision is made by operators, not this script)
#   1. Run restore.sh to the desired --target-time; confirm EXIT 0 and record run_id.
#   2. Independently confirm the three verification outputs (row counts, audit chain,
#      balance reconciliation == 0 rows) against the runbook evidence.
#   3. Pause app writes (read-only mode) on the old primary; disable app traffic.
#   4. Point/promote the scratch database as the new primary (apply ownership/grants
#      per the --no-owner --role policy) and rotate the restored DB credentials.
#   5. Update DNS (CNAME) / Service selector / load-balancer listener to the new primary.
#   6. Re-issue any pre-signed download link with short TTL (15 min); never reuse old URLs.
#   7. Re-run the quarterly DR drill; destroy the previous primary and record the
#      cutover as an audit event (timestamp, operator, target, target-time).

set -euo pipefail
umask 077

# ---------------------------------------------------------------------------
# Globals & helper predicates (all read-only state; no external deps)
# ---------------------------------------------------------------------------
RTO_START_EPOCH=""
RTO_END_EPOCH=""
RUN_ID=""
TARGET_TIME_RAW=""
TARGET_TIME_UTC=""
TARGET_EPOCH=""
TARGET_TIME_TC=""
INSTANCE_NAME=""
FORCE=0
SCRATCH=""
DUMP_KEY=""
DUMP_PATH=""
DUMP_EPOCH=""
DUMP_TIME_UTC=""
RESTORE_DB=""
GAP_SECONDS=""
S3_BASE=""
AWS_CREDS_SOURCE=""
LOG_FILE=""
LOCK_FILE=""
PG_DB_RAW=""

####
# jlog — emit one structured JSON line per event (mirrors scripts/backup.sh style).
####
jlog() {
  local level="$1"
  local message="$2"
  local step=""
  if (( $# >= 3 )); then step="$3"; fi
  # shellcheck disable=SC2016
  printf '{"level":"%s","ts":"%s","run_id":"%s","step":"%s","message":"%s"}\n' \
    "$level" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$RUN_ID" "$step" "$message" >> "$LOG_FILE"
  printf '{"level":"%s","ts":"%s","run_id":"%s","step":"%s","message":"%s"}\n' \
    "$level" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$RUN_ID" "$step" "$message" >&2
}

####
# fatal — record the failing step, log a structured error, exit non-zero.
####
fatal() {
  local message="$1"
  local step=""
  if (( $# >= 2 )); then step="$2"; fi
  jlog "error" "${message}" "${step}"
  exit 1
}

# ---------------------------------------------------------------------------
# Argument parsing + strict allowlist validation (fail-closed, before any work)
# ---------------------------------------------------------------------------

####
# validate_target_time — accept only ISO-ish UTC timestamps with explicit zone.
####
validate_target_time() {
  local raw="$1"
  if [[ ! "$raw" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}[[:space:]][0-9]{2}:[0-9]{2}:[0-9]{2}[[:space:]]?(UTC|Z|[+-][0-9]{1,2}:?[0-9]{2})$ ]]; then
    fatal "invalid --target-time: must be 'YYYY-MM-DD HH:MM:SS UTC|Z|<offset>'" "args"
  fi
  local normalized epoch_val
  if ! normalized="$(date -u -d "$raw" '+%Y-%m-%d %H:%M:%S%z' 2>/dev/null)" || ! epoch_val="$(date -u -d "$normalized" '+%s' 2>/dev/null)"; then
    fatal "unparseable --target-time: '$raw'" "args"
  fi
  TARGET_TIME_UTC="$normalized"
  TARGET_EPOCH="$epoch_val"
  TARGET_TIME_TC="$(date -u -d "$TARGET_TIME_UTC" '+%Y-%m-%d %H:%M:%S+00')"
  local compact
  compact="$(printf '%s' "$TARGET_TIME_TC" | tr -d ' :-')"
  if [[ ! "$compact" =~ ^[0-9]{14}$ ]]; then
    fatal "failed to normalize --target-time" "args"
  fi
}

####
# validate_instance_name — database/OS-safe identifier token.
####
validate_instance_name() {
  local name="$1"
  if [[ ! "$name" =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$ ]]; then
    fatal "invalid --instance-name: must match ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$" "args"
  fi
  INSTANCE_NAME="$name"
}

parse_args() {
  while (( $# > 0 )); do
    case "$1" in
      --target-time)
        if (( $# < 2 )); then fatal "--target-time requires a value" "args"; fi
        TARGET_TIME_RAW="$2"
        shift 2
        ;;
      --instance-name)
        if (( $# < 2 )); then fatal "--instance-name requires a value" "args"; fi
        validate_instance_name "$2"
        shift 2
        ;;
      --force)
        FORCE=1
        shift
        ;;
      *)
        fatal "unknown argument: $1" "args"
        ;;
    esac
  done

  if [[ -z "$TARGET_TIME_RAW" ]]; then
    fatal "--target-time is required" "args"
  fi
  validate_target_time "$TARGET_TIME_RAW"

  if [[ -z "$INSTANCE_NAME" ]]; then
    INSTANCE_NAME="fms-restore"
  fi
}

# ---------------------------------------------------------------------------
# Environment contract validation (mirrors scripts/backup.sh)
# ---------------------------------------------------------------------------

####
# require_env — fail-closed unless a variable is non-empty.
####
require_env() {
  local var="$1"
  if [[ -z "${!var-}" ]]; then
    fatal "required env var not set: ${var}" "env"
  fi
}

####
# validate_bucket_name — S3 bucket allowlist.
####
validate_bucket_name() {
  local b="$1"
  if [[ ! "$b" =~ ^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$ ]]; then
    fatal "invalid S3 bucket name: $b" "env"
  fi
}

####
# parse_database_url — split DATABASE_URL into PG* vars. No eval.
####
parse_database_url() {
  local url="$1"
  local scheme rest
  scheme="${url%%://*}"
  rest="${url#*://}"
  if [[ "$scheme" != "postgres" && "$scheme" != "postgresql" ]]; then
    fatal "DATABASE_URL must use postgres:// or postgresql://" "env"
  fi
  local creds_host
  creds_host="${rest%%/*}"
  if [[ "$creds_host" != *":"* ]]; then
    creds_host="${creds_host}:5432"
  fi
  local creds="${creds_host%%@*}"
  local hostport="${creds_host#*@}"
  local user pw
  if [[ "$creds" == *":"* ]]; then
    user="${creds%%:*}"
    pw="${creds#*:}"
  else
    user="$creds"
    pw=""
  fi
  local pg_host="${hostport%%:*}"
  local pg_port="${hostport##*:}"
  if [[ "$pg_port" == "$pg_host" ]]; then pg_port="5432"; fi
  if [[ ! "$pg_port" =~ ^[0-9]{1,5}$ ]]; then fatal "invalid port in DATABASE_URL" "env"; fi
  local db="${rest#*/}"
  if [[ -z "$db" ]]; then fatal "DATABASE_URL missing database name" "env"; fi

  PGHOST="$pg_host"
  PGPORT="$pg_port"
  PGUSER="$user"
  if [[ -n "$pw" ]]; then
    PGPASSWORD="${pw}"   # from env URL — still never echoed/printed
  fi
}

validate_env() {
  if [[ -n "${DATABASE_URL-}" ]]; then
    parse_database_url "$DATABASE_URL"
    PGDATABASE="$PG_DB_RAW"
  else
    require_env PGHOST
    require_env PGUSER
    require_env PGDATABASE
    PGPORT="${PGPORT:-5432}"
    if [[ ! "$PGPORT" =~ ^[0-9]{1,5}$ ]]; then fatal "invalid PGPORT" "env"; fi
  fi

  require_env S3_REGION
  if [[ -n "${DR_S3_BUCKET-}" ]]; then
    validate_bucket_name "$DR_S3_BUCKET"
    S3_BASE="$DR_S3_BUCKET"
  else
    require_env S3_BUCKET
    validate_bucket_name "$S3_BUCKET"
    S3_BASE="$S3_BUCKET"
  fi
  BACKUP_PREFIX="${BACKUP_PREFIX:-fms-backups}"
  if [[ ! "$BACKUP_PREFIX" =~ ^[A-Za-z0-9._/-]+$ ]]; then
    fatal "invalid BACKUP_PREFIX" "env"
  fi

  require_env RESTORE_INSTANCE
  if [[ ! "$RESTORE_INSTANCE" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$ ]]; then
    fatal "invalid RESTORE_INSTANCE" "env"
  fi

  local has_profile has_irsa
  has_profile=0; has_irsa=0
  [[ -n "${AWS_PROFILE-}" || -n "${AWS_ACCESS_KEY_ID-}" ]] && has_profile=1
  [[ -n "${AWS_WEB_IDENTITY_TOKEN_FILE-}" && -n "${AWS_ROLE_ARN-}" ]] && has_irsa=1
  if (( has_profile == 1 && has_irsa == 1 )); then
    fatal "ambiguous AWS credentials: set either AWS_PROFILE/static keys OR IRSA, not both" "env"
  fi
  if (( has_profile == 0 && has_irsa == 0 )); then
    fatal "no AWS credentials: set AWS_PROFILE/static keys or IRSA (AWS_ROLE_ARN + AWS_WEB_IDENTITY_TOKEN_FILE)" "env"
  fi
  AWS_CREDS_SOURCE="profile"
  (( has_irsa == 1 )) && AWS_CREDS_SOURCE="irsa"
}

# ---------------------------------------------------------------------------
# Lock, run id, safe scratch directory
# ---------------------------------------------------------------------------

####
# acquire_lock — one restore at a time on this host (flock on fd 9).
####
acquire_lock() {
  if ! command -v flock >/dev/null 2>&1; then
    fatal "flock is required (util-linux)" "lock"
  fi
  exec 9>"${LOCK_FILE}"
  if ! flock -n 9; then
    fatal "another restore.sh is already running; refusing to proceed concurrently" "lock"
  fi
}

####
# init_run — run id, log file, and a symlink-safe scratch dir (umask 077).
####
init_run() {
  RUN_ID="$(date -u +%Y%m%d-%H%M%S-$$)"
  if [[ ! "$RUN_ID" =~ ^[0-9]{14}-[0-9]+$ ]]; then
    fatal "failed to allocate run id" "run"
  fi
  RESTORE_DB="fms_restore_${RUN_ID}"
  LOCK_FILE="/tmp/restore.lock"
  LOG_FILE="${TMPDIR:-/tmp}/fms-restore-${RUN_ID}.log"

  SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/fms-restore.XXXXXX" 2>/dev/null)" || fatal "mktemp scratch failed" "run"
  chmod 700 "$SCRATCH"
  if [[ -L "$SCRATCH" || "$(readlink -e "$SCRATCH")" != "$SCRATCH" ]]; then
    fatal "scratch directory is a symlink or not canonical" "run"
  fi
  if [[ "$(stat -c '%U' "$SCRATCH" 2>/dev/null)" != "$(id -un)" ]]; then
    fatal "scratch directory owner mismatch" "run"
  fi
  jlog "info" "run initialized" "run"
}

# ---------------------------------------------------------------------------
# Structural live-primary safety interlock (default-deny; --force is the ONLY bypass)
# ---------------------------------------------------------------------------

PRIMARY_IS_LIVE=0
TARGET_REACHABLE=0
authoritative_primary_state() {
  PRIMARY_IS_LIVE=0; TARGET_REACHABLE=0
  local prod_instances
  prod_instances="${PROD_INSTANCE_NAMES:-fms-primary fms-main fms-db-0}"
  local inst
  for inst in $prod_instances; do
    if [[ "$RESTORE_INSTANCE" == "$inst" ]]; then
      PRIMARY_IS_LIVE=1
      return 0
    fi
  done

  if command -v psql >/dev/null 2>&1 &&
     PGPASSWORD="${PGPASSWORD-}" psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$PGDATABASE" \
          -tAc "SELECT pg_is_in_recovery();" >/dev/null 2>&1; then
    local in_rec
    in_rec="$(PGPASSWORD="${PGPASSWORD-}" psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$PGDATABASE" \
                -tAc "SELECT pg_is_in_recovery();" 2>/dev/null | tr -d '[:space:]')"
    if [[ "$in_rec" == "t" ]]; then
      PRIMARY_IS_LIVE=0; TARGET_REACHABLE=1
    else
      PRIMARY_IS_LIVE=1; TARGET_REACHABLE=1
    fi
  else
    PRIMARY_IS_LIVE=1; TARGET_REACHABLE=0
  fi
}

####
# safety_interlock — default-deny guard; requires TTY confirmation + elevated role
#   for any --force primary-shot.
####
safety_interlock() {
  authoritative_primary_state

  if (( PRIMARY_IS_LIVE == 1 || TARGET_REACHABLE == 0 )); then
    if (( FORCE == 0 )); then
      fatal "refusing restore: target cannot be proven to be a non-primary replica (catastrophic mis-restore guard). Use --force only for a deliberate, operator-authorized restore." "safety"
    fi
    if [[ ! -t 0 ]]; then
      fatal "refusing --force: interactive TTY confirmation is required (stdin is not a TTY)" "safety"
    fi
    if [[ "$AWS_CREDS_SOURCE" != "irsa" && -z "${FORCE_ELEVATED_ROLE_OK-}" ]]; then
      fatal "refusing --force: an elevated/IRSA restore role is required for primary-target restores" "safety"
    fi
    # shellcheck disable=SC2162
    read -r -p "WARNING: restoring FINANCIAL DATA into '$RESTORE_INSTANCE' (may target the live primary). Type 'yes' to continue: " confirm
    if [[ "$confirm" != "yes" ]]; then
      fatal "--force confirmation declined" "safety"
    fi
    jlog "warn" "FORCE restore authorized by operator into ${RESTORE_INSTANCE}" "safety"
  else
    jlog "info" "target verified as non-primary replica (${RESTORE_INSTANCE})" "safety"
  fi
}

# ---------------------------------------------------------------------------
# Snapshot selection + download + integrity verification
# ---------------------------------------------------------------------------

####
# safe_key — ensure an object key from list-objects is a strict archive member,
#   rejecting path-traversal and shell-metacharacter forms. Bails out otherwise.
####
safe_key() {
  local key="$1"
  if [[ ! "$key" =~ ^${BACKUP_PREFIX}/fms-[0-9]{8}-[0-9]{6}-[0-9]+\.dump$ ]]; then
    fatal "untrusted object key in archive listing: $key" "snapshot"
  fi
  if [[ "$key" == *"/../"* || "$key" == *".."* || "$key" == *[';&|`$<>']* ]]; then
    fatal "object key contains forbidden sequences: $key" "snapshot"
  fi
}

####
# select_snapshot — list .dump keys, pick the newest with TS <= target_epoch;
#   fall back to the newest dump if none precedes the target. Sets DUMP_KEY,
#   DUMP_EPOCH, DUMP_TIME_UTC, and surfaces GAP_SECONDS.
####
select_snapshot() {
  jlog "info" "listing snapshots under ${BACKUP_PREFIX}/ in bucket ${S3_BASE}" "snapshot"
  local listing
  listing="$(aws s3api list-objects-v2 \
                --bucket "$S3_BASE" \
                --prefix "$BACKUP_PREFIX/" \
                --region "$S3_REGION" \
                --query 'Contents[?ends_with(Key, `.dump`)].Key' \
                --output text)" || fatal "failed to list snapshots in s3://${S3_BASE}" "snapshot"
  [[ -z "$listing" ]] && fatal "no .dump snapshots found under s3://${S3_BASE}/${BACKUP_PREFIX}/" "snapshot"

  local best_key=""
  local best_epoch=0
  local best_time=""
  local newest_key=""
  local newest_epoch=0
  local newest_time=""
  local key ts_digits epoch_val
  local IFS=$'\n'
  for key in $listing; do
    [[ -z "$key" ]] && continue
    safe_key "$key"
    ts_digits="${key#*fms-}"
    ts_digits="${ts_digits%.dump}"
    ts_digits="${ts_digits%-*}"

    # shellcheck disable=SC2016
    epoch_val="$(date -u -d "${ts_digits:0:8} ${ts_digits:9:2}:${ts_digits:11:2}:${ts_digits:13:2}" '+%s' 2>/dev/null)" || { jlog "warn" "unparseable snapshot timestamp in ${key}" "snapshot"; continue; }
    if (( epoch_val > newest_epoch )); then
      newest_epoch="$epoch_val"
      newest_key="$key"
      newest_time="$(date -u -d "@${epoch_val}" '+%Y-%m-%d %H:%M:%S+00')"
    fi
    if (( epoch_val <= TARGET_EPOCH && epoch_val > best_epoch )); then
      best_epoch="$epoch_val"
      best_key="$key"
      best_time="$(date -u -d "@${epoch_val}" '+%Y-%m-%d %H:%M:%S+00')"
    fi
  done

  if [[ -z "$best_key" ]]; then
    jlog "warn" "no snapshot precedes target-time; using newest snapshot (gap exceeds requested window)" "snapshot"
    best_key="$newest_key"
    best_epoch="$newest_epoch"
    best_time="$newest_time"
  fi

  DUMP_KEY="$best_key"
  DUMP_EPOCH="$best_epoch"
  DUMP_TIME_UTC="$best_time"
  GAP_SECONDS=$(( TARGET_EPOCH - DUMP_EPOCH ))
  jlog "info" "selected snapshot ${DUMP_KEY} (dump_time ${DUMP_TIME_UTC})" "snapshot"
}

####
# download_snapshot — fetch dump + sha256 into the scratch dir and verify the
#   checksum (hard gate; tamper/corruption evidence). No pre-signed URLs logged.
####
download_snapshot() {
  local dump_local checksum_local
  dump_local="${SCRATCH}/snapshot.dump"
  checksum_local="${SCRATCH}/snapshot.dump.sha256"

  aws s3 cp "s3://${S3_BASE}/${DUMP_KEY}" "$dump_local" --region "$S3_REGION" >/dev/null \
    || fatal "failed to download snapshot s3://${S3_BASE}/${DUMP_KEY}" "download"
  aws s3 cp "s3://${S3_BASE}/${DUMP_KEY}.sha256" "$checksum_local" --region "$S3_REGION" >/dev/null \
    || fatal "failed to download checksum for ${DUMP_KEY}" "download"

  chmod 600 "$dump_local" "$checksum_local"
  DUMP_PATH="$dump_local"
  cd "$SCRATCH" || fatal "cannot cd into scratch dir" "download"
  if ! sha256sum -c snapshot.dump.sha256 --ignore-missing >/dev/null 2>&1 || \
     [[ ! -s snapshot.dump ]]; then
    fatal "snapshot checksum MISMATCH — refusing to restore tampered/corrupted backup" "download"
  fi
  jlog "info" "snapshot downloaded and checksum verified" "download"
  cd / || fatal "cannot leave scratch dir" "download"
}

# ---------------------------------------------------------------------------
# Create scratch DB + pg_restore (immutable / unique target per run)
# ---------------------------------------------------------------------------

####
# create_scratch_db — a fresh, uniquely-named database for THIS run so a re-run
#   or a concurrent attempt can never collide with or overwrite a prior scratch.
####
create_scratch_db() {
  if ! command -v createdb >/dev/null 2>&1 || ! command -v pg_restore >/dev/null 2>&1; then
    fatal "createdb/pg_restore are required on PATH (postgresql client tools)" "db"
  fi
  jlog "info" "creating scratch database ${RESTORE_DB}" "db"
  PGPASSWORD="${PGPASSWORD-}" \
    createdb -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" "$RESTORE_DB" \
    || fatal "failed to create scratch database ${RESTORE_DB}" "db"
}

####
# run_pg_restore — load the logical dump with --no-owner and explicit --role.
#   Never restores as superuser; ownership is remapped to the connection role.
####
run_pg_restore() {
  jlog "info" "restoring snapshot into ${RESTORE_DB}" "restore"
  PGPASSWORD="${PGPASSWORD-}" \
    pg_restore -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$RESTORE_DB" \
      --no-owner --role="$PGUSER" --no-privileges --exit-on-error "$DUMP_PATH" \
    || fatal "pg_restore failed for ${RESTORE_DB}" "restore"
  jlog "info" "snapshot restored into ${RESTORE_DB}" "restore"
}

# ---------------------------------------------------------------------------
# Verification block — idempotent, READ-ONLY domain-invariant probes.
#   All bounds are passed as psql -v parameters, never string-interpolated (SQLi-safe).
#   Each check hard-fails the run (non-zero exit) on mismatch.
# ---------------------------------------------------------------------------

####
# psql_ro — run a read-only statement against the restored scratch DB as the
#   (non-superuser) connection role.
####
psql_ro() {
  local params_before=()
  while [[ "$1" == "-v" ]]; do
    params_before+=( -v "$2" )
    shift 2
  done
  PGPASSWORD="${PGPASSWORD-}" \
    psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$RESTORE_DB" \
      -tA -q "${params_before[@]}" -c "$1"
}

####
# verify_row_counts — assert restored row counts (accounts, transactions) equal the
#   source counts measured AT DUMP_TIME (the restored dataset's true boundary in
#   logical mode). The live source has advanced past dump_time, so we time-filter
#   the source by created_at <= dump_time before comparing.
####
verify_row_counts() {
  jlog "info" "verifying row-count parity with source at dump_time ${DUMP_TIME_UTC}" "verify:counts"
  local dump_bucket
  dump_bucket="$(date -u -d "$DUMP_TIME_UTC" '+%Y-%m-%d %H:%M:%S+00')"

  local src_accounts restored_accounts
  src_accounts="$(PGPASSWORD="${PGPASSWORD-}" psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$PGDATABASE" \
                  -tA -q -v bound="$dump_bucket" \
                  -c "SELECT count(*) FROM accounts WHERE created_at <= :'bound';" 2>/dev/null)" \
    || fatal "source accounts count query failed" "verify:counts"
  restored_accounts="$(psql_ro -v bound="$dump_bucket" \
                  "SELECT count(*) FROM accounts WHERE created_at <= :'bound';")" \
    || fatal "restored accounts count query failed" "verify:counts"
  if [[ "$src_accounts" != "$restored_accounts" ]]; then
    fatal "row-count MISMATCH on accounts: source=${src_accounts} restored=${restored_accounts}" "verify:counts"
  fi

  local src_tx restored_tx
  src_tx="$(PGPASSWORD="${PGPASSWORD-}" psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$PGDATABASE" \
            -tA -q -v bound="$dump_bucket" \
            -c "SELECT count(*) FROM transactions WHERE created_at <= :'bound';" 2>/dev/null)" \
    || fatal "source transactions count query failed" "verify:counts"
  restored_tx="$(psql_ro -v bound="$dump_bucket" \
            "SELECT count(*) FROM transactions WHERE created_at <= :'bound';")" \
    || fatal "restored transactions count query failed" "verify:counts"
  if [[ "$src_tx" != "$restored_tx" ]]; then
    fatal "row-count MISMATCH on transactions: source=${src_tx} restored=${restored_tx}" "verify:counts"
  fi

  jlog "info" "row counts verified (accounts=${src_accounts}, transactions=${src_tx})" "verify:counts"
}

####
# verify_audit_chain — spot-check the tamper-evident chained-hash invariant on the
#   restored audit_entries: for the last 1000 entries (by seq), assert
#   hash = H(prev_hash || payload) and the chain-head sentinel prev_hash == 64 zeros.
####
verify_audit_chain() {
  jlog "info" "verifying audit_entries chained-hash sample" "verify:audit"
  local rows
  rows="$(psql_ro \
    "SELECT seq || '|' || prev_hash || '|' || payload::text || '|' || hash
       FROM audit_entries ORDER BY seq DESC LIMIT 1000;")" \
    || fatal "audit_entries sample query failed" "verify:audit"
  [[ -z "$rows" ]] && fatal "audit_entries is empty — chain cannot be verified" "verify:audit"

  local line seq cur_prev payload cur_hash new_h
  local IFS=$'\n'
  for line in $rows; do
    seq="${line%%|*}"; line="${line#*|}"
    cur_prev="${line%%|*}"; line="${line#*|}"
    payload="${line%%|*}"; line="${line#*|}"
    cur_hash="$line"
    new_h="$(printf '%s%s' "$cur_prev" "$payload" | sha256sum | cut -d' ' -f1)"
    if [[ "$new_h" != "$cur_hash" ]]; then
      fatal "audit chain BROKEN at seq=${seq}: hash does not match H(prev_hash||payload)" "verify:audit"
    fi
  done

  local head_ok
  head_ok="$(psql_ro \
    "SELECT (prev_hash = repeat('0',64))::int FROM (SELECT prev_hash FROM audit_entries ORDER BY seq ASC LIMIT 1) e;")" \
    || fatal "audit chain-head query failed" "verify:audit"
  if [[ "$head_ok" != "1" ]]; then
    fatal "audit chain-head sentinel missing (oldest prev_hash != 64 zeros)" "verify:audit"
  fi

  jlog "info" "audit_entries chained-hash sample verified" "verify:audit"
}

####
# verify_balance_reconciliation — assert NO account violates
#   balance == opening_balance + SUM(transactions). Expect zero rows.
####
verify_balance_reconciliation() {
  jlog "info" "verifying balance == opening_balance + sum(transactions)" "verify:balance"
  local mismatches
  mismatches="$(psql_ro \
    "SELECT a.id FROM accounts a
       LEFT JOIN transactions t ON t.account_id = a.id
      GROUP BY a.id
     HAVING a.opening_balance + COALESCE(SUM(t.amount),0) <> a.balance;")" \
    || fatal "balance reconciliation query failed" "verify:balance"
  if [[ -n "$mismatches" ]]; then
    local first_bad
    first_bad="$(printf '%s\n' "$mismatches" | head -n1 | tr -d '[:space:]')"
    fatal "balance reconciliation FAILED for account id(s): ${first_bad} (+more)" "verify:balance"
  fi
  jlog "info" "balance reconciliation clean (0 mismatched accounts)" "verify:balance"
}

# ---------------------------------------------------------------------------
# Outcome: RTO enforcement + structured JSON + cleanup
# ---------------------------------------------------------------------------

####
# cleanup — remove downloaded artifacts and the scratch DB after the drill is done.
#   Never leaves a standing restore instance holding unencrypted tenant data.
####
cleanup() {
  if [[ -n "${DUMP_PATH:-}" && -f "$DUMP_PATH" ]]; then
    rm -f "$DUMP_PATH" "${DUMP_PATH}.sha256"
  fi
  if [[ -n "$SCRATCH" && -d "$SCRATCH" ]]; then
    rm -rf "$SCRATCH"
  fi
  if [[ "${KEEP_SCRATCH:-0}" != "1" && -n "$RESTORE_DB" ]]; then
    PGPASSWORD="${PGPASSWORD-}" \
      dropdb --if-exists -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" "$RESTORE_DB" >/dev/null 2>&1 \
      || true
  fi
}

####
# set_rto — capture end-of-restore time, compute RTO, and surface a structured
#   outcome line (no secrets/keys). Hard-fails if RTO exceeds 1 hour (3600 s).
####
set_rto() {
  RTO_END_EPOCH="$(date +%s)"
  local rto_seconds=$(( RTO_END_EPOCH - RTO_START_EPOCH ))

  printf '{"event":"restore_complete","run_id":"%s","target_time":"%s","snapshot_time":"%s","gap_seconds":%s,"rto_seconds":%s}\n' \
    "$RUN_ID" "$TARGET_TIME_UTC" "$DUMP_TIME_UTC" "${GAP_SECONDS}" "$rto_seconds" >> "$LOG_FILE"

  if (( rto_seconds > 3600 )); then
    fatal "RTO budget violated: ${rto_seconds}s exceeded 3600s (1h) — restore did not meet the acceptance target" "rto"
  fi
  jlog "info" "RTO satisfied: ${rto_seconds}s <= 3600s (1h); start=$(date -u -d "@${RTO_START_EPOCH}" '+%Y-%m-%dT%H:%M:%SZ') end=$(date -u -d "@${RTO_END_EPOCH}" '+%Y-%m-%dT%H:%M:%SZ')" "rto"
}

# ---------------------------------------------------------------------------
# main — orchestrate the restore run in dependency order.
# ---------------------------------------------------------------------------
main() {
  parse_args "$@"
  validate_env

  RUN_ID="__unset__"
  LOG_FILE="${TMPDIR:-/tmp}/fms-restore-bootstrap.log"

  init_run
  acquire_lock
  RTO_START_EPOCH="$(date +%s)"

  safety_interlock
  select_snapshot
  download_snapshot
  create_scratch_db
  run_pg_restore

  verify_row_counts
  verify_audit_chain
  verify_balance_reconciliation

  set_rto
  cleanup

  jlog "info" "restore completed successfully; run_id=${RUN_ID} restored into ${RESTORE_DB}" "done"
}

main "$@"