# codex2api 线上部署手册（codex-node + node2 双机架构）

> 真值以 `docker ps` 为准，不信文档。文档滞后时以线上状态为准并反向更新本文。
>
> **2026-05-15 起架构变更**：codex2api 应用层迁到独立 VPS `codex-node` (`152.53.210.32`)，PG/nginx 留 `node2` (`152.53.240.159`)，两机走 **WireGuard** 互连。整体见 `ARCHITECTURE.md`。

## 1. 线上状态（截至 2026-05-19 10:30）

| 项 | 值 |
|---|---|
| 镜像 | `codex2api:v1.7.57-rate-limit-probe`（`latest`）|
| 容器 | `codex2api` （在 **codex-node** 上）|
| 容器内端口 | `8123`（`CODEX_PORT=8123`，固定不变） |
| 宿主端口 | `10.10.0.2:8106:8123`（**仅监听 WG IP**，公网不可达） |
| nginx upstream | `proxy_pass http://10.10.0.2:8106;` （在 **node2** 上）|
| 部署目录 | `/data/codex2api/`（在 **codex-node** 上） |
| Admin | https://cx.wyzai.top/admin/　secret = `65187777` |
| 数据库 | PG `codex2api-postgres`（在 **node2**, 通过 socat 暴露 `10.10.0.1:5432`）|
| Redis | `codex2api-redis`（在 **codex-node** 本地，cache-only, `--memory=256m`）|
| 网络 | `codex2api-net`（在 **codex-node** 上；docker run 时不要加 compose 前缀）|
| 图像卷 | `/data/codex2api-images:/app/images`（在 **codex-node** 上，独立持久卷）|
| nginx conf | `/www/server/panel/vhost/nginx/cx.wyzai.top.conf`（**node2**）|
| nginx body 上限 | `client_max_body_size 500m` |
| 容器 body 上限 | **64 MB**（`CODEX_MAX_REQUEST_BODY_SIZE_MB=64`）|
| Dialog 采集 | ❌ 默认关闭（v1.7.55+ 起 `DIALOG_COLLECTION_ENABLED` 默认 false，需显式 opt-in）|
| Prefer-paid 调度 | Admin 面板运行时开关（默认 OFF = prefer_free）|
| 图床 `img.niji.edu.rs` | DNS→node2; node2 nginx `try_files → fallback @codex_node` 反代到 `10.10.0.2:8888` （`codex-images-nginx` 容器）|

## 2. SSH 接入

```bash
# codex-node（应用层，做 docker build / run / restart）
sshpass -p 'fmAJ8zmcAKL0ER8' ssh -p 22 -o StrictHostKeyChecking=no root@152.53.210.32

# node2（nginx + PG，做切流量 / 看 PG 日志）
sshpass -p 'f3t7uCBeTCizT12' ssh -p 22222 -o StrictHostKeyChecking=no root@152.53.240.159

# 跳板（shell.wyzai.top 的 CF DNS 轮询：两条 A 记录）
sshpass -p 'vCbeY8FXcSGw'   ssh -p 18604 root@156.238.226.23
sshpass -p '2R18UapfDNoT'   ssh -p 24598 root@156.238.226.55
```

## 3. 蓝绿 SOP（端口轮换）

> 容器内 `CODEX_PORT=8123` **始终不变**。蓝绿换的是 codex-node 上对外暴露的 WG 端口（`-p 10.10.0.2:NEW:8123`）。
>
> 当前主端口 8106；下次蓝绿候选 `8107 / 8108 / 8109`（在 codex-node 上 +1 轮换，便于 nginx 平滑切换）。

**🚨 不要把多步串成一行 bash——stdout buffer 会让你误以为卡死。每步独立跑。**

```bash
SSH_CODEX='sshpass -p fmAJ8zmcAKL0ER8 ssh -p 22 -o StrictHostKeyChecking=no root@152.53.210.32'
SSH_NODE2='sshpass -p f3t7uCBeTCizT12 ssh -p 22222 -o StrictHostKeyChecking=no root@152.53.240.159'
SCP_CODEX='sshpass -p fmAJ8zmcAKL0ER8 scp -P 22 -o StrictHostKeyChecking=no'

OLD=8106; NEW=8107; TAG=v1.7.58-xxx
```

> **起容器前先验空（在 codex-node 上）**：`ss -tlnp | grep ":810[4-9]"` 确认目标端口无人监听

### Step 1 · 本地 tar 打包（5-10s）

