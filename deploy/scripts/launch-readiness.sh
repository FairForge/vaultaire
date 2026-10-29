#!/bin/bash
# launch-readiness.sh — which launch switches are still off (Stage 1a Prompt 43 Part A).
# READ-ONLY: one line per gate, then a summary; it changes nothing on the box except
# its own textfile gauge. Run as root on slc-vaultaire-01:
#
#   sudo bash /opt/vaultaire/bin/launch-readiness.sh            # table + gauge
#   sudo bash /opt/vaultaire/bin/launch-readiness.sh --no-metrics
#
# Each line is `GREEN|RED|UNKNOWN <gate> [launch|info] — <evidence>`:
#   GREEN    the source was read and says the switch is on
#   RED      the source was read and says it is off
#   UNKNOWN  the source could not be read — NEVER counted as GREEN (the fail-open
#            default is the bug: RP56)
# Exit 0 only when every `launch` gate is GREEN; 1 otherwise (a RED or an UNKNOWN);
# 2 on a usage error. LR_LAUNCH_GATES=<comma list> overrides which gates are `launch`
# (the owner's call; default below: every security gate + Stripe + signups).
#
# Sources (read-only, each gate names its own): the active slot's /metrics and its
# journal since it started, node_exporter's textfiles in /opt/vaultaire/metrics, the
# database through `runuser -u postgres -- psql` (SELECTs only), Alertmanager's API on
# localhost, the .env BY KEY NAME — a value is compared inside the script and NEVER
# printed (a test runs it over an .env holding a sentinel secret) — and file existence.
#
# Gauge: vaultaire_launch_gate{gate,class} 1 = GREEN, 0 = RED or UNKNOWN, written to
# /opt/vaultaire/metrics/vaultaire_launch_gate.prom (atomic rename, 0644; node_exporter
# reads it through the root-owned symlink of deploy/monitoring/README.md). No alert
# rule: the owner reads it — a rule on a click-list would page for weeks.
set -uo pipefail

ROOT=${LR_ROOT:-/opt/vaultaire}
ENV_FILE=$ROOT/configs/.env
METRICS_DIR=${LR_METRICS_DIR:-$ROOT/metrics}
ALERTMANAGER_URL=${LR_ALERTMANAGER_URL:-http://127.0.0.1:9093}
RELEASE_VKEY=${LR_RELEASE_VKEY:-$ROOT/configs/release.vkey}
CURL=${LR_CURL:-curl}
JOURNALCTL=${LR_JOURNALCTL:-journalctl}
SYSTEMCTL=${LR_SYSTEMCTL:-systemctl}
# The WP-DOMAIN-1 targets.
TARGET_ENDPOINT=https://s3.stored.cloud
TARGET_PUBLIC_HOST=https://stored.cloud
DEFAULT_LAUNCH=master_key,translog_signing_key,dump_sealed,credentials_wrapped,plaintext_dropped,keys_rotated,blomp_sealed,sia_sealed,geyser_sealed,stripe_ready,signups_open
LAUNCH_GATES=,${LR_LAUNCH_GATES:-$DEFAULT_LAUNCH},

WRITE_METRICS=true
case "${1:-}" in
  '') ;;
  --no-metrics) WRITE_METRICS=false ;;
  *) echo "usage: $0 [--no-metrics]" >&2; exit 2 ;;
esac

RESULTS=()    # "gate status class" per gate, in order
LAUNCH_NOT_GREEN=0

# gate NAME STATUS EVIDENCE — prints the line and records it.
gate() {
  local name=$1 status=$2 evidence=$3 class=info
  case "$LAUNCH_GATES" in *",$name,"*) class=launch ;; esac
  printf '%-7s %s [%s] — %s\n' "$status" "$name" "$class" "$evidence"
  RESULTS+=("$name $status $class")
  if [ "$class" = launch ] && [ "$status" != GREEN ]; then LAUNCH_NOT_GREEN=$((LAUNCH_NOT_GREEN + 1)); fi
}

# q SQL — one SELECT against the app database; prints psql's -At output, returns its
# status. LR_PSQL replaces the client in tests (it receives the SQL as $1).
q() {
  if [ -n "${LR_PSQL:-}" ]; then "$LR_PSQL" "$1"; return; fi
  runuser -u postgres -- psql -X -At -v ON_ERROR_STOP=1 -d "$DB_NAME" -c "$1" 2>/dev/null
}

