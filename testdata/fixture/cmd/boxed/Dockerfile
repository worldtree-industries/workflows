# Built by the reusable workflow's docker path (build: dockerfile), with the
# Project root as the build context. The build stage runs on the runner's own
# platform and cross-compiles, so building for other platforms needs no
# emulation.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/boxed ./cmd/boxed

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/boxed /boxed
ENTRYPOINT ["/boxed"]
