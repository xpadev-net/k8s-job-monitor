# ビルドステージ
FROM golang:1.23-alpine AS builder

WORKDIR /app

# 依存関係をコピーしてインストール
COPY go.mod .
RUN go mod download

# ソースコードをコピー
COPY . .

# アプリケーションをビルド
RUN CGO_ENABLED=0 GOOS=linux go build -a -o k8s-job-monitor .

# 実行ステージ
FROM alpine:3.18

WORKDIR /app

# ビルドステージから実行可能ファイルをコピー
COPY --from=builder /app/k8s-job-monitor .

# 実行ユーザーを設定
RUN adduser -D -u 10001 appuser
USER 10001

ENTRYPOINT ["/app/k8s-job-monitor"]