# env_has KEY — 0 when the .env names KEY (any value).
env_has() { grep -q "^$1=" "$ENV_FILE" 2>/dev/null; }
# env_val KEY — the value of KEY (one level of quotes removed). For comparison inside
# this script ONLY: never echo it, never put it in an evidence string.
env_val() {
  local line value
  line=$(grep -m1 "^$1=" "$ENV_FILE" 2>/dev/null) || return 1
  value=${line#"$1"=}
  case "$value" in \"*\") value=${value#\"}; value=${value%\"} ;; \'*\') value=${value#\'}; value=${value%\'} ;; esac
  printf '%s' "$value"
}
is_off() { case "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')" in ''|false|0|off|no) return 0 ;; *) return 1 ;; esac; }

# metric NAME [LABEL_SUBSTRING] — the first sample's value of NAME on the active slot's
# /metrics (empty when absent). METRICS_TEXT is fetched once.
metric() {
  local name=$1 want=${2:-}
  printf '%s\n' "$METRICS_TEXT" | awk -v n="$name" -v w="$want" '
    $1 == n || index($1, n "{") == 1 { if (w == "" || index($1, w) > 0) { print $2; exit } }'
}

# --- the sources ------------------------------------------------------------------------
ENV_READABLE=false
[ -r "$ENV_FILE" ] && ENV_READABLE=true
DB_NAME=vaultaire
if $ENV_READABLE && v=$(env_val DB_NAME) && [ -n "$v" ]; then DB_NAME=$v; fi

PORT=$(tr -dc '0-9' < "$ROOT/ACTIVE_PORT" 2>/dev/null) || PORT=
METRICS_OK=false
METRICS_TEXT=
if [ -n "$PORT" ] && METRICS_TEXT=$("$CURL" -sf --max-time 10 "http://127.0.0.1:$PORT/metrics" 2>/dev/null) && [ -n "$METRICS_TEXT" ]; then
  METRICS_OK=true
fi
DB_OK=false
[ "$(q 'SELECT 1' 2>/dev/null)" = 1 ] && DB_OK=true

echo "launch readiness — $(date -u +%Y-%m-%dT%H:%M:%SZ) — active slot ${PORT:-?}"

# --- security gates ---------------------------------------------------------------------

# master_key: the active slot's own boot line names the source (never the key) and no
# tenant key fails to unwrap. Prod must load it from the TPM-sealed credential.
gate_master_key() {
  local since journal src failures
  if [ -z "$PORT" ]; then gate master_key UNKNOWN "no active port in $ROOT/ACTIVE_PORT"; return; fi
  since=$("$SYSTEMCTL" show -p ActiveEnterTimestamp --value "vaultaire@$PORT" 2>/dev/null) || since=
  if [ -z "$since" ] || ! journal=$("$JOURNALCTL" -u "vaultaire@$PORT" -o cat --no-pager --since "$since" 2>/dev/null); then
    gate master_key UNKNOWN "cannot read the journal of vaultaire@$PORT since it started"; return
  fi
  # The Info line's sources only: the Warn after it ("master key: lower-precedence …") is not one.
  src=$(printf '%s\n' "$journal" | grep -oE 'master key: (credential|file|env)' | tail -1 | sed 's/master key: //')
  if [ -z "$src" ]; then gate master_key RED "no 'master key:' Info line since vaultaire@$PORT started ($since): no ENCRYPTION_MASTER_KEY"; return; fi
  if [ "$src" != credential ]; then gate master_key RED "loaded from '$src', not the TPM-sealed credential (docs/ENCRYPTION.md)"; return; fi
  if ! $METRICS_OK; then gate master_key UNKNOWN "master key: credential, but /metrics of :$PORT did not answer (unwrap failures unknown)"; return; fi
  failures=$(metric vaultaire_tenant_keys_unwrap_failures)
  if [ -z "$failures" ]; then gate master_key UNKNOWN "master key: credential, vaultaire_tenant_keys_unwrap_failures absent"; return; fi
  if [ "${failures%.*}" != 0 ]; then gate master_key RED "master key: credential, but $failures tenant key row(s) do not unwrap"; return; fi
  gate master_key GREEN "master key: credential (vaultaire@$PORT since $since), 0 unwrap failures"
}

