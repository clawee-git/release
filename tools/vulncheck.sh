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

# vulncheck_scan_dirs — prints "name<TAB>dir" for every MODULE a cut ships.
# Requires src_for() and the COMPONENTS array (both defined in release.sh).
#
# The set is DISCOVERED, not declared. Every go.mod under a component's source
# root is its own module with its own dependency graph, so scanning the parent
# says nothing about it. clawee's two components are single-module today, which
# is exactly why this must not be a hand-maintained list: the day one of them
# grows a nested module, a declared list keeps reporting success while shipping
# it unscanned, and a gate that silently under-covers looks identical to a clean
# scan. (Ported from burrowee-git/release, where relay/cli was that module.)
#
# Per-component BINARIES (clawee-updater, claweed-updater) are cmd/ dirs inside
# their module, so `./...` at the module root already covers them; they are not
# separate entries and do not need to be.
vulncheck_scan_dirs() {
    local c root
    for c in "${COMPONENTS[@]}"; do
        root="$(src_for "${c}")"
        [ -n "${root}" ] || continue
        vulncheck_modules_under "${c}" "${root}"
    done
}

# vulncheck_modules_under <name> <root> — one line per go.mod beneath root.
# The root module keeps <name>; a nested one is <name>-<subpath>.
#
# A root with NO go.mod is still emitted, so the gate tries to scan it and
# reports it as unscannable. Dropping it here would ship an unchecked module
# while the gate reported success.
vulncheck_modules_under() {
    local name="$1" root="$2" mod dir sub found=0
    # Normalise a trailing slash away FIRST. dirname never returns one, so a
    # root that carries one (SRC_CLAWEE/SRC_CLAWEED are operator-overridable via
    # CLAWEE_SRC_*, and a path pasted with a trailing slash is ordinary) would
    # fail to prefix-match the dirname of its OWN go.mod — the root module's
    # sub-path would come out as the entire absolute path, and its report would
    # land at dist/vulncheck/clawee-Volumes-…-cli.txt instead of clawee.txt.
    # Not fail-open (the right dir is still scanned and a finding still aborts),
    # but it silently breaks the naming contract this function documents.
    root="${root%/}"
    [ -n "${root}" ] || root=/
    while IFS= read -r mod; do
        [ -n "${mod}" ] || continue
        found=1
        dir="$(dirname "${mod}")"
        sub="${dir#"${root}"}"; sub="${sub#/}"
        printf '%s\t%s\n' "${name}${sub:+-${sub//\//-}}" "${dir}"
    done <<EOF
$(find "${root}" -name go.mod -not -path '*/vendor/*' -not -path '*/node_modules/*' 2>/dev/null \
    | awk -F/ '{print NF"\t"$0}' | sort -n -k1,1 -k2,2 | cut -f2-)
EOF
    [ "${found}" = 1 ] || printf '%s\t%s\n' "${name}" "${root}"
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
