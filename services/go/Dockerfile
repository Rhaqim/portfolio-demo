# One image per Go service: docker build --build-arg CMD=buckt .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG CMD
RUN test -n "$CMD" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}
# The shared volume is created from this directory, so it must be owned by
# the distroless nonroot user (65532) or no service can write to it.
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
COPY --from=build --chown=65532:65532 /out/data /data
USER nonroot
EXPOSE 8080
ENTRYPOINT ["/app"]
