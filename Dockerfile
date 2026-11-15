FROM golang:1.26-alpine AS build
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY server/ .
RUN CGO_ENABLED=0 go build -o /acq-server .

FROM scratch
# TLS root CAs: without them every outbound HTTPS call fails x509
# verification (scratch ships nothing).
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /acq-server /acq-server
# Canonical public origin for absolute links (invite/share/install). Prevents
# client Host/X-Forwarded-Proto headers from poisoning generated links.
# Self-hosters override with their own origin.
ENV ACQ_PUBLIC_BASE_URL=https://acq.damri.org
ENTRYPOINT ["/acq-server"]
