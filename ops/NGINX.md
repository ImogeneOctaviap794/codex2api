# nginx 配置要点

## 1. 真实链路

```
客户端 → CF(橙云, 100s idle)
       → 跳板(.23 / .55 CF DNS 轮询)
       → new-api-horizon:30002
       → cx.wyzai.top                          ← node2 nginx、SSL 终结
       → http://10.10.0.2:8104                 ← WireGuard 隧道，同 IDC RTT ~0.6ms
       → codex-node docker codex2api (内 8123)
       → OpenAI
```

**2026-05-15 架构调整**：upstream 从 `127.0.0.1:8122/8123` 改为 `10.10.0.2:8104`。node2 仅留 nginx + PG，应用层迁 codex-node。详见 `ARCHITECTURE.md`。

shell.wyzai.top 的 CF DNS 为两条 A 记录负载均衡：
- 156.238.226.23（SSH 18604）
- 156.238.226.55（SSH 24598）

## 2. 跳板 nginx · 关 SSE buffer

**文件**：`/www/server/panel/vhost/nginx/proxy/shell.wyzai.top/api-sse.conf`

**根因**：宝塔默认 `proxy_buffering on + 16KB buffer + 300s timeout`。对 reasoning=xhigh 这种常 >60s 的长 SSE：
- 单个 event 被积攒到 16KB 才下发
- 300s 超时 nginx 断连
- buffer 里半个 `data: ...` 送给客户端 → Rust reqwest 报 `error decoding response body`

**修复**：给 `/v1/`、`/hf/v1/`、`/pg/` 加独立 location（原 `/` 保持，内置 `sub_filter` 暗色主题注入仅对 HTML 生效，nginx 按最长前缀匹配）：

```nginx
location ^~ /v1/ {
    proxy_pass http://152.53.240.159:30002;
    client_max_body_size 100m;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_http_version 1.1;

    proxy_buffering off;
    proxy_cache off;
    proxy_request_buffering off;

    proxy_connect_timeout 60s;
    proxy_read_timeout 86400s;
    proxy_send_timeout 86400s;
    send_timeout 86400s;

    proxy_set_header Accept-Encoding "identity";
}
# /hf/v1/ 和 /pg/ 同构
```

**两台跳板都要改**：CF 轮询会漏流量到另一台。

## 3. Node2 · cx.wyzai.top

**文件**：`/www/server/panel/vhost/nginx/cx.wyzai.top.conf`

**client_max_body_size 500m**（2026-04-27 修，之前继承全局 50m 经常 413）

**关键配置（在 `proxy_pass http://10.10.0.2:8104;` 之后）**：

```nginx
proxy_buffering off;
proxy_cache off;
proxy_request_buffering off;
proxy_read_timeout 86400s;
proxy_send_timeout 86400s;
send_timeout 86400s;
```

## 4. 关键细节

- **`sub_filter` 默认只处理 `text/html`**，对 `text/event-stream` 不介入，**本身不是罪魁**
- 罪魁是 `proxy_buffering on + 16KB + 300s timeout` 的组合
- `proxy_buffering off` 下 `sub_filter` 仍能工作（output filter 独立于 proxy 缓冲）
- `Accept-Encoding: identity` 防止上游返回 gzip/br 被重压缩破坏 SSE 帧边界
- nginx 前缀 `location ^~ /v1/` 比 `^~ /` 长，优先匹配；**无需删除原 `/` 块**
- `codex2api` 代码层已发 `X-Accel-Buffering: no`（`proxy/handler.go`），无需改

## 5. 如果仍复现 decode error

按优先级排查：
1. **WireGuard 隧道中断**（2026-05-15+）：`wg show wg0 latest-handshakes` 查握手时间，ping `10.10.0.2`。隧道挂了所有请求会 502
2. **Cloudflare 100s idle 超时**（橙云硬限）：需要 CF DNS 灰云，或靠 `codex2api` 的 SSE/JSON keepalive ticker（v1.7.45+ 已发 10s 间隔空 chunk）
3. **new-api-horizon 渠道 timeout**：K8s Pod 默认渠道超时，管理后台把 cx.wyzai.top 渠道 timeout 调到 300s+
4. **代码层**：v1.7.45 已发 `X-Accel-Buffering: no` 和 keepalive，不必再改

