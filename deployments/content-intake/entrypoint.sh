#!/bin/sh
set -eu
umask 077

# Container Apps provides the dedicated key as a Key Vault-backed secretRef
# environment variable. Materialize only a private runtime file; never log it.
: "${NL_CONTENT_STORAGE_KEY_BOOTSTRAP:?content storage key secret reference is required}"
: "${NL_CONTENT_STORAGE_KEY_ID:?content storage key ID is required}"
case "$NL_CONTENT_STORAGE_KEY_BOOTSTRAP" in
  *[!0123456789abcdefABCDEF]*|'')
    echo 'content storage key bootstrap value is malformed' >&2
    exit 1
    ;;
esac
[ "${#NL_CONTENT_STORAGE_KEY_BOOTSTRAP}" -eq 64 ] || {
  echo 'content storage key bootstrap value has invalid length' >&2
  exit 1
}

private_root=/tmp/nl-private
db_parent="$private_root/db"
key_parent="$private_root/key"
mkdir -p "$db_parent" "$key_parent"
chmod 0700 "$private_root" "$db_parent" "$key_parent"
[ ! -L "$private_root" ] && [ ! -L "$db_parent" ] && [ ! -L "$key_parent" ] || {
  echo 'private runtime path traverses a symlink' >&2
  exit 1
}

key_file="$key_parent/content-storage.key"
tmp_key="$key_parent/.content-storage.key.$$"
( set -C; printf '%s\n' "$NL_CONTENT_STORAGE_KEY_BOOTSTRAP" > "$tmp_key" ) || {
  echo 'unable to create private content key file' >&2
  exit 1
}
chmod 0600 "$tmp_key"
mv "$tmp_key" "$key_file"
unset NL_CONTENT_STORAGE_KEY_BOOTSTRAP
unset tmp_key
export NL_CONTENT_STORAGE_KEY_FILE="$key_file"
export NL_DB_PATH="$db_parent/northern-lights.db"

exec /workiva-mcp
