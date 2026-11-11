FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN GOWORK=off go mod download
COPY main.go readiness.go status.go ./
RUN CGO_ENABLED=0 GOWORK=off go build -trimpath -o /station-gateway .
FROM gcr.io/distroless/static-debian12:nonroot
USER 0:0
COPY --from=build /station-gateway /station-gateway
ENTRYPOINT ["/station-gateway"]
