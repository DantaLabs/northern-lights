# Build with the repository root as the ACR task context.
# Set VCS_REF to the commit SHA in the ACR build; local builds default to dev.
FROM golang:1.27-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VCS_REF=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -X main.version=${VCS_REF}" -o /out/workiva-mcp ./cmd/workiva-mcp

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
COPY --from=build /out/workiva-mcp /workiva-mcp
COPY deployments/content-intake/entrypoint.sh /entrypoint.sh
RUN chmod 0555 /entrypoint.sh /workiva-mcp
ENV NL_DB_PATH=/tmp/nl-private/db/northern-lights.db
EXPOSE 8080
ENTRYPOINT ["/entrypoint.sh"]
