# 部署架构

> 适用于：2026-05-15 以后的双机分离架构。迁移背景与历史事故复盘见父目录 `KUBESPHERE_NODE2_SETUP.md` 的「2026-05-15 重大事故」章节。

## 1. 一句话

应用层在 **codex-node** 独立 VPS（8C/16GB），数据层 PG 留在 **node2**，两机走 **WireGuard** 加密内网（`10.10.0.0/24`）互连，公网入口只走 node2 nginx。

## 2. 数据流

```
client
  → cx.wyzai.top (CF DNS)
  → node2 nginx (152.53.240.159, SSL 终结 + WAF)
  → http://10.10.0.2:8104 (WireGuard 隧道, RTT ~0.6ms)
  → codex-node docker codex2api 容器 (内部 8123)
       ├─→ codex2api-redis (本地新建, cache-only)
       └─→ 10.10.0.1:5432 (回到 node2)
              → docker socat 桥接 → codex2api-postgres:5432
              → 持久化 codex2api_pgdata
```

**关键点**：

- 客户端**永远不直接连 codex-node 公网**。codex-node 上 codex2api 仅 `-p 10.10.0.2:8104:8123`，监听的是 WG IP。
- node2 上 codex2api 容器保留 `Exited (137)` 状态作为秒级回滚点（`docker start codex2api` 即可，配 nginx 切回 `127.0.0.1:8123`）。
- Redis 是 cache-only，丢了重建即可。**source of truth 是 node2 PG**。

## 3. 两机分工

| 项 | node2 (152.53.240.159) | codex-node (152.53.210.32) |
|---|---|---|
| 角色 | 数据层 + 网关 | 应用层 |
| 容器 | `codex2api-postgres`, `codex2api-redis`（旧）, `socat-pg-bridge`, K8s + 全部其他业务 | `codex2api` (v1.7.55+), `codex2api-redis`（新, cache-only）, `codex-images-nginx`（图床静态） |
| 公网入口 | nginx 80/443 (`cx.wyzai.top` 等所有域名) | 不暴露应用，仅 SSH/宝塔 |
| 内存 | 31 GB（迁移后 used 21 GB） | 16 GB DDR5 ECC |
| CPU | 12 核 EPYC Genoa | 8 核 EPYC Genoa |
| OS | Debian 13 | Debian 13 |
| 备注 | KubeSphere 已 2026-05-04 拆除 | bootstrap 脚本 `scripts/bootstrap_codex_node.sh`（在 node2） |

## 4. WireGuard 隧道

**网段**：`10.10.0.0/24`，`server=node2 (10.10.0.1, listen 51820/UDP)`，`client=codex-node (10.10.0.2, 主动出站 + 25s keepalive)`。

| 主机 | WG IP | 公钥 |
|---|---|---|
| node2 | `10.10.0.1` | `y0YYOvGB8cVw5UHIjROD9CK3I81U+y4aNEmHHmAz0mQ=` |
| codex-node | `10.10.0.2` | `I0V0aY6G3DaDBj9CuXRibWXQVkqmU/4JNmqZiZv4UT4=` |

配置文件均为 `/etc/wireguard/wg0.conf`，systemd 单元 `wg-quick@wg0`（开机自启）。私钥位于各自 `/etc/wireguard/privatekey`。

**验证**（在任一端执行）：
```bash
wg show wg0 latest-handshakes transfer
ping -c 3 10.10.0.1   # codex-node → node2
ping -c 3 10.10.0.2   # node2 → codex-node
```

## 5. SSH 速查

| 主机 | 命令 | 密码 |
|---|---|---|
| node2 | `ssh -p 22222 root@152.53.240.159` | `f3t7uCBeTCizT12` |
| codex-node | `ssh -p 22 root@152.53.210.32` | `fmAJ8zmcAKL0ER8` |