# metric_is_one GATE NAME [LABEL] — GREEN at 1, RED at 0, UNKNOWN when unreadable/absent.
metric_is_one() {
  local g=$1 name=$2 label=${3:-} v shown=$2
  [ -z "$label" ] || shown="$name{$label}"
  if ! $METRICS_OK; then gate "$g" UNKNOWN "/metrics of the active slot did not answer"; return; fi
  v=$(metric "$name" "$label")
  case "${v%.*}" in
    1) gate "$g" GREEN "$shown 1" ;;
    '') gate "$g" UNKNOWN "$shown absent from /metrics" ;;
    *) gate "$g" RED "$shown $v" ;;
  esac
}

# dump_sealed: the gauge pg-backup.sh writes (WP-P42).
gate_dump_sealed() {
  local f=$METRICS_DIR/vaultaire_backup_dump_encrypted.prom v
  if [ ! -r "$METRICS_DIR" ] || [ ! -x "$METRICS_DIR" ]; then gate dump_sealed UNKNOWN "$METRICS_DIR is not readable"; return; fi
  if [ ! -e "$f" ]; then gate dump_sealed RED "no vaultaire_backup_dump_encrypted gauge (pg-backup.sh before WP-P42, or no run since)"; return; fi
  v=$(awk '$1 == "vaultaire_backup_dump_encrypted" { print $2; exit }' "$f" 2>/dev/null)
  case "$v" in
    1) gate dump_sealed GREEN "vaultaire_backup_dump_encrypted 1 (the last dump was sealed with age)" ;;
    0) gate dump_sealed RED "vaultaire_backup_dump_encrypted 0 (no recipients file: the dump is plaintext)" ;;
    *) gate dump_sealed UNKNOWN "vaultaire_backup_dump_encrypted unreadable in $f" ;;
  esac
}

LIVE_UNWRAPPED_SQL="/* lr:unwrapped */ SELECT (SELECT COUNT(*) FROM api_keys WHERE revoked_at IS NULL AND tenant_id IS NOT NULL AND secret_wrapped IS NULL AND secret_key IS NOT NULL) + (SELECT COUNT(*) FROM sts_tokens WHERE expires_at > NOW() AND secret_wrapped IS NULL AND secret_key IS NOT NULL)"
DROP_FLAG_SQL="/* lr:dropflag */ SELECT COALESCE((SELECT enabled::text FROM feature_flags WHERE flag_key = 'credential_plaintext_drop' AND tenant_id = '*'), 'none')"
PLAINTEXT_SQL="/* lr:plaintext */ SELECT (SELECT COUNT(*) FROM api_keys WHERE secret_key IS NOT NULL) || ' ' || (SELECT COUNT(*) FROM sts_tokens WHERE secret_key IS NOT NULL) || ' ' || (SELECT COUNT(*) FROM tenants WHERE secret_key IS NOT NULL)"
# Live keys and tokens created no later than the flag row's last write (B3: every key
# rotated after the drop — the pre-flip dumps and PITR hold their old secrets).
UNROTATED_SQL="/* lr:unrotated */ WITH f AS (SELECT updated_at AS at FROM feature_flags WHERE flag_key = 'credential_plaintext_drop' AND tenant_id = '*' AND enabled) SELECT (SELECT COUNT(*) FROM api_keys, f WHERE revoked_at IS NULL AND tenant_id IS NOT NULL AND created_at <= f.at) || ' ' || (SELECT COUNT(*) FROM sts_tokens, f WHERE expires_at > NOW() AND created_at <= f.at)"

gate_credentials_wrapped() {
  local n
  if ! $DB_OK || ! n=$(q "$LIVE_UNWRAPPED_SQL") || [ -z "$n" ]; then gate credentials_wrapped UNKNOWN "the database did not answer"; return; fi
  if [ "$n" = 0 ]; then gate credentials_wrapped GREEN "0 live api_keys/sts_tokens rows hold only the plaintext"
  else gate credentials_wrapped RED "$n live api_keys/sts_tokens row(s) hold only the plaintext (no master key, or key_reencrypt has not run)"; fi
}

drop_flag() { q "$DROP_FLAG_SQL"; }

gate_plaintext_dropped() {
  local flag counts ak st tn
  if ! $DB_OK || ! flag=$(drop_flag) || [ -z "$flag" ] || ! counts=$(q "$PLAINTEXT_SQL") || [ -z "$counts" ]; then
    gate plaintext_dropped UNKNOWN "the database did not answer"; return
  fi
  read -r ak st tn <<< "$counts"
  if [ "$flag" != true ]; then gate plaintext_dropped RED "credential_plaintext_drop is off (global row: $flag); plaintext secrets: api_keys $ak, sts_tokens $st, tenants $tn"; return; fi
  if [ "$ak/$st/$tn" = 0/0/0 ]; then gate plaintext_dropped GREEN "credential_plaintext_drop on; 0 plaintext secrets in api_keys, sts_tokens, tenants"
  else gate plaintext_dropped RED "credential_plaintext_drop on, but plaintext secrets remain: api_keys $ak, sts_tokens $st, tenants $tn (key_reencrypt nulls them run by run)"; fi
}

