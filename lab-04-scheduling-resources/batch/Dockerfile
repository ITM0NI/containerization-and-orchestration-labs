FROM golang:1.25-bookworm AS build

WORKDIR /src

COPY go.mod main.go ./

RUN CGO_ENABLED=0 GOOS=linux \
    go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/batch \
    .

FROM scratch

COPY --from=build /out/batch /batch

EXPOSE 8080

ENTRYPOINT ["/batch"]