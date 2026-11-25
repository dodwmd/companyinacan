# Throw-away image for test/hostfw: real nft, ip and socat, away from the host's network stack.
FROM docker.io/library/debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends nftables iproute2 socat procps ca-certificates && rm -rf /var/lib/apt/lists/*
