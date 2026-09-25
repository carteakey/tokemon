# Keep the build reproducible across runners. The Go manifest digest was
# resolved with `docker buildx imagetools inspect` on 2026-09-24.
FROM golang:1.26.8@sha256:6c2a5538f964f1c82f97ad14988bf05de100d922d159d0e398b54c7b0ca0c6c9 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TOKEMON_VERSION=dev
ARG TOKEMON_COMMIT=unknown
ARG TOKEMON_BUILD_DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/tokemon/tokemon/internal/version.Version=${TOKEMON_VERSION} -X github.com/tokemon/tokemon/internal/version.Commit=${TOKEMON_COMMIT} -X github.com/tokemon/tokemon/internal/version.BuildDate=${TOKEMON_BUILD_DATE}" \
    -o /out/tokemon ./cmd/tokemon

FROM gcr.io/distroless/static-debian12:nonroot@sha256:f5b485ea962d9bd1186b2f6b3a061191539b905b82ec395de78cbfae51f20e35
COPY --from=build /out/tokemon /tokemon
COPY catalog/models.yaml /catalog/models.yaml
EXPOSE 8080
ENTRYPOINT ["/tokemon"]
