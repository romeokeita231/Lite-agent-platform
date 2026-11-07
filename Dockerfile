FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/platform ./cmd/platform \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/policy-example ./cmd/policy-example

FROM alpine:3.20 AS platform
COPY --from=build /out/platform /platform
EXPOSE 8080
CMD ["/platform"]

FROM alpine:3.20 AS policy-example
COPY --from=build /out/policy-example /policy-example
EXPOSE 9090
CMD ["/policy-example"]
