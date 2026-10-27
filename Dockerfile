FROM golang:1.27.3 AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /aka-provisioner ./cmd/aka-provisioner
# 自動生成するサーバー証明書を保存するディレクトリ。ボリュームの初期の所有者を nonroot にするため、ここで作る。
RUN mkdir -p /out/data/tls

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /aka-provisioner /aka-provisioner
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENTRYPOINT ["/aka-provisioner"]
CMD ["serve"]
