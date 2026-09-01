# Built from the repository root, not from this directory: the source is part
# of the main module, and giving it a nested go.mod would drop it out of
# `go build ./...` and `go vet ./...` at the root -- so it would stop being
# compiled by CI and quietly rot.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY test/kind/fakemetrics/ test/kind/fakemetrics/
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
      -o /out/fakemetrics ./test/kind/fakemetrics

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/fakemetrics /fakemetrics
USER 65532:65532
ENTRYPOINT ["/fakemetrics"]
