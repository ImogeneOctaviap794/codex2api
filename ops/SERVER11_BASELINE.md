# SERVER11 Baseline（relay11 / cx11）

> 与 server10（159.195.14.153）同款 Netcup 裸机，架构复刻：一套 codex2api + 一套 new-api（单实例）。
> 开荒日期：2026-06-15。

## 1. 服务器规格

- Provider：Netcup，IP `159.195.15.199`
- OS：Debian 13 trixie，16 vCPU / 62Gi RAM / 2.0T disk
- 主网卡：`eth0`
- 流量策略：Netcup 3TB/24h **滚动双向**（rx+tx 相加，同 server10）
- hostname：`server11-megasrv-de`

## 2. 访问入口

| 服务 | 地址 | 凭证 |
|---|---|---|
| SSH | `root@159.195.15.199` 端口 22 / 22222 | key `~/.ssh/azure_vpn`（azure-vpn-20260517） |
| 宝塔面板 | `http://159.195.15.199:14549/caaf6bec` | `q7fhhrnx` / `b47347db` |
| new-api | `https://relay11.wyzai.top` | 待初始化（首注册即 root） |
| codex2api | `https://cx11.wyzai.top` | admin secret `d04d155f3393b05e562f7e8b`（header `X-Admin-Key`） |

## 3. 系统基线（phase1）

- Docker 29.5.3（官方源）+ Compose v5.1.4，daemon.json：live-restore / nofile 1048576
- sysctl：BBR + fq、somaxconn 65535、tcp_tw_reuse、file-max 4194304
- limits：nofile 1048576 / nproc 65535
- UFW（deny incoming）：放行 22 / 22222 / 80 / 443 / 14549
- fail2ban sshd jail，swap 8G，THP 禁用，journald 持久化
- 脚本：`scripts/bootstrap_server11_megasrv_159_195_15_199_phase1.sh`（由 server10 phase1 改 IP+hostname 生成）

## 4. codex2api 栈

- 目录：`/data/apps/codex2api`
- 容器：`codex2api-standalone`（app）+ `codex2api-standalone-postgres`（PG18-alpine）+ `codex2api-standalone-redis`（Redis7）
- 端口：`0.0.0.0:18123 -> 8123`
- 网络：`codex2api-standalone-net` + `newapi-net`（已接入，供 new-api 内网调用）
- 凭证（详见 `/data/apps/codex2api/.env`）：
  - `CODEX_API_KEYS=sk-codex-standalone-2249a3cab1092ab8ca8db4096c4f06c23fe9ecf05ec87dd7`
  - `ADMIN_SECRET=d04d155f3393b05e562f7e8b`
- 资源：APP 12g / PG 6g（max_connections 800，shared_buffers 4GB）/ Redis 2g
- system_settings：max_concurrency 1000、pg_max_conns 500、redis_pool 500、global_rpm 0
- 部署脚本：`ops/deploy_codex2api_standalone.sh`
- 源码：从 server10 `/data/apps/codex2api/app` 打包（含 200ms fastqueue 调度改动）
- `PUBLIC_BASE_URL=http://159.195.15.199:18123`（图片 URL；如需用域名改为 `https://cx11.wyzai.top` 后 restart）

## 5. new-api 栈（单实例）

- 目录：`/data/apps/new-api`
- 容器：`newapi-app`（calciumion/new-api:latest = v1.0.0-rc.11）+ `newapi-postgres`（PG15）+ `newapi-redis`（Redis7）
- 端口：`127.0.0.1:3000 -> 3000`
- 网络：`newapi-net`
- 凭证：随机生成于 `/data/apps/new-api/.env`（PG_PASSWORD / REDIS_PASSWORD / SESSION_SECRET / CRYPTO_SECRET）
- 资源：app 8g / PG 8g（max_connections 200）/ Redis 3g（maxmemory 2gb）
- 关键 env：MEMORY_CACHE_ENABLED、BATCH_UPDATE、SQL_MAX_OPEN_CONNS 100
- 部署脚本：`ops/deploy_newapi_standalone.sh`
- 与 server10 差异：**单实例**（无 slave-1/2，无 upstream cluster）

## 6. 网络打通

- `docker network connect newapi-net codex2api-standalone` 已执行
- new-api 容器内可解析 `codex2api-standalone`，验证：`wget http://codex2api-standalone:8123/health` → ok
- **new-api 渠道 Base URL 填 `http://codex2api-standalone:8123`**

## 7. DNS / SSL / nginx

- Cloudflare（wyzai.top，zone `a68e905ebe0de9e20bec9bbf462b1c8d`）：
  - `relay11.wyzai.top` A → 159.195.15.199 proxied=false
  - `cx11.wyzai.top` A → 159.195.15.199 proxied=false
- nginx：**系统 nginx**（apt 1.26.3，非宝塔；server11 宝塔未装 nginx 组件）
- vhost：`/etc/nginx/conf.d/relay11.wyzai.top.conf`、`/etc/nginx/conf.d/cx11.wyzai.top.conf`
  - 本地源：`ops/server11/*.conf`
  - `relay11` → 127.0.0.1:3000，`cx11` → 127.0.0.1:18123
  - `/v1/` SSE 优化：proxy_buffering off、timeout 86400s、chunked、Accept-Encoding identity
