# codex2api 线上部署手册（Node2 · cx.wyzai.top）

> 真值以 `docker ps` 为准，不信文档。文档滞后时以线上状态为准并反向更新本文。

## 1. 线上状态（截至 2026-05-10 22:40）

| 项 | 值 |
|---|---|
| 镜像 | `codex2api:v1.7.48-upstream-may10-paid`（`latest`）|
| 容器 | `codex2api` |
| 端口 | `8123`（nginx upstream 指向 127.0.0.1:8123）|
| 部署目录 | `/data/codex2api/` |
| Admin | https://cx.wyzai.top/admin/　secret = `65187777` |
| 数据库 | PG `codex2api-postgres`, Redis `codex2api-redis`, 网络 `codex2api_codex2api-net` |
| 图像卷 | `/data/codex2api-images:/app/images`（独立持久卷，蓝绿不会重建）|
| nginx conf | `/www/server/panel/vhost/nginx/cx.wyzai.top.conf` |
| nginx body 上限 | `client_max_body_size 500m` |
| 容器 body 上限 | **64 MB**（`CODEX_MAX_REQUEST_BODY_SIZE_MB=64`）|
| Dialog 采集 | ✅ 启用（默认 true，写入 PG `dialog_logs`）|
| Prefer-paid 调度 | Admin 面板运行时开关（默认 OFF = prefer_free）|
| 本地 git HEAD | `9fcd50b`（`main` / `merge/upstream-2026-05-10`）|

## 2. SSH 接入

```bash
# Node2（cx.wyzai.top 落点）
sshpass -p 'f3t7uCBeTCizT12' ssh -p 22222 -o StrictHostKeyChecking=no root@152.53.240.159

# 跳板（shell.wyzai.top 的 CF DNS 轮询：两条 A 记录）
sshpass -p 'vCbeY8FXcSGw'   ssh -p 18604 root@156.238.226.23
sshpass -p '2R18UapfDNoT'   ssh -p 24598 root@156.238.226.55
```

## 3. 蓝绿 SOP（端口轮换）

端口：`8120 → 8121 → 8122 → 8123 → 8120 …`

**🚨 不要把多步串成一行 bash——stdout buffer 会让你误以为卡死。每步独立跑。**

```bash
SSH='sshpass -p f3t7uCBeTCizT12 ssh -p 22222 -o StrictHostKeyChecking=no root@152.53.240.159'
SCP='sshpass -p f3t7uCBeTCizT12 scp -P 22222 -o StrictHostKeyChecking=no'
OLD=8123; NEW=8120; TAG=v1.7.49-xxx
```

> **下次端口**：8123 → 8120（轮换到最老的回收端口即可）

### Step 1 · 本地 tar 打包（5-10s）

```bash
rm -f /tmp/codex2api-src.tar.gz
time tar --exclude='.git' --exclude='node_modules' --exclude='frontend/dist' \
    --exclude='data' --exclude='codex2api.db*' --exclude='logs' --exclude='*.log' \
    --exclude='.DS_Store' \
    -czf /tmp/codex2api-src.tar.gz -C /Users/yinghua/Documents/fly/codex2api .
```

### Step 2 · scp 上传（30s 内）

```bash
time $SCP /tmp/codex2api-src.tar.gz root@152.53.240.159:/tmp/
```

### Step 3 · 解压 + 继承旧 .env

```bash
$SSH "rm -rf /data/codex2api-new && mkdir -p /data/codex2api-new && \
      tar -xzf /tmp/codex2api-src.tar.gz -C /data/codex2api-new 2>&1 | grep -v 'Ignoring unknown extended header' | head -3 && \
      cp /data/codex2api/.env /data/codex2api-new/.env && echo OK"
```

### Step 4 · docker build（独立跑！增量 20s，全量 2 min）

```bash
$SSH "cd /data/codex2api-new && time docker build --build-arg BUILD_VERSION=$TAG -t codex2api:$TAG . 2>&1 | tail -10"
```

### Step 5 · 启 codex2api-new

