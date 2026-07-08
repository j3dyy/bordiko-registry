# Standalone image for the Bordiko registry (marketplace).
#
# Build context is THIS directory (no go.work dependency), so the service can be
# extracted into its own repo. Persist packages with REGISTRY_DATA_DIR + a volume.
FROM golang:1.26-bookworm AS build
ENV CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -o /out/app .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
EXPOSE 8082
ENTRYPOINT ["/app"]
