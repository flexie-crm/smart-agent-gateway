# The inference node.
#
# Deliberately not part of the default stack. It is the one image that takes
# twenty minutes to build and tens of gigabytes to be useful, and the machine
# that runs models is often not the machine that runs everything else. Bring it
# up with `--profile machine` when it belongs on the same box.
#
# The accelerator is a BUILD choice, not a runtime one (KB/35): the engine is
# compiled for the hardware it will run on. `ACCEL` selects it, and none means
# the processor, which is correct for a box with no GPU and slow for one with.
ARG ACCEL=

FROM rust:1.97-bookworm AS build
ARG ACCEL
WORKDIR /src

# What the engine and the TLS stack need to compile. cmake and clang are for the
# cryptography; pkg-config and libssl-dev for the HTTP client.
RUN apt-get update && apt-get install -y --no-install-recommends \
        cmake clang pkg-config libssl-dev \
    && rm -rf /var/lib/apt/lists/*

COPY . .
RUN cargo build --release ${ACCEL:+--features $ACCEL}

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates libssl3 \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 10001 --create-home --shell /usr/sbin/nologin sag

COPY --from=build /src/target/release/sag-inference /usr/local/bin/sag-inference

# The one directory this node owns: its key, the certificate it serves with, the
# authority it trusts, and the weights. All of it is worth keeping, and none of
# it is worth rebuilding, so it is a volume.
RUN mkdir -p /var/lib/sag-inference && chown -R sag:sag /var/lib/sag-inference
USER sag
ENV SAG_NODE_DATA=/var/lib/sag-inference \
    SAG_NODE_ADDR=0.0.0.0:19443
EXPOSE 19443
ENTRYPOINT ["sag-inference"]
CMD ["serve"]