**bash 中 sshpass 写法**：
```bash
SSH_NODE2='sshpass -p f3t7uCBeTCizT12 ssh -p 22222 -o StrictHostKeyChecking=no root@152.53.240.159'
SSH_CODEX='sshpass -p fmAJ8zmcAKL0ER8 ssh -p 22 -o StrictHostKeyChecking=no root@152.53.210.32'
```

## 6. 端口约定

| 位置 | 端口 | 说明 |
|---|---|---|
| codex-node 容器内 `CODEX_PORT` | **8123** | 应用真实监听端口（容器内） |
| codex-node 宿主端口（仅 WG IP） | **8104** | `-p 10.10.0.2:8104:8123` 暴露给 node2 |
| node2 nginx upstream | `10.10.0.2:8104` | 通过 WG 反代 |
| node2 socat 桥接 | `10.10.0.1:5432 → codex2api-postgres:5432` | PG 走 WG 暴露给 codex-node |
| codex-node 图床 nginx | `10.10.0.2:8888` | `nginx:alpine` 容器，serve `/data/codex2api-images` |
| node2 旧 codex2api（停） | `127.0.0.1:8123` | 回滚保留 |

**`CODEX_PORT=8123` 是容器内监听的固定值**，蓝绿换版本时改 codex-node 宿主端口（如 8105/8106 +1），不动 8123。

## 7. 图床链路（`img.niji.edu.rs`）

图生成走 `codex2api` 应用层依然在 codex-node；生成后返回的 URL 形如 `https://img.niji.edu.rs/<sha256_8B>.png`，独立于主 API 链路。

```
client (看到的图 URL)
  → https://img.niji.edu.rs/<hash>.png
  → DNS → 152.53.240.159 (= node2)
  → node2 nginx (SSL 终结 + Let's Encrypt 证书)
  → location ~ \.(png|jpg|jpeg|webp)$
        ├─ try_files → /data/codex2api-images/<hash>     ← 老图（5-15 19:30 之前的）本地命中
        └─ fallback @codex_node → http://10.10.0.2:8888    ← 新图走 WG 反代
  → codex-node docker codex-images-nginx (nginx:alpine, mount /data/codex2api-images:ro)
```

**关键点**：

- 应用实际写文件到 codex-node `/app/images`（容器内）= host `/data/codex2api-images`（`docker -v` 挂载），并返回 URL `https://img.niji.edu.rs/<hash>.png`。
- node2 仍保留 7.8GB 老图，不需迁移；nginx `try_files → fallback` 语义保证老图本地读、新图走 WG 。
- `codex2api` 容器本身**不暴露 `/images/` 静态路由**，静态服务整个交给 `codex-images-nginx` 容器（`nginx:alpine`，只读挂盘）。
- DNS：`img.niji.edu.rs` A 记录 → `152.53.240.159`（node2），不变。所有入口还是 node2 接。

详细 nginx 配置见 `NGINX.md`，部署、重启、备份见 `DEPLOYMENT.md`。

## 8. 数据流向纪律

**写**：所有写都进 node2 PG（应用层 → WG → socat → PG）。Redis 仅缓存。

**镜像迁移**（codex2api 新版本部署到 codex-node）：
```bash
# 在 node2 上 build → save → scp 到 codex-node → load
docker save codex2api:vX.Y.Z -o /tmp/codex2api.tar
scp -P 22 /tmp/codex2api.tar root@10.10.0.2:/tmp/   # 走 WG
ssh -p 22 root@10.10.0.2 "docker load -i /tmp/codex2api.tar"
```
（具体蓝绿步骤见 `DEPLOYMENT.md`）

## 9. 迁移背景（一句话）

2026-05-15 11:00 节点 OOM 事故根因之一是 codex2api 在 node2 31GB 内存上跟 MySQL/horizon ×3/Kuboard 等多服务挤占；当晚 19:30 把应用层迁到独立 codex-node 永久脱困，node2 used 从 30GB 降到 21GB。详细复盘见 `KUBESPHERE_NODE2_SETUP.md` 第 1153 行起。
