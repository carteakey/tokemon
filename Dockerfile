FROM golang:1.26 AS build

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

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tokemon /tokemon
COPY catalog/models.yaml /catalog/models.yaml
EXPOSE 8080
ENTRYPOINT ["/tokemon"]
