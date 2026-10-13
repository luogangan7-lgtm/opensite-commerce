# Build once from locked Go modules; API contains no administrative credential.
FROM golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS build
ENV GOTOOLCHAIN=local CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -tags pgx -trimpath -o /out/api ./cmd/api && go build -trimpath -o /out/healthcheck ./cmd/healthcheck

FROM postgres:18.6@sha256:74935e72241653ca55e0414067e6d8763aceb8a810eb51b452253ec3dcfc4336 AS migrator
RUN apt-get update && apt-get install -y --no-install-recommends python3 && rm -rf /var/lib/apt/lists/*
WORKDIR /workspace
COPY migrations ./migrations
COPY scripts/migrate.sh scripts/ops_deploy.py ./scripts/
ENTRYPOINT ["python3", "/workspace/scripts/ops_deploy.py"]
CMD ["install"]

FROM postgres:18.6@sha256:74935e72241653ca55e0414067e6d8763aceb8a810eb51b452253ec3dcfc4336 AS api
COPY --from=build /out/api /out/healthcheck /usr/local/bin/
RUN mkdir -p /var/lib/opensite/media && chown 65532:65532 /var/lib/opensite/media
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/api"]
HEALTHCHECK --interval=5s --timeout=4s --retries=15 CMD ["/usr/local/bin/healthcheck"]
