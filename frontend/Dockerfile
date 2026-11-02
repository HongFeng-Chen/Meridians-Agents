# RAG 控制台前端镜像：Vite 构建产物 + nginx 静态托管
#   docker build -t go-agent-frontend:dev .
#
# 运行时 nginx 配置由 k8s ConfigMap 挂到 /etc/nginx/conf.d/default.conf，
# 负责把 /api/ 反代到 go-agent Service；镜像内只放静态文件。

FROM node:20-alpine AS builder

WORKDIR /app

COPY package.json package-lock.json ./
RUN npm config set registry https://registry.npmmirror.com && npm ci

COPY . .
# base 为相对路径，产物可直接被静态服务器托管；API 走同源 /api
RUN npm run build

FROM nginx:1.27-alpine

COPY --from=builder /app/dist /usr/share/nginx/html

EXPOSE 80
