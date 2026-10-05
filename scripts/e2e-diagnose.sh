# Sourced by the e2e scripts. diagnose prints what a failed run leaves in its
# work directory, so a failure on a CI runner, whose work directory goes with
# it, can be read from the log: the agents' output, the hook log, the server's
# log, and what the model was sent.
diagnose() {
  local w=$1 f
  printf '\n== diagnostics (%s)\n' "$w"
  for f in "$w"/*.out "$w"/*.err "$w"/server.log "$w"/model.log; do
    [ -s "$f" ] || continue
    printf -- '--- %s (last 40 lines)\n' "${f#"$w"/}"
    tail -n 40 "$f"
  done
  find "$w" -name hook.log -not -path '*/node_modules/*' 2>/dev/null | while read -r f; do
    printf -- '--- %s (last 60 lines)\n' "${f#"$w"/}"
    tail -n 60 "$f"
  done
  if ls "$w"/model/req-*.json >/dev/null 2>&1; then
    printf -- '--- model requests\n'
    ls -la "$w"/model/
    for f in "$w"/model/req-*.json; do
      printf -- '--- %s: what it says about intagent\n' "${f#"$w"/}"
      grep -o '.\{0,160\}intagent.\{0,240\}' "$f" | head -n 6
    done
  fi
}
