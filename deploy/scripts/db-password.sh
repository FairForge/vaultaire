#!/bin/bash
# db-password.sh — sourced, never run: defines db_password_from_env, the ONE way a shell
# reads DB_PASSWORD out of /opt/vaultaire/configs/.env. deploy.yml copies it to the box and
# sources it before the migrations; pg-backup.sh carries the same function (it is installed
# as a single file). pg-backup_test.sh asserts both copies give the same answer on every
# fixture — `grep DB_PASSWORD | cut -d= -f2` truncated a password at its second `=`, kept
# its quotes and matched DB_PASSWORD_OLD (#633).

# db_password_from_env FILE — prints the value of the first DB_PASSWORD= line of an
# .env file, with ONE level of surrounding quotes removed (what systemd's EnvironmentFile
# does). Everything after the first `=` is the value: a password may contain `=`.
# Returns 1 when the file has no such line; prints nothing for an empty value.
db_password_from_env() {
  local line value
  line=$(grep -m1 '^DB_PASSWORD=' "$1") || return 1
  value=${line#DB_PASSWORD=}
  case "$value" in
    \"*\") value=${value#\"}; value=${value%\"} ;;
    \'*\') value=${value#\'}; value=${value%\'} ;;
  esac
  printf '%s' "$value"
}
