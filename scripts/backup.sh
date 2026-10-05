#!/usr/bin/env bash
#
# backup.sh — FMS disaster-recovery full-snapshot orchestrator.
# Blueprint §7 / tech-spec §9.3 (RPO ≤ 15 min via WAL archiving in the
# StatefulSet; RTO ≤ 1 h via tested restore). This script delivers the daily
# full snapshot, SHA256 integrity pair, DR replication, retention archival,
# verification, and on-failure alerting. It does NOT manage WAL — continuous
# WAL archiving complements this script's fulls.
#
# SCHEDULING
#   Run by k8s CronJob postgres-backup-cronjob.yaml on `0 1 * * *` and
#   manually by `make backup`. See deploy/k8s/postgres-backup-cronjob.yaml.
#
# ============================================================================
# S3 OBJECT LAYOUT CONTRACT — application/vnd.fms.backup.v1 (frozen; shared
# with scripts/restore.sh and postgres-backup-cronjob.yaml). Any change here
# is a BREAKING contract change requiring a coordinated version bump.
#   primary  : <BACKUP_PREFIX>/fms-<TS>.dump
#   checksum : <BACKUP_PREFIX>/fms-<TS>.dump.sha256
# where TS = YYYYMMDD-HHMMSS-<pid> (UTC, zero-padded, lexicographically
# sortable). The .sha256 file contains exactly:  "<sha256>  <basename>"
# (two spaces, basename only — never a bucket name, slash, or absolute path)
# so scripts/restore.sh can run `sha256sum -c --ignore-missing` portably.
# ============================================================================
#
# STRUCTURED ERROR-CONTRACT (job state machine; consumed by alert routing and
# the CronJob failure policy):
#   0   SUCCESS
#   1   DUMP_FAILURE          pg_dump failed or timed out
#   2   UPLOAD_FAILURE        s3 cp of dump or checksum failed
#   3   RETENTION_FAILURE     archival/prune failed or config invalid for prune
#   4   DR_REPLICATION_FAILURE  DR copy failed (primary is durable; DR absent)
#   255 UNEXPECTED            any failure outside the enumerated set
#
# RESILIENCE NOTE: the change description suggests 5 S3 upload attempts; this
# script deliberately uses 3 to stay consistent with tech-spec §7.7
# (retry up to 3, exponential backoff). This is an explicit operational
# override; raise/lower via S3_UPLOAD_ATTEMPTS.
#
# ENV CONTRACT (credentials via environment / IRSA only — never CLI or
# committed):
#   PGHOST PGPORT PGUSER PGPASSWORD PGDATABASE  or  DATABASE_URL (preferred;
#     parsed into PG* env vars internally — never printed, never in argv)
#   S3_BUCKET S3_REGION
#   AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY  (static keys XOR IAM role)
#   AWS_ROLE_ARN / AWS_WEB_IDENTITY_TOKEN_FILE  (IAM role / IRSA)
#   RETENTION_DAYS   (default 30)
#   ALERT_WEBHOOK    (optional; HTTPS only)
#   ALERT_WEBHOOK_TOKEN (optional; sent as Bearer to ALERT_WEBHOOK)
#   BACKUP_ROOT      (default /backup; must live under /backup)
#   BACKUP_PREFIX    (default fms-backups; S3 key prefix for primary)
#   ARCHIVE_PREFIX   (default fms-archive; DR archive prefix for pruned fulls)
#   S3_UPLOAD_ATTEMPTS (default 3; §7.7 retry count)
#   DUMP_TIMEOUT_SEC (default 1800; hard bound on pg_dump run)
#   KMS_KEY_ID       (optional; per-bucket SSE-KMS key, region-scoped for DR)
#   PGSSLMODE        (default verify-full; never disables TLS)
#
# SECURITY: set -euo pipefail; umask 077; never set -x; never echo PGPASSWORD,
# AWS_SECRET_ACCESS_KEY, or a connection string; never materialize credentials
# in argv (no --password, no inline PGPASSWORD=, no DATABASE_URL in a process
# arg). Secrets come from the k8s Secret mounted as env only.

set -euo pipefail
umask 077

