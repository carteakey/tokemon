# Keep the build reproducible across runners. The Go manifest digest was
# resolved from Docker Hub's official image index with
# `docker buildx imagetools inspect` on 2026-11-12 (bumped from 1.26.10 for
# GO-2026-6859, the net/http MaxBytesReader header race fixed in 1.26.11).
FROM golang:1.26.11@sha256:957952ae74dc010608717144399a59a2c6e82ea03d63be1c7ff6577c5a135f93 AS build

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