```bash
rm -f /tmp/codex2api-src.tar.gz
time tar --exclude='.git' --exclude='node_modules' --exclude='frontend/dist' \
    --exclude='data' --exclude='codex2api.db*' --exclude='logs' --exclude='*.log' \
    --exclude='.DS_Store' \
    -czf /tmp/codex2api-src.tar.gz -C /Users/yinghua/Documents/fly/codex2api .
```

### Step 2 · scp 上传到 codex-node（30s 内）

```bash
time $SCP_CODEX /tmp/codex2api-src.tar.gz root@152.53.210.32:/tmp/
```

### Step 3 · 解压 + 继承旧 .env

```bash
$SSH_CODEX "rm -rf /data/codex2api-new && mkdir -p /data/codex2api-new && \
      tar -xzf /tmp/codex2api-src.tar.gz -C /data/codex2api-new 2>&1 | grep -v 'Ignoring unknown extended header' | head -3 && \
      cp /data/codex2api/.env /data/codex2api-new/.env && echo OK"
```

### Step 4 · docker build（在 codex-node 上跑；增量 20s，全量 2 min）

```bash
$SSH_CODEX "cd /data/codex2api-new && time docker build --build-arg BUILD_VERSION=$TAG -t codex2api:$TAG . 2>&1 | tail -10"
```

### Step 5 · 启 codex2api-new

> ⚠️ **`--memory=6g --memory-swap=6g` 必须保留**：2026-05-15 事故后多轮实测调定。冷启动仅探针 ~2.4 GB；**热稳态（生产流量灌入后）RSS 3.4-4.2 GB 波动**（token cache + clientPool + 2360 账号探针 + in-flight SSE buffers + 大请求突发），4g 会在产流下触顶 OOMKilled。6g 留 ~2GB 突发余量。详见 `KUBESPHERE_NODE2_SETUP.md` 故障记录。
>
> ⚠️ **`-p 10.10.0.2:NEW:8123` 必须监听 WG IP**：不要写成 `-p NEW:8123`（会暴露公网），不要写成 `-p 127.0.0.1:NEW:8123`（node2 nginx 反代不到）。

```bash
$SSH_CODEX "docker rm -f codex2api-new 2>/dev/null || true; \
      docker run -d --name codex2api-new \
        --network codex2api-net \
        -p 10.10.0.2:$NEW:8123 \
        --env-file /data/codex2api-new/.env \
        -e CODEX_PORT=8123 \
        --memory=6g --memory-swap=6g \
        -v /data/codex2api-new/logs:/app/logs \
        -v /data/codex2api-images:/app/images \
        --restart unless-stopped \
        codex2api:$TAG && \
      sleep 8 && curl -s http://10.10.0.2:$NEW/health && \
      docker exec codex2api-new strings /usr/local/bin/codex2api | grep -oE 'v1\\.7\\.[0-9]+[a-z0-9-]*' | head -2"
```

### Step 6 · node2 nginx 切流量

```bash
$SSH_NODE2 "cp /www/server/panel/vhost/nginx/cx.wyzai.top.conf{,.bak-\$(date +%Y%m%d-%H%M%S)} && \
      sed -i 's|proxy_pass http://10.10.0.2:$OLD;|proxy_pass http://10.10.0.2:$NEW;|g' \
          /www/server/panel/vhost/nginx/cx.wyzai.top.conf && \
      nginx -t && nginx -s reload"
curl -s https://cx.wyzai.top/health
```

### Step 7-8 · 老容器 graceful 退出 + 固化

`docker stop -t 120` 必须：默认 10s 不够 `main.go` 的 90s shutdownTimeout。

```bash
$SSH_CODEX "sleep 30 && time docker stop -t 120 codex2api && \
      docker logs codex2api --tail 30 2>&1 | grep -E '收到关闭信号|HTTP 存量请求|已关闭' ; \
      docker rm codex2api && docker rename codex2api-new codex2api && \
      docker tag codex2api:$TAG codex2api:latest && \
      mv /data/codex2api /data/codex2api-old-\$(date +%Y%m%d-%H%M%S) && \
      mv /data/codex2api-new /data/codex2api && \
      docker ps --filter name=codex2api --format '{{.Names}} {{.Image}} {{.Status}}'"
```

> 注：`CODEX_PORT` 不需要 sed 改，固定为 8123。

## 4. 关键提醒