## 6. 验证 command

```bash
# 跳板
curl https://shell.wyzai.top/v1/models   # 401（缺 key 正常），首字节 0.21-0.30s

# Node2 (公网 → nginx → WG → codex-node)
curl -s https://cx.wyzai.top/health

# WG 隧道连通性（在 node2 上）
ping -c 3 10.10.0.2
curl -s http://10.10.0.2:8104/health
```

## 7. WireGuard 隧道透明代理

- **服务端**：node2 (`10.10.0.1:51820/UDP`)，systemd `wg-quick@wg0`
- **客户端**：codex-node (`10.10.0.2`)，PersistentKeepalive 25s
- **nginx 在 node2 上只需以普通 HTTP 反代 `10.10.0.2:8104`**，WG 对 nginx 透明，不需额外配置
- RTT ~0.6ms（同 IDC Manassas），与原 `127.0.0.1` 本机反代的性能几乎一致
- 详见 `ARCHITECTURE.md`

## 8. 图床 `img.niji.edu.rs`（混合 try_files + WG 反代）

> 2026-05-15 迁移后，图生成由 codex-node 应用层写入；URL 形如 `https://img.niji.edu.rs/<sha256_8B>.png`。
>
> 老图 7.8GB 仍在 node2 `/data/codex2api-images/`（不迁，迁了费工无收益）；新图（5/15 19:30 后）在 codex-node `/data/codex2api-images/`，由 `nginx:alpine` 容器 serve。

**文件**：`/www/server/panel/vhost/nginx/img.niji.edu.rs.conf`

```nginx
server {
    listen 80;
    server_name img.niji.edu.rs;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    http2 on;
    server_name img.niji.edu.rs;

    ssl_certificate /www/server/panel/vhost/letsencrypt/img.niji.edu.rs/fullchain.pem;
    ssl_certificate_key /www/server/panel/vhost/letsencrypt/img.niji.edu.rs/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_ciphers EECDH+CHACHA20:EECDH+AES128:RSA+AES128:EECDH+AES256:RSA+AES256;
    ssl_prefer_server_ciphers on;
    add_header Strict-Transport-Security "max-age=31536000" always;

    # 老图本地读，找不到 fallback 走 WG 到 codex-node
    location ~* \.(png|jpg|jpeg|webp)$ {
        root /data/codex2api-images;
        try_files $uri @codex_node;
        add_header Cache-Control "public, immutable, max-age=2592000";
        access_log off;
    }

    location @codex_node {
        proxy_pass http://10.10.0.2:8888;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        add_header Cache-Control "public, immutable, max-age=2592000";
        access_log off;
    }

    location / { return 404; }

    access_log /www/wwwlogs/img.niji.edu.rs.log;
    error_log /www/wwwlogs/img.niji.edu.rs.error.log;
}
```

**codex-node 上游 nginx:alpine 容器配置**（`/tmp/codex-images-nginx.conf`，挂载到容器 `/etc/nginx/conf.d/default.conf`）：

```nginx
server {
    listen 80;
    server_name _;
    root /usr/share/nginx/html;
    location ~* \.(png|jpe?g|webp)$ {
        add_header Cache-Control "public, immutable, max-age=2592000";
        access_log off;
        try_files $uri =404;
    }
    location = /health { return 200 'ok'; access_log off; }
    location / { return 404; }
}
```

**关键设计**：

- `try_files $uri @codex_node`：老图先尝试 node2 本地（最快 0ms），找不到再 fallback WG 反代（+0.6ms RTT）
- 文件名是 SHA-256 前 8B hex（16 字符），全局唯一不冲突，老图新图共享同一命名空间无歧义
- `codex2api` 容器**不**暴露 `/images/`，静态服务整体交给 `codex-images-nginx` 容器（`nginx:alpine`，只读挂盘 `/data/codex2api-images:ro`）
- 改完 nginx 必须 `nginx -t && nginx -s reload`，宝塔 vhost 改完不要忘 reload

**部署/启停**：见 `DEPLOYMENT.md` §8 图床节。
