# Everstack from its public GitHub release.
#
# The container images are in a private registry, but the server binary is
# published on the public releases page, so the subject under test can be built
# by anyone with no credentials at all. That matters: a benchmark whose subject
# only the author can run is not reproducible.
#
# https://github.com/everstacklabs/everstack/releases
FROM debian:bookworm-slim

ARG EVERSTACK_VERSION=v0.1.25
# Set by buildkit; matches the release asset naming (amd64 / arm64).
ARG TARGETARCH

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    url="https://github.com/everstacklabs/everstack/releases/download/${EVERSTACK_VERSION}/everstack-linux-${TARGETARCH}.gz"; \
    curl -fsSL -o /tmp/everstack.gz "$url"; \
    gunzip -c /tmp/everstack.gz > /usr/local/bin/everstack; \
    chmod +x /usr/local/bin/everstack; \
    rm /tmp/everstack.gz; \
    /usr/local/bin/everstack version || true

# The released archive contains only the binary, but the server reads two sets
# of files from disk at paths relative to its working directory:
#
#   cmd/config/gateway/schemas/     config validation schemas
#   internal/database/migrations/   the SQL migrations it applies at startup
#
# Without the migrations it logs "postgres migrations applied and schema
# verified" and then fails on the first query, because it globbed an empty
# directory and had nothing to apply. Both sets live in the public repository at
# the same tag, so fetching the source tarball keeps this build credential-free.
RUN set -eux; \
    curl -fsSL -o /tmp/src.tar.gz \
      "https://github.com/everstacklabs/everstack/archive/refs/tags/${EVERSTACK_VERSION}.tar.gz"; \
    mkdir -p /app; \
    tar -xzf /tmp/src.tar.gz -C /tmp; \
    src="$(find /tmp -maxdepth 1 -type d -name 'everstack-*' | head -1)"; \
    mkdir -p /app/cmd/config/gateway /app/internal/database/migrations; \
    cp -r "$src/cmd/config/gateway/schemas" /app/cmd/config/gateway/schemas; \
    cp -r "$src/internal/database/migrations/sql" /app/internal/database/migrations/sql; \
    rm -rf /tmp/src.tar.gz "$src"; \
    ls /app/internal/database/migrations/sql/postgres | wc -l

WORKDIR /app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/everstack"]