- **`--memory=6g --memory-swap=6g` 必须**：兜底内存越限。热稳态 RSS 3.4-4.2GB（6GB 留 ~2GB 突发余量），小于 5GB 会在生产流量下触顶 OOMKilled
- **`-p 10.10.0.2:NEW:8123` 必须绑 WG IP**：避免公网暴露，且 node2 nginx 反代要打到 WG IP 才通
- **每次必传 `--build-arg BUILD_VERSION`**：否则前端徽章显示 `dev`
- **`docker stop -t 120` 必须**：默认 10s 会强 kill 长 SSE 连接
- **图像卷必须 `/data/codex2api-images`**（在 codex-node 上）：避免蓝绿 mv 时连带迁走
- **每次必跑 `docker tag latest`**
- **取版本登 codex-node `docker ps`，不信 doc**
- **PG 连接走 WG**：`.env` 里 PG host 为 `10.10.0.1`（不是 localhost）

## 5. 应急回滚

### A. 同 codex-node 上回滚到上个版本（codex-node 内蓝绿）

```bash
$SSH_CODEX "docker run -d --name codex2api-rollback \
  --network codex2api-net \
  -p 10.10.0.2:<OLD_PORT>:8123 \
  --env-file /data/codex2api-old-YYYYMMDD-HHMMSS/.env \
  -e CODEX_PORT=8123 \
  --memory=6g --memory-swap=6g \
  -v /data/codex2api-images:/app/images \
  -v /data/codex2api-old-YYYYMMDD-HHMMSS/logs:/app/logs \
  --restart unless-stopped \
  codex2api:<OLD_TAG>"
$SSH_NODE2 "sed -i 's|10.10.0.2:<NEW_PORT>|10.10.0.2:<OLD_PORT>|' /www/server/panel/vhost/nginx/cx.wyzai.top.conf && nginx -s reload"
```

### B. 紧急回滚到 node2 旧容器（如 codex-node 整机故障）

> node2 上的旧 `codex2api` 容器目前是 `Exited (137)` 状态保留作秒级回滚点。

```bash
$SSH_NODE2 "docker start codex2api && \
            sed -i 's|10.10.0.2:8104|127.0.0.1:8123|' /www/server/panel/vhost/nginx/cx.wyzai.top.conf && \
            nginx -s reload && \
            curl -s https://cx.wyzai.top/health"
```

3 秒内业务恢复。WireGuard 故障也走这条路。

## 6. 常用运维

