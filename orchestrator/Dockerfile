# The orchestrator, and the console it serves.
#
# One image, two roles (`server` and `worker`), and the built console inside it.
# The console is NOT a separate service on purpose (KB/19): in production the
# orchestrator serves the files itself so the page and the API are one origin,
# which is what lets the session cookie be `SameSite=Lax` and path-scoped rather
# than a cross-site arrangement with the exemptions that needs.
#
# The build context is the REPOSITORY ROOT, because the console's sources live
# beside the server's and one image needs both.

FROM node:22-alpine AS console
WORKDIR /src
COPY admin-ui/package*.json ./
RUN npm ci
# One directory above the front end, because that is where its vite.config.ts
# imports it from: both front ends stamp themselves with the commit they were
# built at, and the script that reads it is shared rather than copied. Copying
# `admin-ui/` alone builds an image whose console cannot configure itself, and
# the failure is a vite config that will not load.
COPY scripts/ /scripts/
COPY admin-ui/ ./
RUN npm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY orchestrator/go.mod orchestrator/go.sum ./
RUN go mod download
COPY orchestrator/ ./
# Passed in by deploy/build.sh, which has the repository this does not.
ARG SAG_VERSION=unknown
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$SAG_VERSION" -o /out/sag ./cmd/sag

FROM alpine:3.20
RUN adduser -D -u 10001 sag
COPY --from=build /out/sag /usr/local/bin/sag
COPY --from=console /src/dist /srv/console
# Uploaded bytes live here, on a volume. Owned by the account that runs, because
# a directory it cannot write to is a boot failure rather than a later surprise.
RUN mkdir -p /var/lib/sag/uploads && chown -R sag:sag /var/lib/sag
USER sag
ENV SAG_CONSOLE_DIR=/srv/console \
    SAG_UPLOAD_DIR=/var/lib/sag/uploads
EXPOSE 8080
ENTRYPOINT ["sag"]
# One image, two roles: the worker container overrides this with "worker".
CMD ["server"]
