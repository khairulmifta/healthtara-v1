FROM golang:1.27.2-alpine AS build
WORKDIR /build
COPY source/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -trimpath -ldflags="-s -w" -o /healthtara .
FROM scratch
COPY --from=build /healthtara /app/healthtara
COPY docs/ /app/docs/
COPY data/ /app/data/
WORKDIR /app
ENV HEALTHTARA_MODE=cloud HEALTHTARA_DATA_DIR=/var/data PORT=8080
EXPOSE 8080
ENTRYPOINT ["/app/healthtara", "--cloud"]
