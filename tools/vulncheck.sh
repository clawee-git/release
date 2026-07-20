#!/usr/bin/env bash
# vulncheck.sh — release-time CVE gate helpers, sourced by tools/release.sh.
# Kept self-contained so a future shared release flow can lift it unchanged.
# Ported from burrowee-git/release (clawee has no dispatcher/relay — it scans
# exactly the components a cut ships).

# resolve_release_mode <apple> <vulncheck> <answer>
# Folds the interactive prompt answer into the final signing/scan modes and
# prints "<apple>|<vulncheck>" (each "1" or empty). A y/Y answer forces both on.
resolve_release_mode() {
    local apple="$1" vuln="$2" ans="$3"
    case "${ans}" in [yY]*) apple=1; vuln=1 ;; esac
    printf '%s|%s' "${apple}" "${vuln}"
}

# vulncheck_scan_dirs — prints "name<TAB>dir" for every module a cut ships.
# Requires src_for() and the COMPONENTS array (both defined in release.sh).
vulncheck_scan_dirs() {
    local c
    for c in "${COMPONENTS[@]}"; do
        printf '%s\t%s\n' "${c}" "$(src_for "${c}")"
    done
}

# vulncheck_gate — hard CVE gate. No-op unless VULNCHECK is set. Scans every
# shipped module with source-mode govulncheck (GOWORK=off, matching build.sh's
# tag-pinned resolution); any non-zero scan (finding or scan error) aborts the
# whole release cut.
vulncheck_gate() {
    [ -n "${VULNCHECK:-}" ] || return 0
    local gv="${GOVULNCHECK:-govulncheck}"
    command -v "${gv}" >/dev/null 2>&1 || gv="$("${GO_BIN:-go}" env GOPATH 2>/dev/null)/bin/govulncheck"
    { command -v "${gv}" >/dev/null 2>&1 || [ -x "${gv}" ]; } \
        || { echo "✗ --vulncheck set but govulncheck not found (install: go install golang.org/x/vuln/cmd/govulncheck@latest)" >&2; exit 1; }

    local report_dir="${REPO_ROOT}/dist/vulncheck" failed=0 name dir
    mkdir -p "${report_dir}"
    while IFS=$'\t' read -r name dir; do
        [ -n "${name}" ] || continue
        echo "→ govulncheck: ${name} (${dir})" >&2
        if ( cd "${dir}" && GOWORK=off "${gv}" ./... ) >"${report_dir}/${name}.txt" 2>&1; then
            echo "✓ govulncheck: ${name} clean" >&2
        else
            echo "✗ govulncheck: ${name} — known vulnerability or scan error (report: ${report_dir}/${name}.txt)" >&2
            cat "${report_dir}/${name}.txt" >&2
            failed=1
        fi
    done < <(vulncheck_scan_dirs)

    [ "${failed}" = 0 ] || { echo "✗ CVE gate failed — release aborted" >&2; exit 1; }
}
