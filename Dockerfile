FROM golang:1.26.5-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/lexinet ./cmd/lexinet

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /
COPY --from=build /out/lexinet /usr/local/bin/lexinet

ENV LEXINET_CONFIG=/etc/lexinet/config.toml
EXPOSE 17892
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/lexinet"]
CMD ["serve", "--bind", "0.0.0.0:17892", "--tailnet", "off"]
