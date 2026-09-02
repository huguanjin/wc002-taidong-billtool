# ---- 前端构建 ----
FROM node:20-alpine AS frontend-builder
WORKDIR /app/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# ---- 后端构建 ----
FROM golang:1.25-alpine AS backend-builder
WORKDIR /app/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/server .

# ---- 运行时镜像 ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app

COPY --from=backend-builder /out/server ./server
COPY --from=frontend-builder /app/frontend/dist ./frontend/dist
# 账单模板/报价表不随镜像打包（未纳入版本库），运行时通过 volume 挂载 /app/data
RUN mkdir -p ./data

ENV BILL_ADDR=:8080 \
    BILL_DATA_DIR=/app/data \
    BILL_FRONTEND_DIST=/app/frontend/dist \
    BILL_JOB_DIR=/tmp/taidong-bill-jobs

EXPOSE 8080
ENTRYPOINT ["/app/server"]
