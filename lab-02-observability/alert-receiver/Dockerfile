FROM golang:1.25.6-bookworm AS build

WORKDIR /src
ENV GOTOOLCHAIN=local

COPY go.mod main.go ./
RUN CGO_ENABLED=0 GOOS=linux \
    go build -mod=readonly -trimpath -ldflags="-s -w" -o /out/alert-receiver .

FROM scratch

COPY --from=build /out/alert-receiver /alert-receiver

USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/alert-receiver"]
