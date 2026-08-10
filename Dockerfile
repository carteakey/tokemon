# Keep the build reproducible across runners. These are the multi-platform
# manifest digests resolved and checked with `docker buildx imagetools inspect`
# on 2026-08-10; updates must be deliberate and reviewed.
FROM golang:1.26@sha256:2005724102f45917a63e9d092fc0e4ea56ea575048ce147caad5f5f61502c365 AS build

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
