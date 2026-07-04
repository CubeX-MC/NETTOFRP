# NETTOFRP

Minecraft 服务器 FRP 线路的智能选路入口。将多条独立的 FRP 线路合并为单一的 `auto` 访问入口，后台实时探测各线路质量，为玩家自动选择最优线路。

## 工作原理

玩家只需连接 `auto` 入口，程序在后台周期性并行探测所有线路（延迟、抖动、成功率、带宽），综合打分后维护一个全局最优线路。新连接到来时，程序按客户端能力选择两种方式之一将其导向最优线路：

```
                          ┌─────────────────────────────┐
                          │        NETTOFRP (auto)        │
玩家 ──握手──▶            │  ① 探测循环：打分 → 最优线路   │
                          │  ② 分流：Transfer / TCP 透传  │
                          └──────────────┬────────────────┘
                                         │
         ┌───────────────────────────────┼───────────────────────────────┐
         ▼                               ▼                                ▼
      play2                           play3                         play-srv (SRV)
         └────────────── 各线路后端（如 limbo 做正版验证）──────────────┘
```

### 两种选路方式

| 方式 | 触发条件 | 效果 |
|------|----------|------|
| **Transfer 直连** | 开启 `enable_transfer` 且客户端协议 ≥766（1.20.5+）| 代理下发 Transfer 包令客户端**直连**最优线路，游戏流量不经过代理，**无中转延迟** |
| **TCP 透传** | 低版本客户端 / 状态查询 / Transfer 关闭 | 代理中转转发，作为兜底，保证任何客户端都可用 |

Transfer 路径下，NETTOFRP 只做「引路」：以离线方式完成登录握手并把玩家 Transfer 到最优线路，**真正的正版验证由该线路后端（如 limbo + littleskin）完成**。两层是接力关系，不冲突。

## 评分算法

对每条可达线路按绝对基准归一化后加权（默认权重）：

- **延迟** `0.6`：以 300ms 为基准反向归一化，越低越高分
- **稳定性** `0.3`：成功率(0.7) + 抖动(0.3)，抖动以 100ms 为基准
- **带宽** `0.1`：相对本轮最大值归一化；MC 握手阶段通常测不到，此时给中性分 0.5

采用绝对基准而非线路间相对归一化，避免把毫秒级的微小延迟差放大成满分差距，让稳定性权重真正生效。

## 配置

复制 `config.example.json` 为 `config.json` 后按需修改：

```bash
cp config.example.json config.json
```

```json
{
  "listen": "0.0.0.0:25565",
  "mc_host": "auto.cubexmc.org",
  "probe_interval_seconds": 15,
  "probe_samples": 5,
  "probe_timeout_ms": 2000,
  "enable_transfer": true,
  "transfer_packet_id": 11,
  "weights": {
    "latency": 0.6,
    "stability": 0.3,
    "bandwidth": 0.1
  },
  "lines": [
    { "name": "play2", "address": "play2.cubexmc.org:25565" },
    { "name": "play3", "address": "play3.cubexmc.org:25565" },
    { "name": "play-srv", "address": "play.cubexmc.org", "srv": true }
  ]
}
```

| 字段 | 说明 |
|------|------|
| `listen` | auto 入口监听地址 |
| `probe_interval_seconds` | 探测周期 |
| `probe_samples` | 每轮每条线路的采样次数 |
| `probe_timeout_ms` | 单次建连超时，同时用作转发建连超时 |
| `enable_transfer` | 是否对 1.20.5+ 客户端启用 Transfer 直连 |
| `transfer_packet_id` | configuration 状态 Transfer 包 ID，1.20.5~1.21.x 为 `11`(0x0B)。**未来版本包 ID 若变动，改此处即可，无需改代码** |
| `weights` | 三项指标权重 |
| `lines[].srv` | 为 `true` 时 `address` 只填域名，程序查询 `_minecraft._tcp.<域名>` 的 SRV 记录获得真实 host:port |

## 构建与运行

```bash
# 本地运行
go run . -config config.json

# 交叉编译 Linux 二进制
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/nettofrp-linux-amd64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/nettofrp-linux-arm64 .
```

### 部署（nohup 后台常驻）

```bash
# 上传（x86_64 用 amd64）
scp dist/nettofrp-linux-amd64 root@<服务器>:/data/NTF/nettofrp
scp config.json root@<服务器>:/data/NTF/config.json

# 启动
ssh root@<服务器> 'chmod +x /data/NTF/nettofrp && cd /data/NTF && nohup ./nettofrp -config config.json > nettofrp.log 2>&1 &'

# 查看日志 / 停止
ssh root@<服务器> 'tail -f /data/NTF/nettofrp.log'
ssh root@<服务器> 'pkill -f "nettofrp -config"'
```

> 记得放行 `listen` 端口的入站 TCP（防火墙 + 云安全组）。

## 项目结构

| 目录 | 职责 |
|------|------|
| `internal/config` | JSON 配置加载、校验与默认值 |
| `internal/resolver` | 线路地址解析，SRV 查询 + 缓存 + DNS 抖动时的陈旧结果回退 |
| `internal/prober` | 多次采样采集延迟、抖动、成功率、带宽 |
| `internal/selector` | 加权综合评分与候选线路排序 |
| `internal/mcproto` | 最小 MC 协议子集：握手/登录包读取、Login Success 与 Transfer 包构造 |
| `internal/proxy` | auto 入口：Transfer 分流 + TCP 透传兜底 + 按评分故障转移 |

## 容错设计

- **DNS 抖动**：SRV 解析临时失败时复用上次成功结果，避免拖慢连接
- **线路掉线**：按评分顺序故障转移，最优线路连不上自动退到次优
- **版本兼容**：只解析多年稳定的握手/登录/Transfer 结构，不碰游戏内封包；低版本客户端自动回落 TCP 透传

## 已知限制

- 探测测的是「NETTOFRP 服务器 → 各线路」的质量，不含「玩家 → NETTOFRP」这一段。中转机的网络位置会影响最终体验，建议部署在网络位置较优的机器上。
- Transfer 直连依赖客户端协议 ≥766（1.20.5+），更早版本走 TCP 透传，中转延迟依旧存在。