```bash
# 健康（codex-node 容器内 + 域名）
$SSH_CODEX 'curl -s http://10.10.0.2:8104/health'
curl -s https://cx.wyzai.top/health
$SSH_CODEX 'docker logs codex2api --tail 200 2>&1 | grep -vE "历史数据修复"'

# 实时跟日志（plan_type 同步、retry 触发等）
$SSH_CODEX 'docker logs codex2api -f 2>&1 | grep "同步上游 plan_type"'

# WG 隧道状态（在 codex-node 或 node2 上跑均可）
$SSH_CODEX 'wg show wg0 latest-handshakes transfer'
$SSH_NODE2 'ping -c 3 10.10.0.2'   # node2 → codex-node

# AT-only active 账号 + 最早到期（PG 在 node2，从任一机走 socat 都行）
$SSH_NODE2 'docker exec codex2api-postgres psql -U codex2api -d codex2api -c \
  "SELECT COUNT(*), MIN(NULLIF(credentials->>\"expires_at\",\"\")::timestamptz) earliest \
   FROM accounts WHERE COALESCE(credentials->>\"refresh_token\",\"\")=\"\" AND status=\"active\""'

# plan 分布
$SSH_NODE2 'docker exec codex2api-postgres psql -U codex2api -d codex2api -c \
  "SELECT COALESCE(NULLIF(credentials->>\"plan_type\",\"\"),\"(empty)\") plan, \
          COUNT(*) total, COUNT(*) FILTER (WHERE status=\"active\") active \
   FROM accounts GROUP BY 1 ORDER BY total DESC"'

# socat PG 桥接状态（node2）
$SSH_NODE2 'docker ps --filter name=socat-pg-bridge --format "{{.Names}} {{.Status}} {{.Ports}}"'
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
| v1.7.48-upstream-may10-paid | 8123 | 2026-05-10 | upstream port（流式 usage / 5h 紧迫性 / 工具参数剥离 / validation 扩充）+ 合并回 v1.7.47 的 prefer-paid & dialog logs + 413 提到 64MB |
| v1.7.49-tool-calls-fix | 8121 | 2026-05-13 | 修复非流式 ChatCompletions tool_calls 丢失：从 `response.output_item.done` 收 function_call（上游不一定在 `completed.output[]` 回填），stream=true 不受影响 |
| v1.7.50-partial-images | 8122 | 2026-05-13 | 修复 `metadata.image_partial_images` 不生效：Translator 在两条路径上都把 `response.image_generation_call.partial_image` 事件静默丢弃，现在渐进预览帧会输出为单独的 markdown image content chunk（N partial + 1 final）|
| v1.7.51-overload-retry | 8123 | 2026-05-13 | 扩充 capacityErrorMarkers 覆盖真实上游 transient 错误文案（overloaded / try again later / processing error），透明重试不再遗漏 |
| v1.7.52-error-visibility | 8121 | 2026-05-13 | usage_logs 加 4 列（upstream_error_kind/msg + retry_count + final_outcome）、GetUsageStats 增 3 个可视化字段、`/usage/error-breakdown` 新 endpoint、Usage 页 UI 加卡片 + 上游错误列（kind badge + retry pill）|
| v1.7.53-retry-reason | 8122 | 2026-05-13 | 重试拼救成功的 usage_log 行也携带首次重试原因（`firstRetryReasonMsg` 跨 attempt sticky，覆盖 capacity / stream-break 两种 continue 路径）|
| v1.7.53.1-retry-reason-fix | 8123 | 2026-05-13 | 补 transport-error （dial fail / proxy fail 等）continue 点同样 capture firstRetryReason，现在所有重试拼救行都能看到原因 |
| v1.7.53.2-proxy-auth | 8121 | 2026-05-13 | 发现上个版本暴露出大量代理 407 Proxy Authentication 错误被错误归类为 `auth` kind。拆出独立 `proxy_auth` kind（优先级高于 auth，避免 "Proxy Authentication" 被 "authentication" 同化）+ UI 橙色 badge 区分 |
| **v1.7.51-overload-retry** | **8123** | **2026-05-13** | **扩充 capacity 重试 marker：原有 `at capacity` / `try a different model` 只能识别 codex CLI 客户端渲染文案，未命中上游真实载荷。10min 实测样本：886 请求 / 149 `response.failed` = **17% 隐藏故障率**，全部漏出到客户端。新增 markers 覆盖 `Our servers are currently overloaded` (90%) / `An error occurred while processing your request` (10%) / `try again later`，透明重试机制现在能实际拦住这些错误** |
| **v1.7.55-image-gen-mem-fix** | **8123** | **2026-05-15** | **修复 5/15 内存事故根因 + 固化 cgroup**：(a) `IsImageGenDialogEvent` 过滤 4 处 SSE 采集点的 image_generation 事件（partial_image / output_item.done@image_generation_call 等），单 image 请求 dialog 占用 8MB→~50KB；(b) `DIALOG_COLLECTION_ENABLED` 默认从 true 改为 false（明确 opt-in）；(c) 前端 `formatResetAt` 过期 reset_7d_at 不再隐藏，改为灰色斜体 + tooltip "数据陈旧"；(d) cgroup 多轮调定后定为 `--memory=6g`（热稳态 RSS 3.4-4.2GB，原 2g 启动期OOMKilled / 4g 生产流量后可能触顶）固化进 `ops/DEPLOYMENT.md` Step 5 + 应急回滚 |
| **v1.7.55-migrated** | **10.10.0.2:8104** | **2026-05-15 19:30** | **架构迁移**：同镜像 v1.7.55-image-gen-mem-fix 从 node2 docker save → scp WG → codex-node docker load，20 分钟零中断完成。应用层迁 codex-node，原 node2 容器保留 `Exited (137)` 作秒级回滚。node2 used 从 30GB 降到 21GB。详见 `ARCHITECTURE.md` |
| **v1.7.56-metadata-alias** | **10.10.0.2:8105** | **2026-05-19 04:00** | **OpenAI 原生 metadata 键名兼容**：demo 客户传 `metadata: {size, quality}`（无前缀）被透传上游 → `Unsupported parameter: metadata`。修复：translator 加 `imageMetadataAliases` normalize `size→image_size` 等，+ 兜底删 metadata 防未识别字段透传。同时从容器导出重建丢失的 `/data/codex2api/.env`（上次蓝绿后丢了，本次补上）。[2 原子 commit] |
| **v1.7.57-rate-limit-probe** | **10.10.0.2:8106** | **2026-05-19 10:30** | **修复 rate_limited cooldown 死锁**：账号被一次 429 后设 `cooldown_until=reset_7d_at`（最长 7d），但 7d 滑动窗口推进后实际用量可能已降到 <110%。原逻辑 `NeedsUsageProbe` 在 rate_limited cooldown 全程禁探针，导致 ClearCooldown 永远调不到 → 632 个账号被锁死。修复加 4h escape hatch：距离 `LastRateLimitedAt > 4h` 时放行试探。一次性脚本差量复活 240 个误锁账号（46/596 是真 429 保留）。[3 文件变更] |

## 8. 不要再做的事 / 踩过的坑

- ❌ **基于 cooldown 时长推断 plan**：v1.7.29 之前误伤过 plus 被 7d ban 的合法账号
- ❌ **id_token 解析回滚 plan_type**：用户拒绝，已废弃
- ❌ **多步骤 bash 命令串成一行**：stdout buffer 误判卡死
- ❌ **全局改 `/` 的 buffering**：会关掉 new-api UI 静态资源缓冲
- ❌ **删除跳板 `sub_filter`**：前端暗色主题依赖它
- ❌ **拿客户端看到的错误文案去定 fix（v1.7.51 教训）**：codex CLI 会把任何 `response.failed` 事件都渲染成 `Selected model is at capacity. Please try a different model.` 这句兜底文案。要验证上游真实错误文案，查 `dialog_logs.response_body → 序列化 type=“response.failed”.response.error.message`（例如 `Our servers are currently overloaded`）
- ❌ **PG 78GB+ 全表 `::text ILIKE` 扫描**：TOAST jsonb 全量进内存会超时。必须加 `WHERE ts >= now() - interval 'N minutes'` 限定时间窗口
- ✅ **plan_type 校正的正道**：让 OpenAI 的 429 `error.plan_type` 自动同步（v1.7.29 已实现）
- ✅ **`isCapacityError` 不要名不副实重命名**：语义已泛化为"上游瞬时可重试错误"，但函数名保留可减少 4 处调用点的变动及测试破坏面

## 9. 数据规模快照（2026-05-13 实测）

| 项 | 值 | 说明 |
|---|---|---|
| `dialog_logs` 表总体积 | **78 GB** | 6.86 天累积，主表 27 MB + TOAST 77 GB + 索引 9 MB |
| `dialog_logs` 行数 | 12.86 万 | 平均 636 KB/行 |
| 月增量预估 | **~341 GB** | 同速率下 30 天 |
| `request_body` 含 base64 image | 23% 行 / **73% 体积** | 平均 1.7 MB/行（未含图的仅 178 KB）|
| `/data/codex2api-images` | 4.6 GB / 2715 张 | 20.83 天累积，hash 去重后增长稳定 |
| 月增图片 | ~6.6 GB | v1.7.50 后partial 帧会提高 ~1.5-2x |

> 优化机会：写入前剥离 `request_body` 中的 data URL base64 可砍 **~60% 表体积**
> （月省 ~210 GB）。代码 ~30 行 + 1 个测试。

## 10. 常量速查

| 常量 | 值 | 位置 |
|---|---|---|
| shutdownTimeout | 90s | `main.go` |
| freeUsageRateLimitThresholdPct | 90.0 | `auth/store.go` |
| team score_bias | +100 | v1.7.28+ |
| plus/pro score_bias | +50 | - |
| AT-only 过期窗口 | 5 min | - |
| MaxRequestBodySize 默认 | 32 MB | `security/validator.go` |
| CF idle timeout | 100s | Cloudflare 硬限 |

## 11. 关键环境变量

```bash
# /data/codex2api/.env（在 codex-node 上）
CODEX_PORT=8123                         # 容器内固定，外部端口由 docker -p 决定，不随蓝绿轮换
# 上游错误可见性字段（v1.7.52+）：usage_logs.upstream_error_kind/msg, retry_count, final_outcome
# kind 取值：overloaded/capacity/processing_error/rate_limit/auth/proxy_auth/context_length/content_filter/timeout/client_disconnect/upstream_5xx/upstream_4xx/unknown
CODEX_MAX_REQUEST_BODY_SIZE_MB=64       # 请求体上限，默认 32MB；v1.7.48 起调到 64MB 解决 413
DIALOG_COLLECTION_ENABLED=false         # v1.7.55+ 默认 false；需采集时显式 opt-in
CODEX_TRANSPORT_MODE=standard           # TLS 指纹：standard=Go 原生 / utls_chrome=仿 Chrome
# PG 走 WG 到 node2
DB_HOST=10.10.0.1                       # node2 上的 socat 桥接
DB_PORT=5432
# Redis 本机
REDIS_HOST=codex2api-redis              # docker network 内部服务名
REDIS_PORT=6379
# 其余参数（admin/image/proxy 等）见 `.env` 本体
```

## 12. 图床部署（`img.niji.edu.rs`）

> 2026-05-15 迁移后补上反代。老图留 node2，新图在 codex-node `/data/codex2api-images`，nginx `try_files → fallback` 两边都能读。完整链路见 `ARCHITECTURE.md` §7。

### 初次部署（已完成，仅记录供重建用）

**Step 1 · codex-node 起 `nginx:alpine` 容器**：

```bash
$SSH_CODEX "cat > /tmp/codex-images-nginx.conf <<'EOF'
server {
    listen 80;
    server_name _;
    root /usr/share/nginx/html;
    location ~* \.(png|jpe?g|webp)\$ {
        add_header Cache-Control \"public, immutable, max-age=2592000\";
        access_log off;
        try_files \$uri =404;
    }
    location = /health { return 200 'ok'; access_log off; }
    location / { return 404; }
}
EOF
docker rm -f codex-images-nginx 2>/dev/null; \
docker run -d --name codex-images-nginx \
    -p 10.10.0.2:8888:80 \
    -v /data/codex2api-images:/usr/share/nginx/html:ro \
    -v /tmp/codex-images-nginx.conf:/etc/nginx/conf.d/default.conf:ro \
    --restart unless-stopped \
    nginx:alpine"
