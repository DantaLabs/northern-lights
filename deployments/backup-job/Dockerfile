FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/nl-backup-job ./cmd/nl-backup-job
COPY internal/backupjob ./internal/backupjob
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/nl-backup-job ./cmd/nl-backup-job

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/nl-backup-job /nl-backup-job
USER nonroot:nonroot
ENTRYPOINT ["/nl-backup-job"]
