FROM golang:1.27.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN go vet ./... && go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /heating-curve .

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /heating-curve /heating-curve
ENTRYPOINT ["/heating-curve"]
