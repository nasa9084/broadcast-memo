FROM golang:1.16 AS build

WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 go build -o /main /app/main.go

FROM alpine:latest
COPY --from=build /main /main

ENV PORT=${PORT}
ENV REDIS_URL=${REDIS_URL}
ENV USERNAME=${USERNAME}
ENV PASSWORD=${PASSWORD}

CMD [ "/main" ]
