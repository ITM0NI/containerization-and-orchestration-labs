FROM golang:1.25.6-bookworm AS build

WORKDIR /src
ENV GOTOOLCHAIN=local

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux \
    go build -mod=readonly -trimpath -ldflags="-s -w" -o /out/lab2-api .

FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/lab2-api /lab2-api

USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/lab2-api"]
