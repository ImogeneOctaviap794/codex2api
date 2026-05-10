# 反检测升级路线

## 1. 核心哲学

**隐入人群，而不是扮演明星。**

- 让检测成本 > 收益
- 多层一致性 > 单层完美
- 评估升级时必问三件事：当前异常频率 / 升级后可维持多久 / 成本回收周期

## 2. 当前策略（已上线 / 正在做）

| 层 | 实现 | 版本 |
|---|---|---|
| 应用层 | 113 UA 画像 + FNV hash | v1.7.40-ua-proxy |
| 传输层 | 双轨 TLS（`CODEX_TRANSPORT_MODE=standard`/`utls_chrome`）| 合并 upstream 6204889 |
| 会话层 | session_id 按 API Key 哈希隔离 | - |
| 网络层 | 代理 IP 池 | - |

**双轨模式含义**：
- `standard`（默认）：Go 原生 TLS，**混入 openai-go SDK 人群**
- `utls_chrome`：uTLS 仿 Chrome，作为备用

## 3. 备选方案：自定义 rustls 画像（高成本高价值）

**触发条件**（须全部满足才值得做）：

- 观察到 OpenAI 部署细粒度 JA4 过滤
- `standard` 和 `utls_chrome` 两个模式都开始封号
- 业务量大到自定义画像维护成本 < 封号损失

### 3.1 技术实现路径

#### Step 1 · 抓真实 `codex_cli_rs` 的 ClientHello
- 环境：mac 或 linux 装真 codex CLI
- 工具：`tcpdump -i any -w codex.pcap host api.openai.com`
- 或 Wireshark 带 TLS keylog

#### Step 2 · 解析 ClientHello 字段
- Cipher suites 列表和顺序
- Extensions 列表和顺序（含 GREASE 位置）
- Supported groups（X25519 + 传统曲线）
- Signature algorithms
- ALPN（h2, http/1.1）

#### Step 3 · 用 uTLS 手写画像
- `utls.ClientHelloSpec{...}` 自定义 spec
- 参考 `utls/u_parrots.go` 里 Chrome/Firefox 写法
- 注册为 `HelloCustom_Rustls_v0_23`（带 rustls 版本号）
- 落到 `proxy/utls_rustls.go`，作为 `CODEX_TRANSPORT_MODE=rustls` 实现

### 3.2 工程成本

- **初始开发**：2-3 天
- **持续维护**：rustls 新版本发布后验证（~月频）
- **风险**：rustls ClientHello 细节变动需及时跟进，否则反而暴露

### 3.3 关键参考

- [`refraction-networking/utls`](https://github.com/refraction-networking/utls) → `u_parrots.go`
- `codex_cli_rs` 源码：依赖 `reqwest`，reqwest 依赖 `rustls`
- rustls 版本：`cargo tree` 看 codex 的 `Cargo.lock`

## 4. 多层对比速查

| 层 | 修改前（v1.7.41）| 当前（双层保险）| rustls 升级（备选）|
|---|---|---|---|
| TLS 指纹 | uTLS Chrome | Go 原生 | rustls 仿真 |
| UA | 113 画像 | 113 画像 | 113 画像 |
| session | 透传 | API Key 哈希 | API Key 哈希 |
| debug 日志 | 无 | 有 | 有 |