```bash
$SSH "docker rm -f codex2api-new 2>/dev/null || true; \
      docker run -d --name codex2api-new \
        --network codex2api_codex2api-net \
        -p 127.0.0.1:$NEW:$NEW \
        --env-file /data/codex2api-new/.env \
        -e CODEX_PORT=$NEW \
        -v /data/codex2api-new/logs:/app/logs \
        -v /data/codex2api-images:/app/images \
        --restart unless-stopped \
        codex2api:$TAG && \
      sleep 8 && curl -s http://127.0.0.1:$NEW/health && \
      docker exec codex2api-new strings /usr/local/bin/codex2api | grep -oE 'v1\\.7\\.[0-9]+[a-z0-9-]*' | head -2"
```

### Step 6 · nginx 切流量

```bash
$SSH "cp /www/server/panel/vhost/nginx/cx.wyzai.top.conf{,.bak-\$(date +%Y%m%d-%H%M%S)} && \
      sed -i 's|proxy_pass http://127.0.0.1:$OLD;|proxy_pass http://127.0.0.1:$NEW;|g' \
          /www/server/panel/vhost/nginx/cx.wyzai.top.conf && \
      nginx -t && nginx -s reload"
curl -s https://cx.wyzai.top/health
```

### Step 7-8 · 老容器 graceful 退出 + 固化

`docker stop -t 120` 必须：默认 10s 不够 `main.go` 的 90s shutdownTimeout。

```bash
$SSH "sleep 30 && time docker stop -t 120 codex2api && \
      docker logs codex2api --tail 30 2>&1 | grep -E '收到关闭信号|HTTP 存量请求|已关闭' ; \
      docker rm codex2api && docker rename codex2api-new codex2api && \
      docker tag codex2api:$TAG codex2api:latest && \
      mv /data/codex2api /data/codex2api-old-\$(date +%Y%m%d-%H%M%S) && \
      mv /data/codex2api-new /data/codex2api && \
      sed -i 's/^CODEX_PORT=.*/CODEX_PORT=$NEW/' /data/codex2api/.env && \
      docker ps --filter name=codex2api --format '{{.Names}} {{.Image}} {{.Status}}'"
```

## 4. 关键提醒

- **每次必传 `--build-arg BUILD_VERSION`**：否则前端徽章显示 `dev`
- **`docker stop -t 120` 必须**：默认 10s 会强 kill 长 SSE 连接
- **图像卷必须 `/data/codex2api-images`**：避免蓝绿 mv 时连带迁走
- **每次必跑 `docker tag latest`**
- **取版本登 `docker ps`，不信 doc**

## 5. 应急回滚

```bash
$SSH "docker run -d --name codex2api-rollback \
  --network codex2api_codex2api-net \
  -p 127.0.0.1:<OLD_PORT>:<OLD_PORT> \
  --env-file /data/codex2api-old-YYYYMMDD-HHMMSS/.env \
  -v /data/codex2api-images:/app/images \
  -v /data/codex2api-old-YYYYMMDD-HHMMSS/logs:/app/logs \
  --restart unless-stopped \
  codex2api:<OLD_TAG>"
$SSH "sed -i 's|:<NEW_PORT>|:<OLD_PORT>|' /www/server/panel/vhost/nginx/cx.wyzai.top.conf && nginx -s reload"
```

## 6. 常用运维

```bash
# 健康
curl -s http://127.0.0.1:8122/health
docker logs codex2api --tail 200 2>&1 | grep -vE '历史数据修复'

# plan_type 实时同步日志（v1.7.29+）
docker logs codex2api -f 2>&1 | grep '同步上游 plan_type'

# AT-only active 账号 + 最早到期
docker exec codex2api-postgres psql -U codex2api -d codex2api -c \
  "SELECT COUNT(*), MIN(NULLIF(credentials->>'expires_at','')::timestamptz) earliest \
   FROM accounts WHERE COALESCE(credentials->>'refresh_token','')='' AND status='active'"

# plan 分布
docker exec codex2api-postgres psql -U codex2api -d codex2api -c \
  "SELECT COALESCE(NULLIF(credentials->>'plan_type',''),'(empty)') plan, \
          COUNT(*) total, COUNT(*) FILTER (WHERE status='active') active \
   FROM accounts GROUP BY 1 ORDER BY total DESC"
```

## 7. 版本史

