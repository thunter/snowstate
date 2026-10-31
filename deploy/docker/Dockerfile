# Build from the repository root:
#   docker build -f deploy/docker/Dockerfile -t snowstate .
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/thunter/snowstate/internal/buildinfo.Version=${VERSION} -X github.com/thunter/snowstate/internal/buildinfo.Commit=${COMMIT}" \
    -o /out/snowstate ./cmd/snowstate

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/snowstate /snowstate
USER 65532:65532
EXPOSE 8443
ENTRYPOINT ["/snowstate"]
CMD ["serve"]