gate_keys_rotated() {
  local flag counts ak st
  if ! $DB_OK || ! flag=$(drop_flag) || [ -z "$flag" ]; then gate keys_rotated UNKNOWN "the database did not answer"; return; fi
  if [ "$flag" != true ]; then gate keys_rotated RED "credential_plaintext_drop is off: nothing has been rotated after a drop"; return; fi
  if ! counts=$(q "$UNROTATED_SQL") || [ -z "$counts" ]; then gate keys_rotated UNKNOWN "the database did not answer"; return; fi
  read -r ak st <<< "$counts"
  if [ "$ak/$st" = 0/0 ]; then gate keys_rotated GREEN "every live key and STS token was created after the drop flag's last write"
  else gate keys_rotated RED "$ak live key(s) and $st live STS token(s) predate the drop: rotate them (their old secrets are in the pre-flip dumps and PITR)"; fi
}

# env_flag_off GATE KEY — GREEN while KEY is absent from the .env or set to an off value.
env_flag_off() {
  local g=$1 key=$2
  if ! $ENV_READABLE; then gate "$g" UNKNOWN "$ENV_FILE is not readable"; return; fi
  if ! env_has "$key"; then gate "$g" GREEN "$key absent from .env"; return; fi
  if is_off "$(env_val "$key")"; then gate "$g" GREEN "$key set to an off value"
  else gate "$g" RED "$key is set in .env (remove it: writes may skip the seal)"; fi
}

# signups_open: the DB row wins over SIGNUPS_ENABLED (default true), as flags.Service does.
gate_signups_open() {
  local row
  if ! $DB_OK || ! row=$(q "/* lr:signups */ SELECT COALESCE((SELECT enabled::text FROM feature_flags WHERE flag_key = 'signups' AND tenant_id = '*'), 'none')") || [ -z "$row" ]; then
    gate signups_open UNKNOWN "the database did not answer"; return
  fi
  case "$row" in
    true) gate signups_open GREEN "signups flag row: on" ;;
    false) gate signups_open RED "signups flag row: off (closed; /register 303s to the waitlist)" ;;
    *)
      if ! $ENV_READABLE; then gate signups_open UNKNOWN "no signups flag row and $ENV_FILE is not readable"
      elif ! env_has SIGNUPS_ENABLED || ! is_off "$(env_val SIGNUPS_ENABLED)"; then gate signups_open GREEN "no flag row; SIGNUPS_ENABLED default/on"
      else gate signups_open RED "no flag row; SIGNUPS_ENABLED is off in .env"; fi ;;
  esac
}

# --- info gates -------------------------------------------------------------------------

