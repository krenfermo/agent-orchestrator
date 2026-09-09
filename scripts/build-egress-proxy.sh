#!/usr/bin/env bash
# Build ao-egress-proxy reproducibly, for the Linux architectures a skill
# container can run on, and check the result against its recorded provenance.
#
# The proxy is a Linux binary that has to reach a container. It is NOT compiled
# while a skill runs: it is built here, at AO build time, pinned by SHA-256, and
# embedded into the daemon by `go build -tags ao_embed_egress_proxy`.
#
# Reproducibility is the whole point of the flag set below. `-trimpath` removes
# the build machine's paths, `-buildvcs=false` removes the commit and the dirty
# bit, `-buildid=` removes the content-derived id that otherwise varies, and
# CGO_ENABLED=0 removes the host toolchain. What is left depends only on the
# source and the Go version -- so two people on two machines get the same bytes,
# and `--check` proves it rather than asserting it.
#
# Usage:
#   scripts/build-egress-proxy.sh            build, then verify against provenance
#   scripts/build-egress-proxy.sh --record   build, then REWRITE the provenance
#   scripts/build-egress-proxy.sh --check    build twice and compare; verify too
#   scripts/build-egress-proxy.sh --clean    remove the built artifacts
#
# --record is a deliberate, reviewable act: the diff it produces is the digest
# change, and the toolchain that produced it. Nothing here records silently.
set -euo pipefail

# ARTIFACT_VERSION identifies the packaged proxy for a person. Bump it when the
# proxy's source changes; the SHA-256 is what a machine compares.
ARTIFACT_VERSION="1"

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
backend_dir="${repo_root}/backend"
pkg_dir="${backend_dir}/internal/skillegress/proxybin"
out_dir="${pkg_dir}/artifacts"
provenance="${pkg_dir}/provenance.json"

SOURCE_PKG="./internal/skillegress/cmd/ao-egress-proxy"
BUILD_ENV="CGO_ENABLED=0"
BUILD_FLAGS="-trimpath -buildvcs=false -ldflags=-s -w -buildid="
ARCHES=(amd64 arm64)

mode="verify"
case "${1:-}" in
  "") ;;
  --record) mode="record" ;;
  --check) mode="check" ;;
  --clean) mode="clean" ;;
  *) printf 'unknown argument: %s\n' "$1" >&2; exit 2 ;;
esac

if [[ "${mode}" == "clean" ]]; then
  rm -f "${out_dir}"/ao-egress-proxy-linux-*
  printf 'Removed the built proxy artifacts from %s\n' "${out_dir}"
  exit 0
fi

command -v go >/dev/null || { printf 'go is not on PATH\n' >&2; exit 1; }
toolchain="$(cd "${backend_dir}" && go env GOVERSION)"

digest_of() {
  # One digest, no filename, on every platform this repo builds on.
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

size_of() {
  # BSD stat and GNU stat disagree on the flag; ask wc instead.
  wc -c < "$1" | tr -d ' '
}

build_into() {
  local dest="$1" arch="$2"
  ( cd "${backend_dir}" && \
    CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" GOFLAGS='' \
      go build -trimpath -buildvcs=false -ldflags="-s -w -buildid=" \
        -o "${dest}" "${SOURCE_PKG}" )
}

mkdir -p "${out_dir}"

declare -a files=() digests=() sizes=()
for arch in "${ARCHES[@]}"; do
  name="ao-egress-proxy-linux-${arch}"
  path="${out_dir}/${name}"
  build_into "${path}" "${arch}"
  chmod 0555 "${path}"
  size="$(size_of "${path}")"
  files+=("${name}")
  digests+=("$(digest_of "${path}")")
  sizes+=("${size}")
  printf 'Built %s (%s bytes)\n' "${name}" "${size}"
done

if [[ "${mode}" == "check" ]]; then
  # Reproducibility is a claim until a second build in a different directory
  # produces the same bytes. Do that, and say so.
  tmp="$(mktemp -d)"
  trap 'rm -rf "${tmp}"' EXIT
  for i in "${!ARCHES[@]}"; do
    arch="${ARCHES[$i]}"
    build_into "${tmp}/${files[$i]}" "${arch}"
    again="$(digest_of "${tmp}/${files[$i]}")"
    if [[ "${again}" != "${digests[$i]}" ]]; then
      printf 'NOT REPRODUCIBLE: %s rebuilt to %s, first build was %s\n' \
        "${files[$i]}" "${again}" "${digests[$i]}" >&2
      exit 1
    fi
    printf 'Reproducible: %s rebuilds byte-identical (%s)\n' "${files[$i]}" "${again}"
  done
fi

if [[ "${mode}" == "record" ]]; then
  {
    printf '{\n'
    printf '  "artifactVersion": "%s",\n' "${ARTIFACT_VERSION}"
    printf '  "source": "%s",\n' "${SOURCE_PKG}"
    printf '  "toolchain": "%s",\n' "${toolchain}"
    printf '  "buildEnv": "%s",\n' "${BUILD_ENV}"
    printf '  "buildFlags": "%s",\n' "${BUILD_FLAGS}"
    printf '  "artifacts": [\n'
    for i in "${!files[@]}"; do
      sep=","
      [[ $i -eq $(( ${#files[@]} - 1 )) ]] && sep=""
      printf '    {\n'
      printf '      "goos": "linux",\n'
      printf '      "goarch": "%s",\n' "${ARCHES[$i]}"
      printf '      "file": "%s",\n' "${files[$i]}"
      printf '      "sha256": "%s",\n' "${digests[$i]}"
      printf '      "bytes": %s\n' "${sizes[$i]}"
      printf '    }%s\n' "${sep}"
    done
    printf '  ]\n'
    printf '}\n'
  } > "${provenance}"
  printf 'Recorded %s\n' "${provenance}"
  exit 0
fi

# Verify: every digest this build produced must be the one already recorded.
[[ -f "${provenance}" ]] || { printf 'no provenance at %s; run --record\n' "${provenance}" >&2; exit 1; }
recorded_toolchain="$(sed -n 's/.*"toolchain": "\([^"]*\)".*/\1/p' "${provenance}")"
failed=0
for i in "${!files[@]}"; do
  recorded="$(grep -A3 "\"file\": \"${files[$i]}\"" "${provenance}" \
    | sed -n 's/.*"sha256": "\([0-9a-f]*\)".*/\1/p')"
  if [[ "${recorded}" != "${digests[$i]}" ]]; then
    printf 'DIGEST MISMATCH for %s\n  built:    %s\n  recorded: %s\n' \
      "${files[$i]}" "${digests[$i]}" "${recorded}" >&2
    failed=1
  else
    printf 'Verified %s %s\n' "${files[$i]}" "${digests[$i]}"
  fi
done
if [[ ${failed} -ne 0 ]]; then
  if [[ "${recorded_toolchain}" != "${toolchain}" ]]; then
    printf '\nThe provenance was recorded with %s and this machine has %s.\n' \
      "${recorded_toolchain}" "${toolchain}" >&2
    printf 'A Go version change moves every digest. Build with %s, or re-record\n' \
      "${recorded_toolchain}" >&2
    printf 'with --record so the new toolchain and the new digests land in one\n' >&2
    printf 'reviewable diff.\n' >&2
  else
    printf '\nSame toolchain, different bytes: the proxy source changed without\n' >&2
    printf 'the provenance being re-recorded. Run --record and review the diff.\n' >&2
  fi
  exit 1
fi
printf 'All artifacts match %s (toolchain %s)\n' "${provenance}" "${toolchain}"