```

**Step 2 · node2 nginx 加 fallback 反代**：编辑 `/www/server/panel/vhost/nginx/img.niji.edu.rs.conf`，在 `location ~* \.(png|jpg|jpeg|webp)$` 里加 `try_files $uri @codex_node;`，并加 `location @codex_node { proxy_pass http://10.10.0.2:8888; ... }` 块。完整 conf 见 `NGINX.md` §8。

```bash
$SSH_NODE2 "nginx -t && nginx -s reload"
```

**Step 3 · 验证**：

```bash
# 老图（node2 本地命中）
curl -sI https://img.niji.edu.rs/00c1d53413569103.png | head -3
# 新图（必反代 codex-node）——找一张 5/15 19:30 后生成的 hash
$SSH_CODEX 'ls -t /data/codex2api-images/ | head -1'
curl -sI https://img.niji.edu.rs/<hash>.png | head -3
```

### 日常运维

```bash
# codex-images-nginx 容器状态
$SSH_CODEX 'docker ps --filter name=codex-images-nginx --format "{{.Names}} {{.Status}}"'

# 重启（一般不需，只读挂盘 + 静态块 stable）
$SSH_CODEX 'docker restart codex-images-nginx'

# 查看一下谁在调（走到 codex-node 的都是新图）
$SSH_CODEX 'docker logs codex-images-nginx --tail 50'

# 磁盘（应用在写不是这个容器在写）
$SSH_CODEX 'du -sh /data/codex2api-images && ls /data/codex2api-images | wc -l'
```

### 重要提醒

- **不必迁移老图**：node2 上 7.8GB 老图保留作本地 hot path，迁了只多一跳 RTT。
- **`codex-images-nginx` 只读挂盘**（`-v ...:ro`）：应用层写入走 codex2api 容器自己的 `/app/images` 挂载，不走这个容器。两个容器共享同一个 host 卷，互不冲突。
- **`-p 10.10.0.2:8888:80`**：同样仅绑 WG IP，不暴露公网。
- **不要在 codex2api 容器里加静态路由**：hot path、静态 + 动态混一起，重部署会冲不上。静态服务单容器跨生命周期、`docker restart` 也不会影响主 API。

### 应急

```bash
# codex-images-nginx 挂了：它重启即可，不会影响主 API
$SSH_CODEX 'docker restart codex-images-nginx && sleep 1 && curl -s http://10.10.0.2:8888/health'

# WG 隧道挂了：fallback proxy_pass 会 502，老图仍可访问（try_files 本地命中不需跳 WG）
wg show wg0 latest-handshakes

# 如果要临时从 codex-node 拉一份到 node2（应急独立运行）
$SSH_CODEX 'cd /data/codex2api-images && tar czf /tmp/imgs.tar.gz $(ls -t | head -100)'
# scp 拉回 node2 后解压到 /data/codex2api-images/
```

## 13. v1.7.48 合并变更要点（2026-05-10）

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