# env_is_target GATE — VAULTAIRE_ENDPOINT / VAULTAIRE_PUBLIC_HOST against the WP-DOMAIN-1
# targets (compared here, never printed).
gate_domain() {
  local ep host bad=()
  if ! $ENV_READABLE; then gate domain_switched UNKNOWN "$ENV_FILE is not readable"; return; fi
  ep=$(env_val VAULTAIRE_ENDPOINT) || ep=
  host=$(env_val VAULTAIRE_PUBLIC_HOST) || host=
  [ "${ep%/}" = "$TARGET_ENDPOINT" ] || bad+=("VAULTAIRE_ENDPOINT is not $TARGET_ENDPOINT")
  [ "${host%/}" = "$TARGET_PUBLIC_HOST" ] || bad+=("VAULTAIRE_PUBLIC_HOST is not $TARGET_PUBLIC_HOST")
  if [ ${#bad[@]} = 0 ]; then gate domain_switched GREEN "VAULTAIRE_ENDPOINT and VAULTAIRE_PUBLIC_HOST are the WP-DOMAIN-1 targets"
  else gate domain_switched RED "$(IFS=';'; echo "${bad[*]}")"; fi
}

gate_tenant_zero() {
  local tz n
  if ! $DB_OK; then gate tenant_zero_objects UNKNOWN "the database did not answer"; return; fi
  if [ -n "${LR_TENANT_ZERO:-}" ]; then
    tz=$LR_TENANT_ZERO
  elif ! tz=$(q "/* lr:tz */ SELECT CASE WHEN COUNT(*) = 1 THEN MIN(t.id) ELSE '' END FROM tenants t JOIN users u ON u.email = t.email WHERE u.role = 'admin'") || [ -z "$tz" ]; then
    gate tenant_zero_objects UNKNOWN "tenant zero is not one admin's tenant (set LR_TENANT_ZERO)"; return
  fi
  case "$tz" in *[!A-Za-z0-9_-]*) gate tenant_zero_objects UNKNOWN "LR_TENANT_ZERO is not a tenant id"; return ;; esac
  if ! n=$(q "/* lr:tzobjects */ SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = '$tz'") || [ -z "$n" ]; then
    gate tenant_zero_objects UNKNOWN "the database did not answer"; return
  fi
  if [ "$n" -gt 0 ]; then gate tenant_zero_objects GREEN "tenant zero ($tz) holds $n object(s)"
  else gate tenant_zero_objects RED "tenant zero ($tz) holds no object"; fi
}

gate_alerts() {
  local json n names
  if ! json=$("$CURL" -sf --max-time 10 "$ALERTMANAGER_URL/api/v2/alerts?active=true&silenced=false&inhibited=false" 2>/dev/null) ||
     ! n=$(printf '%s' "$json" | jq 'length' 2>/dev/null); then
    gate alerts_firing UNKNOWN "Alertmanager ($ALERTMANAGER_URL) did not answer"; return
  fi
  if [ "$n" = 0 ]; then gate alerts_firing GREEN "0 alerts firing"; return; fi
  names=$(printf '%s' "$json" | jq -r '[.[].labels.alertname] | unique | join(", ")' 2>/dev/null)
  gate alerts_firing RED "$n alert(s) firing: $names"
}

gate_release_key() {
  if [ -r "$RELEASE_VKEY" ] && head -1 "$RELEASE_VKEY" | grep -q '^stored\.cloud/release/v1+'; then
    gate release_signing_key GREEN "verifier key in $RELEASE_VKEY (stored.cloud/release/v1)"
  else
    gate release_signing_key RED "no stored.cloud/release/v1 verifier key at $RELEASE_VKEY (docs/RELEASES.md ceremony)"
  fi
}

gate_master_key
metric_is_one translog_signing_key vaultaire_translog_signing_key_loaded
gate_dump_sealed
gate_credentials_wrapped
gate_plaintext_dropped
gate_keys_rotated
env_flag_off blomp_sealed BLOMP_ALLOW_UNSEALED
env_flag_off sia_sealed SIA_ALLOW_UNSEALED
env_flag_off geyser_sealed GEYSER_ALLOW_UNSEALED
metric_is_one stripe_ready vaultaire_payment_provider_ready 'provider="stripe"'
gate_signups_open
gate_domain
metric_is_one synthetic_check vaultaire_synthetic_check_success
gate_tenant_zero
gate_alerts
gate_release_key

green=0
for r in "${RESULTS[@]}"; do case "$r" in *" GREEN "*) green=$((green + 1)) ;; esac; done
echo "summary: $green/${#RESULTS[@]} GREEN; launch gates not GREEN: $LAUNCH_NOT_GREEN"

if $WRITE_METRICS; then
  if tmp=$(mktemp "$METRICS_DIR/.vaultaire_launch_gate.XXXXXX" 2>/dev/null); then
    if ! {
      echo "# HELP vaultaire_launch_gate 1 = the launch switch is on (GREEN), 0 = off or unknown (deploy/scripts/launch-readiness.sh)"
      echo "# TYPE vaultaire_launch_gate gauge"
      for r in "${RESULTS[@]}"; do
        read -r name status class <<< "$r"
        v=0; [ "$status" = GREEN ] && v=1
        echo "vaultaire_launch_gate{gate=\"$name\",class=\"$class\"} $v"
      done
    } > "$tmp" || ! chmod 0644 "$tmp" || ! mv -f "$tmp" "$METRICS_DIR/vaultaire_launch_gate.prom"; then
      rm -f "$tmp"; echo "METRICS: cannot write $METRICS_DIR/vaultaire_launch_gate.prom (non-fatal)" >&2
    fi
  else
    echo "METRICS: cannot write to $METRICS_DIR (non-fatal)" >&2
  fi
fi

[ "$LAUNCH_NOT_GREEN" = 0 ]