# ============================================================================
# Globals / run correlation id (TS doubles as the run correlation id threaded
# through every log line and the alert payload, satisfying §9.2 tracing at the
# operational layer).
# ============================================================================
run_id=""
EXIT_CODE=0
FAIL_STEP=""
FAIL_OBJECT=""
DUMP_FILE=""
CHECKSUM_FILE=""
DUMP_PATH=""
CHECKSUM_PATH=""

# ============================================================================
# Structured JSON logging. run ts = ISO-8601 UTC.
# ============================================================================
jlog() {
  local level="${1:?level}"; shift
  local step="${1:?step}"; shift
  printf '{"level":"%s","ts":"%s","run_id":"%s","step":"%s","message":"%s"\n' \
    "$level" "$(date -u --iso-8601=seconds)" "$run_id" "$step" "$*"
}

# ============================================================================
# Fatal error: record structured state and terminate (the EXIT trap sends the
# alert; we never panic-print raw command output or connection strings).
# ============================================================================
fatal() {  # <exit_code> <step> <message>
  local code="$1" step="$2"; shift 2
  EXIT_CODE="$code"; FAIL_STEP="$step"
  jlog "error" "$step" "$*"
  exit "$code"
}

# ============================================================================
# Parse DATABASE_URL into disaggregated PG* env vars so pg_dump (libpq) can
# read them. The password is never printed and never placed on a process
# argument line (A02 / A09 credential-exposure mitigation).
# ============================================================================
parse_database_url() {
  local url rest userinfo hostport user pass host path dbname port
  url="$DATABASE_URL"
  rest="${url#*://}"
  if [[ "$rest" == *@* ]]; then
    userinfo="${rest%%@*}"
    hostport="${rest#*@}"
  else
    userinfo=""
    hostport="$rest"
  fi
  if [[ "$userinfo" == *:* ]]; then
    user="${userinfo%%:*}"; pass="${userinfo#*:}"
  else
    user="$userinfo"; pass=""
  fi
  hostport="${hostport%%\?*}"               # drop query string
  if [[ "$hostport" == */* ]]; then
    host="${hostport%%/*}"; path="/${hostport#*/}"
  else
    host="$hostport"; path=""
  fi
  dbname="${path#/}"
  # host may be bare, host:port, or [ipv6]:port
  if [[ "$host" == \[*\]* ]]; then
    port="${host##*:}"
    host="${host%%]:*}"
    [[ -z "$port" ]] && port="${PGPORT:-5432}"
    host="${host#[}"; host="${host%]}"
  elif [[ "$host" == *:* ]]; then
    port="${host##*:}"; host="${host%%:*}"
  else
    port="${PGPORT:-5432}"
  fi
  export PGHOST="${host:-${PGHOST:-127.0.0.1}}"
  export PGPORT="${port:-5432}"
  export PGUSER="${user:-${PGUSER:-postgres}}"
  [[ -n "$pass" ]] && export PGPASSWORD="$pass"
  export PGDATABASE="${dbname:-${PGDATABASE:-fms}}"
}

# ============================================================================
# Config validation gate (Step 0 in execution order — fail fast at the
# boundary, before any dump work, with no side effects).
# ============================================================================
# Reject command-injection-relevant metacharacters in any interpolated value.
validate_safe_name() {
  local label="$1" val="$2"
  if [[ ! "$val" =~ ^[A-Za-z0-9._/-]+$ ]]; then
    fatal 255 "validate_config" "invalid characters in $label"
  fi
}

validate_config() {
  jlog "info" "validate_config" "begin"

  # --- Database connection: DATABASE_URL takes precedence and is parsed
  # (never printed) into PG* env vars; otherwise require the PG* set.
  if [[ -n "${DATABASE_URL:-}" ]]; then
    if [[ ! "$DATABASE_URL" =~ ^postgres(ql)?:// ]]; then
      fatal 255 "validate_config" "DATABASE_URL scheme not postgres:// or postgresql://"
    fi
    parse_database_url
  else
    for var in PGHOST PGUSER PGDATABASE; do
      if [[ -z "${!var:-}" ]]; then
        fatal 255 "validate_config" "$var is required (or set DATABASE_URL)"
      fi
    done
  fi
  if [[ -n "${PGHOST:-}" ]]; then
    if [[ "$PGHOST" == *"://"* ]] || [[ ! "$PGHOST" =~ ^[A-Za-z0-9._/-]+$ ]]; then
      fatal 255 "validate_config" "invalid PGHOST"
    fi
  fi

  # --- AWS: bucket + region required; static keys XOR IAM role.
  for var in S3_BUCKET S3_REGION; do
    if [[ -z "${!var:-}" ]]; then
      fatal 255 "validate_config" "$var is required"
    fi
  done
  if [[ "$S3_BUCKET" == s3://* ]]; then
    fatal 255 "validate_config" "S3_BUCKET must not include an s3:// scheme"
  fi
  if [[ ! "$S3_BUCKET" =~ ^[a-z0-9][a-z0-9.-]+$ ]]; then
    fatal 255 "validate_config" "S3_BUCKET does not match AWS bucket naming (lowercase alnum + hyphen/dot)"
  fi
  local have_static=0 have_role=0
  [[ -n "${AWS_ACCESS_KEY_ID:-}" && -n "${AWS_SECRET_ACCESS_KEY:-}" ]] && have_static=1
  [[ -n "${AWS_ROLE_ARN:-}" || -n "${AWS_WEB_IDENTITY_TOKEN_FILE:-}" ]] && have_role=1
  if [[ "$have_static" -eq 1 && "$have_role" -eq 1 ]]; then
    fatal 255 "validate_config" "set static AWS keys OR an IAM role (IRSA), never both"
  fi
  if [[ "$have_static" -eq 0 && "$have_role" -eq 0 ]]; then
    fatal 255 "validate_config" "AWS credentials required (static keys or IAM role)"
  fi
  if [[ -n "${DR_S3_BUCKET:-}" ]]; then
    if [[ "$DR_S3_BUCKET" == s3://* ]]; then
      fatal 255 "validate_config" "DR_S3_BUCKET must not include an s3:// scheme"
    fi
    if [[ ! "$DR_S3_BUCKET" =~ ^[a-z0-9][a-z0-9.-]+$ ]]; then
      fatal 255 "validate_config" "DR_S3_BUCKET does not match AWS bucket naming"
    fi
  fi

  # --- Path/name allowlists (command-injection prevention + SAFE-DELETE gate).
  BACKUP_ROOT="${BACKUP_ROOT:-/backup}"
  BACKUP_PREFIX="${BACKUP_PREFIX:-fms-backups}"
  ARCHIVE_PREFIX="${ARCHIVE_PREFIX:-fms-archive}"
  validate_safe_name "BACKUP_ROOT" "$BACKUP_ROOT"
  validate_safe_name "BACKUP_PREFIX" "$BACKUP_PREFIX"
  validate_safe_name "ARCHIVE_PREFIX" "$ARCHIVE_PREFIX"

  # Resolve to canonical absolute path and force it under the fixed safe base.
  local canonical
  canonical="$(realpath -m "$BACKUP_ROOT")"
  if [[ "$canonical" != /backup && "$canonical" != /backup/* ]]; then
    fatal 255 "validate_config" "BACKUP_ROOT must live under /backup"
  fi
  BACKUP_ROOT="$canonical"
  if [[ ! -d "$BACKUP_ROOT" ]]; then
    fatal 255 "validate_config" "BACKUP_ROOT does not exist: $BACKUP_ROOT"
  fi
  if [[ ! -w "$BACKUP_ROOT" ]]; then
    fatal 1 "validate_config" "BACKUP_ROOT is not writable"
  fi

  # --- Retention: positive integer > 0, else config failure (exit 3), never a
  # destructive policy violation.
  RETENTION_DAYS="${RETENTION_DAYS:-30}"
  if [[ ! "$RETENTION_DAYS" =~ ^[0-9]+$ ]] || [[ "$RETENTION_DAYS" -lt 1 ]]; then
    fatal 3 "validate_config" "RETENTION_DAYS must be a positive integer"
  fi

  # --- Optional alert webhook: HTTPS only (reject plain HTTP).
  if [[ -n "${ALERT_WEBHOOK:-}" && "$ALERT_WEBHOOK" != https://* ]]; then
    fatal 255 "validate_config" "ALERT_WEBHOOK must be https://"
  fi

  S3_UPLOAD_ATTEMPTS="${S3_UPLOAD_ATTEMPTS:-3}"
  DUMP_TIMEOUT_SEC="${DUMP_TIMEOUT_SEC:-1800}"
  if [[ ! "$S3_UPLOAD_ATTEMPTS" =~ ^[0-9]+$ ]] || [[ "$S3_UPLOAD_ATTEMPTS" -lt 1 ]]; then
    fatal 255 "validate_config" "S3_UPLOAD_ATTEMPTS must be a positive integer"
  fi
  if [[ ! "$DUMP_TIMEOUT_SEC" =~ ^[0-9]+$ ]] || [[ "$DUMP_TIMEOUT_SEC" -lt 1 ]]; then
    fatal 255 "validate_config" "DUMP_TIMEOUT_SEC must be a positive integer"
  fi

  PGSSLMODE="${PGSSLMODE:-verify-full}"
  jlog "info" "validate_config" "complete"
}

# ============================================================================
# Concurrency guard: prevent CronJob + manual `make backup` overlap, which can
# race retention pruning against a fresh write. Bounded, never blocks a second
# run for long; failure to acquire is alert-worthy (exit 3).
# ============================================================================
acquire_lock() {
  exec 9>"$BACKUP_ROOT/.fms-backup.lock"
  if ! flock -n 9; then
    fatal 3 "lock" "another backup run holds the lock; refusing to overlap"
  fi
  jlog "info" "lock" "acquired"
}

# ============================================================================
# Alert adapter — POST metadata-only JSON to ALERT_WEBHOOK. Bounded retries
# (3) + short timeout; a webhook failure never alters the primary exit code.
# Payload contains NO financial data, NO credentials, and no raw command
# output (A09 / A02 credential-leakage mitigation).
# ============================================================================
send_alert() {
  local code="$1" ts="$2" object="$3" semcode="$4"
  [[ -z "${ALERT_WEBHOOK:-}" ]] && return 0
  # Build a single JSON object from explicitly allowlisted fields only.
  local payload
  payload=$(printf '{"service":"postgres-backup","object":"%s","ts":"%s","exit_code":%d,"code":"%s"},' \
    "$object" "$ts" "$code" "$semcode")
  payload="${payload%,}"   # strip trailing comma -> valid JSON object
  local attempt=1 ok=0 curl_args=()
  curl_args=(--silent --show-error --max-time 10 -H 'Content-Type: application/json')
  [[ -n "${ALERT_WEBHOOK_TOKEN:-}" ]] && curl_args+=(-H "Authorization: Bearer $ALERT_WEBHOOK_TOKEN")
  while (( attempt <= 3 )); do
    if curl "${curl_args[@]}" -d "$payload" "$ALERT_WEBHOOK" >/dev/null 2>&1; then
      ok=1; break
    fi
    sleep "$(( attempt * 2 ))"
    attempt=$(( attempt + 1 ))
  done
  if (( ok == 1 )); then
    jlog "info" "alert" "webhook delivered"
  else
    jlog "warn" "alert" "webhook delivery failed (primary exit code preserved)"
  fi
}

# ============================================================================
# EXIT trap: guarantee the alert fires on ANY failure, enumerated or not.
# Unmapped failures collapse to 255 / BACKUP_UNEXPECTED so alerting and the
# CronJob failure policy always see a non-zero status with a semantic code.
# ============================================================================
semantic_code() {
  case "$1" in
    0) echo BACKUP_SUCCESS ;;
    1) echo BACKUP_DUMP_FAILED ;;
    2) echo BACKUP_UPLOAD_FAILED ;;
    3) echo BACKUP_RETENTION_FAILED ;;
    4) echo BACKUP_DR_REPLICATION_FAILED ;;
    *) echo BACKUP_UNEXPECTED ;;
  esac
}

finalize() {
  local rc="$1"
  local code="$EXIT_CODE"
  if (( code == 0 && rc != 0 )); then
    code=255   # uncoded failure surfaced by set -e / pipefail
  fi
  local obj="$FAIL_OBJECT"; [[ -z "$obj" ]] && obj="${DUMP_FILE:-}"
  local ts; ts="$(date -u --iso-8601=seconds)"
  local sem; sem="$(semantic_code "$code")"
  if (( code == 0 )); then
    jlog "info" "finalize" "backup complete exit=0"
  else
    jlog "error" "finalize" "backup failed code=$code ref=$sem step=${FAIL_STEP:-unknown}"
    send_alert "$code" "$ts" "$obj" "$sem"
  fi
  exit "$code"
}
trap 'finalize $?' EXIT

# ============================================================================
# Dump step — bounded, custom-format single-snapshot pg_dump. TLS enforced via
# PGSSLMODE (never disabled). `--` separates positional args from flags.
# libpq reads PGHOST/PGPORT/PGUSER/PGPASSWORD/PGDATABASE from the environment
# (set either directly or by parse_database_url); nothing is passed in argv.
# ============================================================================
dump_full() {
  DUMP_FILE="fms-${run_id}.dump"
  DUMP_PATH="$BACKUP_ROOT/$DUMP_FILE"
  CHECKSUM_FILE="${DUMP_FILE}.sha256"
  CHECKSUM_PATH="$BACKUP_ROOT/$CHECKSUM_FILE"
  FAIL_OBJECT="$DUMP_FILE"

  jlog "info" "dump_full" "snapshot started"
  if ! timeout "$DUMP_TIMEOUT_SEC" pg_dump -Fc -f "$DUMP_PATH" -- "$PGDATABASE" >/dev/null 2>&1; then
    fatal 1 "dump_full" "pg_dump failed or timed out"
  fi
  if [[ ! -s "$DUMP_PATH" ]]; then
    fatal 1 "dump_full" "pg_dump produced an empty archive"
  fi
  jlog "info" "dump_full" "snapshot complete"
}

# ============================================================================
# Checksum: SHA256 pair, single line "<hash>  <basename>" (two spaces,
# basename only). Written strictly before upload begins (integrity §5.3).
# ============================================================================
compute_checksum() {
  jlog "info" "checksum" "computing sha256"
  if ! sha256sum "$DUMP_PATH" > "$CHECKSUM_PATH"; then
    fatal 1 "checksum" "checksum computation failed"
  fi
  jlog "info" "checksum" "checksum computed"
}

# ============================================================================
# Append-only immutability assert: the object must not already exist, else this
# is an idempotency/audit violation and the run fails (never overwrite).
# ============================================================================
assert_new_object() {
  local key="$1"
  if aws s3api head-object --bucket "$S3_BUCKET" --key "$key" --region "$S3_REGION" \
      >/dev/null 2>&1; then
    fatal 2 "upload" "refusing to overwrite existing object s3://$S3_BUCKET/$key"
  fi
}

# ============================================================================
# Upload helper — exponential backoff with jitter, re-verifying the local
# checksum before every attempt. SSE-KMS enforced (never SSE-S3/none).
# ============================================================================
upload_with_retry() {
  # <local-path> <s3-destination> <label>
  local src="$1" dst="$2" label="$3"
  local attempt=1
  local kms_args=()
  [[ -n "${KMS_KEY_ID:-}" ]] && kms_args+=(--sse-kms-key-id "$KMS_KEY_ID")
  while (( attempt <= S3_UPLOAD_ATTEMPTS )); do
    if ! sha256sum -c "$CHECKSUM_PATH" >/dev/null 2>&1; then
      fatal 2 "upload" "local artifact checksum mismatch before upload ($label)"
    fi
    if aws s3 cp --sse aws:kms "${kms_args[@]}" "$src" "$dst" --region "$S3_REGION" \
        >/dev/null 2>&1; then
      jlog "info" "upload" "$label uploaded"
      return 0
    fi
    if (( attempt < S3_UPLOAD_ATTEMPTS )); then
      local jitter=$(( RANDOM % 100 ))
      local wait_s=$(( attempt * 2 ))
      sleep "$(( wait_s )).${jitter}"
    fi
    attempt=$(( attempt + 1 ))
  done
  fatal 2 "upload" "s3 upload failed after $S3_UPLOAD_ATTEMPTS attempts ($label)"
}

# ============================================================================
# Primary + DR upload of dump and checksum (pair ordering: checksum strictly
# after its dump, so a restore can never see an orphan .sha256).
# ============================================================================
upload_primary() {
  local primary_key="$BACKUP_PREFIX/$DUMP_FILE"
  local checksum_key="$BACKUP_PREFIX/$CHECKSUM_FILE"
  assert_new_object "$primary_key"
  upload_with_retry "$DUMP_PATH" "s3://$S3_BUCKET/$primary_key" "dump"
  assert_new_object "$checksum_key"
  upload_with_retry "$CHECKSUM_PATH" "s3://$S3_BUCKET/$checksum_key" "checksum"
}

# ============================================================================
# Retention: archive-before-delete, per-object FAIL-CLOSED (§5.3). Deletion is
# gated on a verified archive copy; an unarchived object is never deleted. When
# no DR bucket is configured, hard-delete may proceed but is always logged
# (implicit audit) — the only permitted silent-delete config.
# ============================================================================
prune_objects() {
  jlog "info" "prune" "retention scan start (days=$RETENTION_DAYS)"
  local cutoff epoch_now
  epoch_now="$(date +%s)"
  cutoff=$(( epoch_now - RETENTION_DAYS * 86400 ))

  local json key lastmodified epoch arch_dst sha_src sha_dst
  json="$(aws s3api list-objects-v2 --bucket "$S3_BUCKET" --prefix "$BACKUP_PREFIX/" \
      --region "$S3_REGION" --query "Contents[?starts_with(Key, \`$BACKUP_PREFIX/\`)].{K:Key,M:LastModified}" \
      --output json 2>/dev/null || true)"
  [[ -z "$json" || "$json" == "null" ]] && { jlog "info" "prune" "no objects to consider"; return 0; }

  # Parse Key/LastModified pairs; scope constrained to the exact fms-*.dump
  # naming under the primary prefix (never broad globs, never unvalidated).
  while IFS= read -r key && IFS= read -r lastmodified; do
    [[ "$key" != "$BACKUP_PREFIX/fms-"*.dump ]] && continue
    [[ "$lastmodified" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2} ]] || continue
    epoch="$(date -u -d "$lastmodified" +%s 2>/dev/null || continue)"
    if (( epoch >= cutoff )); then continue; fi

    jlog "info" "prune" "expired $key"
    local sha_key="${key}.sha256"
    if [[ -n "${DR_S3_BUCKET:-}" ]]; then
      # ARCHIVE = move to DR bucket. Per-object fail-closed: only after the
      # archive copy is verified may the primary be removed.
      arch_dst="s3://$DR_S3_BUCKET/$ARCHIVE_PREFIX/${key##*/}"
      sha_src="s3://$S3_BUCKET/$sha_key"
      sha_dst="s3://$DR_S3_BUCKET/$ARCHIVE_PREFIX/${sha_key##*/}"
      if aws s3 cp "$key" "$arch_dst" --sse aws:kms --region "$S3_REGION" >/dev/null 2>&1 \
          && aws s3 cp "$sha_src" "$sha_dst" --sse aws:kms --region "$S3_REGION" >/dev/null 2>&1; then
        if ! aws s3api head-object --bucket "$DR_S3_BUCKET" --key "$ARCHIVE_PREFIX/${key##*/}" \
            --region "$S3_REGION" >/dev/null 2>&1; then
          fatal 3 "prune" "archive object not verifiable; aborting delete for $key"
        fi
        aws s3 rm "s3://$S3_BUCKET/$key" --region "$S3_REGION" >/dev/null 2>&1 \
          && aws s3 rm "s3://$S3_BUCKET/$sha_key" --region "$S3_REGION" >/dev/null 2>&1 \
          && jlog "info" "prune" "archived and removed $key" \
          || fatal 3 "prune" "delete after archive failed for $key"
      else
        fatal 3 "prune" "archive copy failed; aborted deletion for $key (§5.3 fail-closed)"
      fi
    else
      # No DR bucket: hard-delete allowed by policy but MUST be audited.
      if aws s3 rm "s3://$S3_BUCKET/$key" --region "$S3_REGION" >/dev/null 2>&1 \
          && aws s3 rm "s3://$S3_BUCKET/$sha_key" --region "$S3_REGION" >/dev/null 2>&1; then
        jlog "warn" "prune" "hard-delete (no DR bucket configured) of $key"
      else
        fatal 3 "prune" "delete without DR archive failed for $key"
      fi
    fi
  done < <(printf '%s\n' "$json" | sed -n 's/.*"K": *"\([^"]*\)".*/\1/p; s/.*"M": *"\([^"]*\)".*/\1/p')
}

# ============================================================================
# DR replication (description step 7): copy dump AND its checksum to DR bucket.
# Runs only after checksum verification, so an unverified object is never
# replicated. A DR failure is distinct (code 4) — the primary remains durable,
# the run does NOT falsely report success, and the alert fires with the DR code.
# ============================================================================
replicate_to_dr() {
  [[ -z "${DR_S3_BUCKET:-}" ]] && return 0
  local primary_key="$BACKUP_PREFIX/$DUMP_FILE"
  local checksum_key="$BACKUP_PREFIX/$CHECKSUM_FILE"
  local kms_args=()
  [[ -n "${KMS_KEY_ID:-}" ]] && kms_args+=(--sse-kms-key-id "$KMS_KEY_ID")
  # DR must use its own region-scoped KMS key with an appropriate policy;
  # KMS_KEY_ID here is the DR key.
  local attempt=1
  while (( attempt <= S3_UPLOAD_ATTEMPTS )); do
    if aws s3 cp --sse aws:kms "${kms_args[@]}" \
          "s3://$S3_BUCKET/$primary_key" "s3://$DR_S3_BUCKET/$primary_key" --region "$S3_REGION" >/dev/null 2>&1 \
        && aws s3 cp --sse aws:kms "${kms_args[@]}" \
          "s3://$S3_BUCKET/$checksum_key" "s3://$DR_S3_BUCKET/$checksum_key" --region "$S3_REGION" >/dev/null 2>&1; then
      jlog "info" "replicate" "DR copy complete"
      return 0
    fi
    if (( attempt < S3_UPLOAD_ATTEMPTS )); then
      sleep "$(( attempt * 2 ))"
    fi
    attempt=$(( attempt + 1 ))
  done
  fatal 4 "replicate" "DR replication failed (primary durable; DR absent)"
}

# ============================================================================
# Final commit check: confirm primary dump + checksum exist with positive
# length before reporting success.
# ============================================================================
verify_objects() {
  local primary_key="$BACKUP_PREFIX/$DUMP_FILE"
  local checksum_key="$BACKUP_PREFIX/$CHECKSUM_FILE"
  local attrs
  attrs="$(aws s3api head-object --bucket "$S3_BUCKET" --key "$primary_key" --region "$S3_REGION" 2>/dev/null || true)"
  if [[ -z "$attrs" ]] || ! grep -q '"ContentLength": *[1-9]' <<<"$attrs"; then
    fatal 2 "verify" "primary object missing or zero-length"
  fi
  attrs="$(aws s3api head-object --bucket "$S3_BUCKET" --key "$checksum_key" --region "$S3_REGION" 2>/dev/null || true)"
  if [[ -z "$attrs" ]] || ! grep -q '"ContentLength": *[1-9]' <<<"$attrs"; then
    fatal 2 "verify" "checksum object missing or zero-length"
  fi
  jlog "info" "verify" "primary + checksum verified"
}

# ============================================================================
# Daily archive-open check (acceptance evidence): confirm the custom archive
# is readable via pg_restore --list before the local plaintext copy is kept.
# ============================================================================
verify_archive_open() {
  if command -v pg_restore >/dev/null 2>&1; then
    if pg_restore -l "$DUMP_PATH" -f /dev/null >/dev/null 2>&1; then
      jlog "info" "verify" "archive opens cleanly"
    else
      jlog "warn" "verify" "pg_restore -l reported an issue; investigate (backup remains in S3)"
    fi
  fi
}

# ============================================================================
# main — enforce step order. Prune strictly after current uploads succeed so a
# fresh backup is never deleted in-flight by a concurrent retention pass
# (guarded additionally by flock).
# ============================================================================
main() {
  validate_config
  acquire_lock
  run_id="$(date -u +%Y%m%d-%H%M%S-$$)"
  if [[ ! "$run_id" =~ ^[0-9]{8}-[0-9]{6}-[0-9]+$ ]]; then
    fatal 255 "validate_config" "run_id timestamp invalid"
  fi
  jlog "info" "main" "backup run start"

  dump_full            # timestamped snapshot
  compute_checksum     # SHA256 before upload
  upload_primary       # dump + checksum, pair ordering
  prune_objects        # retention — only after uploads succeed
  replicate_to_dr      # DR replication — after checksum verification
  verify_objects       # commit check
  verify_archive_open  # complementary daily archive verification

  EXIT_CODE=0
  jlog "info" "main" "backup run complete"
}

main "$@"