FROM golang:1.27.3-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/packcove ./cmd/packcove
RUN mkdir -p /out/data && touch /out/data/.keep

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=65532:65532 /out/packcove /packcove
COPY --from=build --chown=65532:65532 /out/data/ /var/lib/packcove/data/
COPY --chown=65532:65532 packcove.yaml /etc/packcove/packcove.yaml

WORKDIR /var/lib/packcove
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/packcove"]
CMD ["-config", "/etc/packcove/packcove.yaml"]
