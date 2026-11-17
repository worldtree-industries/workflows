# Built by the reusable workflow's docker path (build: dockerfile), with the
# Project root as the build context.
FROM golang:1.27 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /out/boxed ./cmd/boxed

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/boxed /boxed
ENTRYPOINT ["/boxed"]
