# 部署到 Ubuntu 服务器

本项目通过 GitHub Actions 自动构建 Docker 镜像并推送到 GHCR（`ghcr.io/huguanjin/wc002-taidong-billtool`），
服务器端用 `docker compose` 拉取镜像运行即可，无需在服务器上安装 Go / Node。

## 0. 前置条件

- Ubuntu 服务器已安装 [Docker Engine](https://docs.docker.com/engine/install/ubuntu/) 及 Compose 插件（`docker compose version` 能正常输出）。
- 服务器能访问 `ghcr.io`（国内服务器如访问受限，需自行配置镜像加速或代理）。
- 已推送代码到 `main` 分支或打过 `v*.*.*` 标签，触发过一次 [.github/workflows/docker-publish.yml](.github/workflows/docker-publish.yml) 并构建成功
  （去 GitHub 仓库 Actions 页签确认，或去 Packages 页签确认镜像已存在）。

## 1. 在服务器上准备部署目录

```bash
sudo mkdir -p /opt/taidong-bill
cd /opt/taidong-bill
```

从本地仓库拷贝以下文件/目录到服务器的 `/opt/taidong-bill`（用 `scp` / `rsync` / Git 拉取均可，任选其一）：

```
docker-compose.yml
.env.example
data/bill_template.xlsx
data/price_table.xlsx
```

> `data/` 里的两个 xlsx 没有提交到 git 仓库（见 [.gitignore](.gitignore)），需要手动拷贝，例如：
> ```bash
> scp docker-compose.yml .env.example ubuntu@<服务器IP>:/opt/taidong-bill/
> scp -r data ubuntu@<服务器IP>:/opt/taidong-bill/
> ```

## 2. 配置 `.env`

```bash
cp .env.example .env
vim .env   # 修改 BILL_AUTH_USERNAME / BILL_AUTH_PASSWORD 为真实账号密码
```

如果源日志文件另外存放在服务器某个目录，想用「服务器路径 / 可视化选择文件」功能直接读取，
额外打开 `.env` 里的 `BILL_BROWSE_ROOT` 注释并改成容器内路径，同时在 `docker-compose.yml` 里
取消对应 `volumes` 挂载的注释（详见文件内注释）。

## 3. （如果镜像包是 private）登录 GHCR

GHCR 上新推送的包默认是 private。两种方式二选一：

- **推荐**：去 GitHub 仓库 Packages 页面把包设为 Public，服务器端无需登录即可拉取。
- **或者**：在服务器上用具备 `read:packages` 权限的 GitHub PAT 登录：
  ```bash
  echo <你的PAT> | docker login ghcr.io -u <你的GitHub用户名> --password-stdin
  ```

## 4. 启动服务

```bash
cd /opt/taidong-bill
docker compose pull
docker compose up -d
```

## 5. 验证

```bash
# 查看容器状态（STATUS 应显示 healthy）
docker compose ps

# 查看启动日志
docker compose logs -f --tail=100

# 健康检查
curl http://127.0.0.1:8080/api/health
```

浏览器访问 `http://<服务器IP>:8080`，用 `.env` 里配置的账号密码登录，测试上传/生成账单。

## 6. 更新版本

代码有新变更并合并到 `main`（或打了新 tag）后，GitHub Actions 会自动构建并推送新镜像。
服务器上拉取最新镜像并重启容器即可：

```bash
cd /opt/taidong-bill
docker compose pull
docker compose up -d
```

## 7. 对外暴露 / HTTPS（可选）

`docker-compose.yml` 默认只监听 `8080` 端口且没有 TLS。生产环境建议在前面加一层反向代理
（Nginx / Caddy / Traefik）做 HTTPS 终结和域名转发，例如 Nginx 示例：

```nginx
server {
    listen 443 ssl;
    server_name bill.example.com;

    ssl_certificate     /etc/letsencrypt/live/bill.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/bill.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }
}
```

配好反向代理后，可以把 `docker-compose.yml` 里的端口映射改成只监听本机：`"127.0.0.1:8080:8080"`，
避免容器端口直接暴露在公网。

## 8. 备份

需要定期备份的内容：

- `data/`：账单模板、报价表（业务基础数据，改动不频繁）。
- 命名卷 `bill-jobs`（上传/生成的临时文件，默认 6 小时自动清理，一般无需备份）：
  ```bash
  docker run --rm -v taidong-bill_bill-jobs:/data -v $(pwd):/backup alpine \
    tar czf /backup/bill-jobs-backup.tar.gz -C /data .
  ```
- `.env`：账号密码配置，妥善保管，不要提交到 git。

## 常见问题

| 现象 | 排查方向 |
|------|----------|
| `docker compose pull` 报 403/未授权 | 镜像包是 private，参照第 3 步登录 GHCR，或把包设为 Public |
| 健康检查一直 unhealthy | `docker compose logs` 看后端是否因缺少 `data/` 下的模板文件启动失败 |
| 页面能打开但生成账单报错「账单模板不存在」 | 确认 `data/bill_template.xlsx`、`data/price_table.xlsx` 已放在部署目录并正确挂载 |
| 登录一直提示用户名密码错误 | 确认 `.env` 已生效：`docker compose config` 查看解析后的环境变量 |
| 想用服务器路径读取源文件但报「路径超出允许范围」 | 检查 `BILL_BROWSE_ROOT` 与实际 `volumes` 挂载路径是否一致 |