| 版本 | 端口 | 日期 | 关键改动 |
|---|---|---|---|
| v1.7.39-batch-add-3000 | 8120 | - | 批量添加 3000 账号 |
| v1.7.40-ua-proxy | 8121 | - | UA 池 20→93 + 前端代理归一化 |
| v1.7.41-dedupe | 8122 | - | 邮箱去重 |
| v1.7.42-dual-fp | 8123 | - | 双轨 TLS + UA 双层保险 |
| v1.7.43-free-55 | 8120 | - | free 账号承接 gpt-5.5 运行时开关 |
| v1.7.44-unlock-banned | 8121 | - | 封禁 plus 账号自动解锁可清理 |
| v1.7.45-sse-keepalive | 8122 | 2026-05-04 | SSE/JSON 双路径 keepalive 防 CF 100s idle |
| v1.7.47-prefer-paid | 8125 | 2026-05-08 | 付费账号优先 Free 兜底（admin 开关）+ Dialog logs 异步采集（⚠️ 代码曾仅在服务器，已合并回本地 9fcd50b）|
| **v1.7.48-upstream-may10-paid** | **8123** | **2026-05-10** | **upstream port（流式 usage / 5h 紧迫性 / 工具参数剥离 / validation 扩充）+ 合并回 v1.7.47 的 prefer-paid & dialog logs + 413 提到 64MB** |

## 8. 不要再做的事

- ❌ **基于 cooldown 时长推断 plan**：v1.7.29 之前误伤过 plus 被 7d ban 的合法账号
- ❌ **id_token 解析回滚 plan_type**：用户拒绝，已废弃
- ❌ **多步骤 bash 命令串成一行**：stdout buffer 误判卡死
- ❌ **全局改 `/` 的 buffering**：会关掉 new-api UI 静态资源缓冲
- ❌ **删除跳板 `sub_filter`**：前端暗色主题依赖它
- ✅ **plan_type 校正的正道**：让 OpenAI 的 429 `error.plan_type` 自动同步（v1.7.29 已实现）

## 9. 常量速查

| 常量 | 值 | 位置 |
|---|---|---|
| shutdownTimeout | 90s | `main.go` |
| freeUsageRateLimitThresholdPct | 90.0 | `auth/store.go` |
| team score_bias | +100 | v1.7.28+ |
| plus/pro score_bias | +50 | - |
| AT-only 过期窗口 | 5 min | - |
| MaxRequestBodySize 默认 | 32 MB | `security/validator.go` |
| CF idle timeout | 100s | Cloudflare 硬限 |

## 10. 关键环境变量

```bash
# /data/codex2api/.env
CODEX_PORT=8123                         # 当前端口（随蓝绿轮换）
CODEX_MAX_REQUEST_BODY_SIZE_MB=64       # 请求体上限，默认 32MB；v1.7.48 起调到 64MB 解决 413
DIALOG_COLLECTION_ENABLED=true          # 对话采集总开关（默认 true，启动级）；false=不创建实例
CODEX_TRANSPORT_MODE=standard           # TLS 指纹：standard=Go 原生 / utls_chrome=仿 Chrome
# 其余参数（DB/Redis/image/admin 等）见 `.env` 本体
```

## 11. v1.7.48 合并变更要点（2026-05-10）

### 功能层
- **Prefer-paid 调度**（admin 运行时开关）：ON 时 plus/pro/team 优先派发，free 兜底；OFF（默认）时 prefer_free 最省额度
- **Dialog logs**：异步写入 PG `dialog_logs` 表，panic 隔离 + channel drop，不阻塞主链路
- **请求体上限 64MB**：解决 Codex CLI 长上下文 / 图片上传触发 413

### Upstream port（来自 james-6-23）
- `4694c54` 流式 usage 追踪：上游 ctx 解耦 + TTFT 黑名单 + function_call 去重
- `4a48799` premium 账号 5h 窗口紧迫性 bonus
- `4bd3c03` 流式工具参数空字段剥离
- `e0919ce`（partial）：validation 补 7 官方类型 + cache item id 剥离

### Admin 面板路径
- **Prefer-paid 开关**：Settings → 派发策略卡片（标签：付费账号优先 Free 兜底）
- **Dialog logs 开关**：Settings（运行时热开关，不影响启动级 env 开关）
- **Dialog 浏览页**：Sidebar → Dialogs（WIP 前端页，可查询近期对话）
