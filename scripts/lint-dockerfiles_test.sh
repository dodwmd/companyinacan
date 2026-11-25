#!/bin/sh
set -eu
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/deploy/x"
d="$tmp/deploy/x/Dockerfile"
good='FROM golang:1.27@sha256:aaaa AS build
FROM gcr.io/distroless/static-debian12:nonroot@sha256:bbbb
USER 65532:65532
'
printf '%s' "$good" >"$d"
sh "$here/lint-dockerfiles.sh" --release "$tmp" >/dev/null || { echo "FAIL: good Dockerfile rejected"; exit 1; }

expect_fail() { # description, content, extra args
	printf '%s' "$2" >"$d"
	# shellcheck disable=SC2086
	if sh "$here/lint-dockerfiles.sh" ${3:-} "$tmp" >/dev/null 2>&1; then echo "FAIL: $1 accepted"; exit 1; fi
}
expect_fail "unpinned FROM" 'FROM golang:1.27
USER 1
'
expect_fail ":latest" 'FROM foo:latest
USER 1
'
expect_fail "missing USER" 'FROM a@sha256:aaaa
'
expect_fail "root user" 'FROM a@sha256:aaaa
USER root
'
expect_fail "curl pipe sh" 'FROM a@sha256:aaaa
RUN curl -fsSL https://x.example/i | sh
USER 1
'
expect_fail "ADD url" 'FROM a@sha256:aaaa
ADD https://x.example/f /f
USER 1
'
# shellcheck disable=SC2016
ph='ARG D=sha256:REPLACE_WITH_X
FROM a@${D}
USER 1
'
printf '%s' "$ph" >"$d"
sh "$here/lint-dockerfiles.sh" "$tmp" >/dev/null || { echo "FAIL: placeholder rejected in dev mode"; exit 1; }
expect_fail "placeholder in release mode" "$ph" --release
echo "lint-dockerfiles_test: ok"
