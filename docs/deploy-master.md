# 主控（mon-master）部署教程

从一台空机器到「看板能打开、节点能上报」，一步一步来。本文所有命令都在 Debian/Ubuntu 与
RHEL 系上验证过；Docker 方式另见[第 7 节](#7-docker-方式)。

> 节点侧的安装脚本：Linux 用 [`deploy/install-agent.sh`](../deploy/install-agent.sh)，
> Windows 用 [`deploy/install-agent.ps1`](../deploy/install-agent.ps1)。

---

## 0. 部署前先理解一件事：主控是「只进不出」的

这不是修辞。`mon-master` 的代码里**不存在**任何出站网络调用（没有 `http.Get`、没有 `net.Dial`），
这条性质由 `scripts/security-audit.sh` 每次提交强制校验。它带来两个直接的部署结论：

| 结论 | 意味着什么 |
|---|---|
| 网络规划很简单 | 只需要一个**入向**端口（443），不需要给主控配任何出站白名单、代理、DNS 到节点的解析 |
| 「节点离线」只能等 | 主控无法主动探测节点、无法触发补报。节点是不是活着，全靠它自己推数据上来 |

所以主控可以放在一个**完全无法访问内网节点**的隔离区（DMZ）里——这正是推荐做法。

---

## 1. 选部署形态

| 形态 | 适用 | 入口 |
|---|---|---|
| **systemd + nginx 反代** | 生产推荐。TLS 由 nginx 终止，主控只听 127.0.0.1 | 本文第 2–6 节 |
| **Docker Compose** | 已有容器基础设施，或想连 nginx 一起起 | 第 7 节 |
| **直接裸跑** | 只在内网联调、临时验证 | 第 5 节（跳过第 6 节） |

主控**不要**直接监听 `0.0.0.0`。它没有内置 TLS，裸暴露等于把上报令牌和全部指标隐私
放在明文里。如果确实要这么干，代码会强制你显式设置 `admin_token`（见第 4 节）。

---

## 2. 准备主机

### 2.1 规格

| 项 | 建议 |
|---|---|
| CPU / 内存 | 1 核 / 1 GB 足够（实测常驻内存约 15 MB / 10 节点） |
| 磁盘 | 单节点约 1.5 MB/天（`interval_sec=10`，压缩后）。100 节点 × 30 天 ≈ 4.5 GB |
| 系统 | 任意 Linux + systemd（脚本依赖 systemd 做加固）；Windows 也能跑，但没有现成单元文件 |

### 2.2 建专用用户和目录

主控以**非登录、无特权**的用户运行：

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin mon-master

sudo install -d -m 0755 -o root      -g root      /etc/mon-master
sudo install -d -m 0700 -o mon-master -g mon-master /var/lib/mon-master
```

- `/etc/mon-master` 放配置（内含令牌，稍后设成 `600`）；
- `/var/lib/mon-master` 是**唯一**的可写目录，全部指标数据都落在这里。

### 2.3 放二进制

```bash
sudo install -m 0755 mon-master-linux-amd64 /usr/local/bin/mon-master
mon-master -version      # → mon-master 1.2.0 (built ...)
```

`dist/SHA256SUMS.txt` 里有全部产物的校验和，公网传输后建议核对：

```bash
sha256sum -c SHA256SUMS.txt --ignore-missing
```

---

## 3. 生成令牌

有两种上报鉴权模式，**先用哪种要想清楚**，因为节点侧的 `token` 必须与之对应。

| 模式 | 配置字段 | 优点 | 缺点 |
|---|---|---|---|
| 全局令牌 | `report_token` | 配置简单 | 任一节点泄露令牌 = 可以伪造**任意**节点的数据 |
| **一节点一令牌**（推荐） | `node_tokens` | 某台机器的令牌泄露，只能污染它自己 | 节点数量多时要多写几行 |

生成强随机令牌：

```bash
openssl rand -hex 24        # 48 个十六进制字符
```

另外还有一个 `admin_token`，它**只用于读取**（看板与查询接口），与上报令牌必须不同：

```bash
openssl rand -hex 24
```

> 三个令牌的权限边界：`report_token` / `node_tokens` 只能**写**；`admin_token` 只能**读**。
> 不存在任何一个令牌能同时读写。

---

## 4. 写配置文件

```bash
sudo tee /etc/mon-master/master.json >/dev/null <<'EOF'
{
  "listen": "127.0.0.1:8080",
  "data_dir": "/var/lib/mon-master",
  "title": "服务器监控",
  "public_base": "https://monitor.example.com",

  "node_tokens": {
    "web-01":  "把 openssl rand -hex 24 的输出贴这里",
    "db-01":   "每台机器一个，互不相同"
  },

  "admin_token": "看板读取令牌，与上面全部不同",

  "retention_days": 30,
  "cache_per_node": 1440,
  "max_body_kb": 256,
  "rate_limit_per_sec": 5,
  "trust_proxy": false
}
EOF

sudo chown root:root /etc/mon-master/master.json
sudo chmod 600      /etc/mon-master/master.json
```

### 字段说明

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen` | `127.0.0.1:8080` | 监听地址。**保持回环**，由 nginx 反代 |
| `data_dir` | `./data` | 数据根目录。每节点一个子目录：`<node_id>/YYYY-MM-DD.jsonl` |
| `title` | `服务器监控` | 看板标题 |
| `public_base` | 空 | 对外地址，**仅用于文档展示**，不影响监听行为 |
| `report_token` | 空 | 全局上报令牌（≥12 字符）。与 `node_tokens` 二选一 |
| `node_tokens` | 空 | 一节点一令牌，映射 `节点名 → 令牌`。**非空时优先于 `report_token`** |
| `admin_token` | 空 | 读取令牌（≥12 字符）。为空时**只允许本机访问**看板 |
| `retention_days` | 30 | 保留天数，范围 1–3650 |
| `cache_per_node` | 1440 | 内存中每节点缓存的样本数（供历史查询用），最小 60 |
| `max_body_kb` | 256 | 单条上报体积上限，范围 16–4096 |
| `rate_limit_per_sec` | 5 | 每 IP / 每节点的令牌桶速率 |
| `trust_proxy` | false | 反代后面**必须设为 true**，否则限流会把所有节点算成 nginx 一个 IP |

`node_tokens` 里的节点名必须符合 `[A-Za-z0-9][A-Za-z0-9._-]{0,63}` —— 它会直接成为磁盘上的
目录名，所以这是防路径穿越的第一道关（主控内部还有第二道）。

### 两个会被代码拦下来的坑

```bash
# 1) 一个令牌都没配 → 拒绝启动
必须配置 report_token 或 node_tokens，否则任何来源都能写入数据

# 2) 监听了非回环地址却没设 admin_token → 拒绝启动
监听地址 0.0.0.0:8080 不是回环地址，但未设置 admin_token。这会让任何能访问该端口的人看到全部节点数据。
```

第 2 条如果确认风险自担，可以 `MON_MASTER_ALLOW_OPEN_READ=1` 放行——但**不推荐**。

---

## 5. 启动与验收

### 5.1 装 systemd 服务

```bash
sudo install -m 0644 deploy/mon-master.service /etc/systemd/system/mon-master.service
sudo systemctl daemon-reload
sudo systemctl enable --now mon-master
```

单元文件已经做了完整加固（`ProtectSystem=strict`、`CapabilityBoundingSet=` 为空、
`RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`、只放开 `/var/lib/mon-master` 可写）。
因为主控运行期只需要「读数据目录 + 应答 HTTP」，这些限制不会挡到任何功能。

```bash
systemctl status mon-master
journalctl -u mon-master -f
```

正常启动的日志长这样：

```
已从磁盘恢复 1 个节点的最新状态
mon-master 1.2.0 已启动，监听 127.0.0.1:8080，数据目录 /var/lib/mon-master，保留 30 天
```

### 5.2 三个验收动作

```bash
# ① 存活探针（无需鉴权）
curl -fsS http://127.0.0.1:8080/healthz

# ② 元信息（无需鉴权）
curl -fsS http://127.0.0.1:8080/api/v1/meta
# {"name":"mon-master","read_auth":true,"retention_days":30,"version":"1.2.0",...}

# ③ 模拟一条上报（把 web-01 换成你 node_tokens 里真实存在的节点名）
curl -fsS -X POST http://127.0.0.1:8080/api/v1/report \
  -H 'Authorization: Bearer 那个节点的令牌' \
  -H 'Content-Type: application/json' \
  -d '{"node_id":"web-01","ts":0,"host":{"hostname":"web-01","os":"linux","arch":"amd64"},
       "cpu":{"usage_pct":12.5,"cores":8,"threads":16,"sockets":1}}'
# {"ok":true,"ts":1789134605}
```

返回 `{"ok":true,...}` 就说明**写入链路完全通了**。这时打开看板应该能看到一个节点。

### 5.3 上报接口的错误码（排查时照着看）

| 码 | 含义 | 常见原因 |
|---|---|---|
| 401 | 上报令牌无效 | 节点侧 `token` 与主控配的不一致 |
| 403 | 该节点未授权 | 用了 `node_tokens` 模式，但里面没有这个 `node_id` |
| 400 | JSON 解析失败 / node_id 不合法 | 节点名带了大写以外的不允许字符，或手工测试时 JSON 写错 |
| 413 | 请求体超过限制 | 传感器特别多时调大 `max_body_kb` |
| 429 | 请求过于频繁 | 节点数很多而 `interval_sec` 很小，或反代后没开 `trust_proxy` |
| 500 | 服务端写入失败 | 数据目录权限不对，看 `journalctl` |

---

## 6. 配 HTTPS 反代（生产必做）

主控没有内置 TLS，**必须**由 nginx/caddy 终止 TLS。

### 6.1 证书

```bash
# 公网域名，用 certbot
sudo certbot certonly --nginx -d monitor.example.com

# 内网自签 / 内部 CA：把这个 CA 装到每台节点上，然后让 agent 指向它
# 节点侧配置 tls.ca_file（见 agent.example.json），不要用 insecure_skip_verify
```

### 6.2 反代

`deploy/nginx.conf.example` 是可直接改用的模板，核心是两段：

```nginx
location = /api/v1/report {          # 上报：小体积、高频，单独限流
    limit_req zone=mon_report burst=40 nodelay;
    client_max_body_size 256k;       # 与 master 的 max_body_kb 对齐
    proxy_pass http://mon_master;
}
location / {                         # 看板与查询
    limit_req zone=mon_read burst=60 nodelay;
    proxy_pass http://mon_master;
}
```

```bash
sudo install -m 0644 deploy/nginx.conf.example /etc/nginx/conf.d/monitor.conf
sudo nginx -t && sudo systemctl reload nginx
```

开了 IP 白名单后，**必须**同时在 `master.json` 里设 `"trust_proxy": true`，
否则主控看到的客户端 IP 全是 nginx 的，限流会把所有节点当成同一台机器。

### 6.3 防火墙

```bash
# 只开 443（以及你自己的 SSH 管理口），不要开 8080
sudo ufw allow 443/tcp
sudo ufw enable
```

节点侧只需要**出向** TCP/443，不需要任何入向端口。

### 6.4 跨公网时的加固建议

- `/api/v1/report` 只对节点所在网段开放（`allow 10.0.0.0/8; deny all;`）；
- `/`（看板）只对办公网出口开放；
- 永远不要把主控放在没有 IP 白名单的公网裸奔——`admin_token` 只能防误看，防不住爆破。

---

## 7. Docker 方式

```bash
# 1) 造镜像（Dockerfile 会把版本号编译进去）
docker build -f deploy/Dockerfile.master -t mon-master:1.2.0 .
docker build -f deploy/Dockerfile.agent  -t mon-agent:1.2.0  .

# 2) 起服务（compose 里 master 已经把数据目录挂到卷上）
docker compose -f deploy/docker-compose.yml up -d
docker compose -f deploy/docker-compose.yml logs -f master
```

容器化的两个注意点：

- **master 容器不需要 `--network host`**，也不需要任何出站规则；把 8080 映射到宿主回环即可
  （`127.0.0.1:8080:8080`）。
- **agent 容器需要挂 `/sys` 才能读到温度**：`-v /sys:/sys:ro`。同时要注意容器里的
  `/proc/stat`、`/proc/meminfo` 反映的是**宿主机内核**的全局数据，不是容器配额——
  在节点上跑 agent 建议用 systemd 而非容器，见 [install-agent.sh](../deploy/install-agent.sh)。

---

## 8. 看板访问

浏览器打开 `http://127.0.0.1:8080`（或反代后的 `https://monitor.example.com`）。

- 设了 `admin_token` 时，首次访问会要你填令牌 → 点右上角「令牌」，粘进去即可。
  令牌存在浏览器 localStorage 里，**不会**发往任何第三方。
- 没设 `admin_token` 时看板直接可看，但主控会被强制要求监听回环地址（见第 4 节）。

看板每 5 秒刷新数据、每 15 秒重绘曲线，不需要手动刷新。

---

## 9. 日常运维

### 备份

数据就是一堆纯文本文件，**直接打包目录即可**，不需要 `mysqldump` 之类的工具：

```bash
sudo tar czf mon-master-$(date +%F).tar.gz -C /var/lib mon-master
```

停下来复制也是安全的：写入是「按天追加 JSONL」，没有复杂的中间状态。

### 升级

```bash
sudo systemctl stop mon-master
sudo install -m 0755 mon-master-linux-amd64 /usr/local/bin/mon-master
sudo systemctl start  mon-master
```

数据格式向后兼容（新增字段都是 `omitempty` 的可选字段），**不需要**迁移脚本。
旧版本读新数据、新版本读旧数据都能正常工作。

### 容量

| 周期 | 单节点未压缩 | 单节点压缩后 |
|---|---|---|
| 1 天 | ≈ 13 MB | ≈ 1.5 MB |
| 30 天 | ≈ 390 MB | ≈ 45 MB |

非当天的文件会被每分钟的维护任务自动 gzip，超过 `retention_days` 的自动清理。
查历史数据直接读文件，不需要额外的查询引擎：

```bash
jq -c 'select(.ts > 1789000000)' /var/lib/mon-master/web-01/2026-09-11.jsonl | head
```

### 卸载

```bash
sudo systemctl disable --now mon-master
sudo rm -f /usr/local/bin/mon-master /etc/systemd/system/mon-master.service
sudo systemctl daemon-reload
sudo rm -rf /etc/mon-master          # 配置（含令牌）
# /var/lib/mon-master 里是历史数据，确认不需要后再删
```

---

## 10. 故障排查

| 现象 | 原因与处理 |
|---|---|
| 启动报「必须配置 report_token 或 node_tokens」 | 两个都没配。补一个 |
| 启动报「不是回环地址，但未设置 admin_token」 | 预期行为。要么加 `admin_token`，要么确认风险后加 `MON_MASTER_ALLOW_OPEN_READ=1` |
| 启动报「listen 地址不合法」 | 要写成 `host:port`，例如 `127.0.0.1:8080`，不能只写 `:8080` 之外的半截 |
| 看板空白 / 提示 unauthorized | 没填 `admin_token`。点右上角「令牌」 |
| 节点一直显示「离线」 | 主控侧：`journalctl -u mon-master` 看有没有 401/403；节点侧：`journalctl -u mon-agent -f` |
| 日志里大量 401 | 节点侧 `token` 与主控 `node_tokens` 不一致 |
| 日志里大量 429 | 反代后忘记开 `trust_proxy: true`，或 `interval_sec` 小于合理值 |
| 数据目录写入失败 | `/var/lib/mon-master` 属主必须是 `mon-master`：`chown -R mon-master:mon-master /var/lib/mon-master` |
| 端口被占 | `ss -lntp \| grep 8080`。注意主控**不会**自动重试绑定，会直接退出 |
| 磁盘涨得比预期快 | 单节点传感器或网络接口特别多时单条样本会变大，调小 `retention_days` 或节点侧 `collect.temp_limit` |
| 反向代理后看不到真实 IP | `"trust_proxy": true` 没设 |
| 想看主控到底有没有出站能力 | `sudo ss -tnp \| grep mon-master` —— 只会有 nginx 连进来的连接，永远没有主动发起的 |

---

## 附：主控安全模型

「为什么主控做不到操作节点」的完整论证（含 8 条结构性保证与威胁模型）见
[SECURITY.md](../SECURITY.md)。一句话版本：

> 主控连「往外发一个请求」这个动作都做不到。即使主控主机被完全攻陷，
> 攻击者拿到的也只是一个「能读磁盘上已有指标、能应答 HTTP 请求」的进程，
> 没有任何可用的原语去联系节点。
