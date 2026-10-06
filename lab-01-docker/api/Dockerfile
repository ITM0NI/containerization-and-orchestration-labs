FROM golang:1.25-bookworm AS build

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY main.go ./

RUN CGO_ENABLED=0 GOOS=linux \
    go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/lab1-api \
    .

FROM scratch

COPY --from=build /out/lab1-api /lab1-api

EXPOSE 8080

ENTRYPOINT ["/lab1-api"]