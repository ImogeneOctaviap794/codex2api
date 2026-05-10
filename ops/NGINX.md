# nginx 配置要点

## 1. 真实链路

```
客户端 → CF(橙云, 100s idle)
       → 跳板(.23 / .55 CF DNS 轮询)
       → new-api-horizon:30002
       → cx.wyzai.top
       → codex2api:8122
       → OpenAI
```

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

**关键配置（在 `proxy_pass http://127.0.0.1:8122;` 之后）**：

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
1. **Cloudflare 100s idle 超时**（橙云硬限）：需要 CF DNS 灰云，或靠 `codex2api` 的 SSE/JSON keepalive ticker（v1.7.45+ 已发 10s 间隔空 chunk）
2. **new-api-horizon 渠道 timeout**：K8s Pod 默认渠道超时，管理后台把 cx.wyzai.top 渠道 timeout 调到 300s+
3. **代码层**：v1.7.45 已发 `X-Accel-Buffering: no` 和 keepalive，不必再改

## 6. 验证 command

```bash
# 跳板
curl https://shell.wyzai.top/v1/models   # 401（缺 key 正常），首字节 0.21-0.30s

# Node2
curl -s https://cx.wyzai.top/health
```
