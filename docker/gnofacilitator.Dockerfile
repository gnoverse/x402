# Release image for gnofacilitator. goreleaser builds the static binary
# (CGO_ENABLED=0) and places it in the build context; this only packages it.
# Build via the release pipeline, not `docker build` directly.
#
# The facilitator holds no keys. It verifies payments offline and relays
# client-signed transactions, so a compromised one cannot move funds anywhere the
# payer did not already sign for. It does need a reachable node:
#
#   docker run --rm -p 8402:8402 ghcr.io/gnoverse/gnofacilitator:latest \
#     -rpc https://rpc.sapphire.testnets.gno.land:443 -chain-id sapphire-1
#
# -listen already defaults to all interfaces on :8402, so publishing the port is
# enough — unlike a service that binds loopback for host safety.
FROM gcr.io/distroless/static-debian12:nonroot
# buildx provides TARGETOS/TARGETARCH; goreleaser lays each platform's binary
# under <os>/<arch>/ in the build context for multi-platform images.
ARG TARGETOS
ARG TARGETARCH
COPY ${TARGETOS}/${TARGETARCH}/gnofacilitator /usr/local/bin/gnofacilitator
EXPOSE 8402
ENTRYPOINT ["/usr/local/bin/gnofacilitator"]
