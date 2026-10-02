# Sourced by scripts/check-standards.sh and scripts/fetch-standards.sh:
# reads api/standards/SOURCE.
#
#   source_files            the file names (section headers), one per line
#   source_get FILE KEY     the value of KEY in FILE's section ("" if none)
#   sha256_of PATH          the SHA-256 of PATH, hex

source_path=${source_path:-api/standards/SOURCE}

source_files() {
  sed -n 's/^\[\(.*\)\]$/\1/p' "$source_path"
}

source_get() {
  awk -v want="[$1]" -v key="$2" '
    /^\[/ { in_section = ($0 == want); next }
    in_section && index($0, key " = ") == 1 { print substr($0, length(key) + 4); exit }
  ' "$source_path"
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum <"$1" | cut -d' ' -f1
  else
    shasum -a 256 <"$1" | cut -d' ' -f1
  fi
}

# core_get FILE KEY: KEY from uspace-core's SOURCE for FILE's standard,
# at the version go.mod pins, read from the module cache.
core_get() {
  local module rel dir
  read -r module rel <<<"$(source_get "$1" core_source)"
  if ! dir=$("${GO:-go}" list -m -f '{{.Dir}}' "$module"); then
    echo "standards: go list -m $module failed (above)" >&2
    return 1
  fi
  if [ -z "$dir" ] || [ ! -f "$dir/$rel" ]; then
    echo "standards: $module $rel is not in the module cache (go mod download)" >&2
    return 1
  fi
  sed -n "s/^$2 = //p" "$dir/$rel"
}