- 证书：acme.sh **dns_cf**（DNS-01）签 Let's Encrypt ECC，装到 `/etc/nginx/ssl/<domain>/`
  - 有效期到 2026-09-13，acme.sh cron 自动续签（ARI 窗口 ~2026-08）
  - CF token 存于 `/root/.acme.sh/account.conf`
- 部署脚本：`ops/server11/setup_ssl_nginx.sh`（`CF_Token` / `CF_Account_ID` 经 env 传入）

## 8. 待办（业务初始化，用户自行操作）

1. **new-api**：访问 `https://relay11.wyzai.top` → 首次注册（第一个用户即 root）→ 渠道页加 codex2api 渠道（见下）→ 令牌页建 token
2. **codex2api**：访问 `https://cx11.wyzai.top/admin`（`X-Admin-Key: d04d155f3393b05e562f7e8b`）→ 加 ChatGPT/Codex 账户（账户池当前为空）

### new-api 加 codex2api 渠道参数

- 类型：OpenAI
- 名称：任意（如 `codex2api-local`）
- Base URL：`http://codex2api-standalone:8123`（new-api 自动补 `/v1`；若失败可尝试带 `/v1`）
- 密钥：`sk-codex-standalone-2249a3cab1092ab8ca8db4096c4f06c23fe9ecf05ec87dd7`
- 模型：`gpt-5.5,gpt-5.5-pro,gpt-5.4,gpt-5.4-pro,gpt-5.4-mini,gpt-5,gpt-5-codex,gpt-5-codex-mini,gpt-5.1,gpt-5.1-codex,gpt-5.1-codex-mini,gpt-5.1-codex-max,gpt-5.2,gpt-5.2-codex,gpt-5.3-codex`

## 9. 常用运维

```bash
# 重启
docker compose -f /data/apps/codex2api/docker-compose.yml restart   # codex2api
docker compose -f /data/apps/new-api/docker-compose.yml restart      # new-api

# 日志
docker logs -f --tail 200 codex2api-standalone
docker logs -f --tail 200 newapi-app

# 健康
curl -s http://127.0.0.1:18123/health        # codex2api
curl -s http://127.0.0.1:3000/api/status      # new-api

# 证书手动续签
/root/.acme.sh/acme.sh --renew -d relay11.wyzai.top --ecc
nginx -t && systemctl reload nginx

# 流量（Netcup 3TB/24h 双向）
cat /proc/net/dev | grep eth0
```

## 10. 待加固（可选）

- SSH phase2：改默认端口 22222、禁密码登录（当前保留 22+密码，未锁）。
  - server10 已做；server11 暂缓，避免开荒中途风险。

## 11. 变更记录

### 2026-06-25 codex2api 移除内置 `gpt-5.4 → gpt-5.5` 别名（蓝绿热更新）

- **背景**：用户反馈在 `https://cx11.wyzai.top` 后台没有任何虚拟模型别名配置，但 `gpt-5.4` 请求仍被定向到上游 `gpt-5.5`。
- **根因**：`proxy/model_overrides.go::BuiltInModelOverrides()` 中硬编码了 `gpt-5.4 → BaseModel=gpt-5.5, ResponseAlias=gpt-5.4` 别名，对所有部署默认生效。
- **修复**：删除该内置条目，保留 `gpt-5.5-fast` / `gpt-5.4-fast` 两条 fast 别名。现在 `gpt-5.4` 默认透传上游，需要在 admin → Settings → 虚拟模型别名（Payload 注入）显式配置 `model_payload_overrides` 才会重定向。前端 `DEFAULT_54_TO_55_ALIAS_OVERRIDES` 预设按钮保留作为模板。
- **部署方式**：蓝绿热更新，公网零中断。
  1. 仅上传 `proxy/model_overrides.go`、`proxy/model_overrides_test.go` 到 `/data/apps/codex2api/app/proxy/`，原文件备份为 `*.bak-20260625-1438`。
  2. server11 构建镜像 `codex2api-standalone:no-builtin-gpt54-alias-20260625-1438`，同步 tag 为 `latest`。
  3. 启动候选容器 `codex2api-standalone-new`，host 端口 `127.0.0.1:18124`，复用 `.env` / `logs` / `images` 卷与 `codex2api-standalone-net` 网络。
  4. 修改 `/etc/nginx/conf.d/cx11.wyzai.top.conf` 把 `proxy_pass http://127.0.0.1:18123` 改成 `:18124`，`nginx -t && systemctl reload nginx`，公网 `/health` 验证。
  5. `docker stop -t 120 codex2api-standalone`，`docker compose up -d codex2api` 用新 latest 镜像重建正式实例 `:18123`。
  6. nginx 切回 `:18123`，删除候选容器 `codex2api-standalone-new`。
- **验证**：本地 `go test ./proxy` 全过；公网 `curl -fsS https://cx11.wyzai.top/health` 返回 `status=ok, available=4, total=6`；`docker ps` 仅剩 `codex2api-standalone` 监听 `0.0.0.0:18123`。
