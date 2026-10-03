FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -o /chlift-soak ./cmd/chlift-soak

FROM gcr.io/distroless/static
COPY --from=build /chlift-soak /chlift-soak
ENTRYPOINT ["/chlift-soak"]
