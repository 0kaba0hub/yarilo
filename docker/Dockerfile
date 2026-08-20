# syntax=docker/dockerfile:1.7
FROM golang:1.26-alpine AS builder

ARG BUILD_VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    set -eu; \
    for cmd in \
        yarilo \
        yarilo-auth \
        yarilo-warden \
        yarctl \
        yarilo-backend-api \
        yarilo-backend-reg \
        yarilo-director \
        yarilo-imap \
        yarilo-imap-login \
        yarilo-pop3 \
        yarilo-pop3-login \
        yarilo-lmtp \
        yarilo-lmtp-login \
        yarilo-submission \
        yarilo-submission-login \
        yarilo-locks \
        yarilo-migrate \
        yarilo-quota-status \
        yarilo-sasl-login \
        yarilo-managesieve \
        yarilo-managesieve-login \
        yarilo-jmap-login \
        yarilo-jmap \
    ; do \
        go build -ldflags="-s -w -X github.com/yarilomail/yarilo/pkg/build.Version=${BUILD_VERSION}" \
            -trimpath -o "/out/$cmd" "./app/$cmd"; \
    done
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go build -ldflags="-s -w -X github.com/yarilomail/yarilo/pkg/build.Version=${BUILD_VERSION}" \
    -trimpath -o /out/yarilo-smoketest ./app/smoketest

## --- yarilo-fts: the only cgo binary (libxapian). Built in its own stage so
## every other binary stays CGO_ENABLED=0 static; ships in the single image.
FROM golang:1.26-alpine AS fts-builder
# hadolint ignore=DL3018
RUN apk add --no-cache build-base xapian-core-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
ARG BUILD_VERSION=dev
ENV CGO_ENABLED=1 GOOS=linux GOARCH=amd64
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    for cmd in yarilo-fts fts-bench; do \
        go build -tags flatcurve -ldflags="-s -w -X github.com/yarilomail/yarilo/pkg/build.Version=${BUILD_VERSION}" \
            -trimpath -o "/out/$cmd" "./app/$cmd"; \
    done

FROM alpine:3.24
# hadolint ignore=DL3018,DL3019
# tini is PID 1 (see ENTRYPOINT): the components that run external programs --
# the director's flush hook, sieve pipe/filter/execute, the quota-warning
# script -- leave orphaned grandchildren behind when one is killed, and Go
# reaps only its own children. With the binary itself as PID 1 those orphans
# stayed as zombies, one per killed hook, against the pod's PID limit (#1373).
RUN apk upgrade --no-cache && apk add --no-cache ca-certificates tzdata xapian-core libstdc++ tini && \
    addgroup -g 1000 yarilo && \
    adduser -u 1000 -G yarilo -s /sbin/nologin -D yarilo

COPY --from=builder /out/ /usr/local/bin/
COPY --from=fts-builder /out/yarilo-fts /usr/local/bin/yarilo-fts
COPY --from=fts-builder /out/fts-bench /usr/local/bin/fts-bench
# Backward-compat alias for the operator CLI: the binary is now `yarctl`, but
# existing tooling (sandbox test jobs, older scripts) still invokes it as
# `yarilo-admin`. A relative symlink (busybox multi-call convention) keeps that
# name working. A hardlink to yarctl, which lives in the lower COPY layer,
# would force an overlayfs copy-up of the ~15 MB binary into THIS layer (image
# bloat); a symlink is a few bytes, resolves in the same dir, and the CLI does
# not branch on argv[0].
RUN ln -s yarctl /usr/local/bin/yarilo-admin
COPY docker/entrypoint.sh /entrypoint.sh
COPY config/yarilo.yaml /etc/yarilo/yarilo.yaml

# Pre-create the storage + state dirs owned by the runtime user. A Docker named
# volume mounted over one of these inherits the image dir's ownership, so the
# uid-1000 process can write (SQLite userdb, maildir) on first start. Without
# this the volume is created root:root and the minimal Compose stack crash-loops.
RUN chmod +x /entrypoint.sh && \
    mkdir -p /var/lib/yarilo /var/mail/vhosts && \
    chown yarilo:yarilo /var/lib/yarilo /var/mail/vhosts

USER 1000:1000

EXPOSE 143 993 465 587 8080

# No -g: the entrypoint execs the component, so tini's direct child IS the
# component and the default signal path already reaches it. Signalling the
# whole group would also hit external programs that did not put themselves in
# one -- a sieve pipe mid-write is not something a shutdown should interrupt.
ENTRYPOINT ["/sbin/tini", "--", "/entrypoint.sh"]
