# Alert notification receiver

A small, dependency-free Go service that records Alertmanager webhook notifications
as JSON logs on stdout. It is a notification sink for the lab.

- `GET /health` returns `200 ok` without a request log.
- `POST /alerts` accepts one Alertmanager version 4 JSON notification, up to 1 MiB.
- `HTTP_ADDR` sets the listen address; the default is `:8080`.

Each accepted notification produces one INFO record with the message
`alert notification received`, the group status and labels, receiver, and all
typed alert details, including individual status, labels, annotations, timestamps,
and an optional fingerprint. Unknown JSON fields are ignored for compatibility.
Malformed notifications return 400; oversized bodies return 413. A log sink failure
returns 503 so Alertmanager can retry.

A successful response means the notification was written to the log sink. There is
no persistence, forwarding, or deduplication; repeated Alertmanager notifications
produce repeated records.

The Dockerfile builds a static binary with Go 1.25.6 and runs it in `scratch` as
UID/GID 65532. The service makes no outbound requests.

Run the local checks with `go test -race ./...` and `go vet ./...`.
Webhook schema reference:
[Alertmanager webhook configuration](https://prometheus.io/docs/alerting/latest/configuration/#webhook_config).
