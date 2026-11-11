FROM golang:1.26 AS build
WORKDIR /src
COPY extensions/station/go.mod extensions/station/go.sum ./extensions/station/
COPY api/go.mod api/go.sum ./api/
COPY api/ ./api/
COPY extensions/station/ ./extensions/station/
WORKDIR /src/extensions/station
RUN GOWORK=off CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /station-controller ./cmd/station-controller

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /station-controller /station-controller
USER 65532:65532
ENTRYPOINT ["/station-controller"]
