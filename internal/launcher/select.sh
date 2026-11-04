# This shell ends after selection. No functions are exported to Homebrew or
# CiderLoad, so the download engine can still exec the original curl normally.
source "$HOMEBREW_LIBRARY/Homebrew/utils.sh" || exit 2
source "$HOMEBREW_LIBRARY/Homebrew/utils/curl.sh" || exit 2

function /usr/bin/curl() { return 1; }
eval "function ${HOMEBREW_PREFIX}/opt/curl/bin/curl() { return 1; }" || exit 2

setup_curl
if [[ "$HOMEBREW_CURL" != "$HOMEBREW_CURL_PATH" ]]; then
  # Recover from a stale selection without touching ordinary brew's cache.
  rm -f "$HOMEBREW_CACHE/tool-selection/curl" || exit 2
  setup_curl
fi
if [[ "$HOMEBREW_CURL" != "$HOMEBREW_CURL_PATH" ]]; then
  printf 'cidl: Homebrew selected %s instead of %s\n' "$HOMEBREW_CURL" "$HOMEBREW_CURL_PATH" >&2
  exit 2
fi
if [[ ! -s "$HOMEBREW_CACHE/tool-selection/curl" ]]; then
  printf '%s\n' 'cidl: this Homebrew does not provide the expected curl selection cache' >&2
  exit 2
fi

# Rejection above is only a selection preference. Preserve the real system
# curl's suitability so Homebrew does not mistake it for an obsolete tool.
cidl_system_usable=0
cidl_version_pattern='^curl ([0-9]+\.[0-9]+(\.[0-9]+)?)'
if cidl_version_output="$(command /usr/bin/curl --version 2>/dev/null)" &&
   [[ "$cidl_version_output" =~ $cidl_version_pattern ]] &&
   [[ "$(numeric "${BASH_REMATCH[1]}")" -ge "$(numeric "$HOMEBREW_MINIMUM_CURL_VERSION")" ]]; then
  cidl_system_usable=1
fi

cidl_cache="$HOMEBREW_CACHE/tool-selection/curl"
if ! {
  IFS= read -r -d '' cidl_key &&
  IFS= read -r -d '' cidl_path &&
  IFS= read -r -d '' cidl_version &&
  IFS= read -r -d '' cidl_cached_system
} < "$cidl_cache"; then
  printf '%s\n' 'cidl: unsupported Homebrew curl selection cache' >&2
  exit 2
fi
if [[ "$cidl_path" != "$HOMEBREW_CURL_PATH" || ! "$cidl_cached_system" =~ ^[01]$ ]]; then
  printf '%s\n' 'cidl: unexpected Homebrew curl selection record' >&2
  exit 2
fi
if [[ "$cidl_cached_system" != "$cidl_system_usable" ]]; then
  cidl_temporary="$(mktemp "$cidl_cache.XXXXXX")" || exit 2
  trap 'rm -f "$cidl_temporary"' EXIT
  printf '%s\0' "$cidl_key" "$cidl_path" "$cidl_version" "$cidl_system_usable" > "$cidl_temporary" || exit 2
  mv -f "$cidl_temporary" "$cidl_cache" || exit 2
fi
