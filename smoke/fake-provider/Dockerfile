FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/fake-provider .

FROM scratch

COPY --from=build /out/fake-provider /usr/local/bin/fake-provider
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/fake-provider"]
