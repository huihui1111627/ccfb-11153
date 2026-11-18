# dnsd — 连锁健身房会员 App 权威 DNS

自研 DNS 报文编解码（RFC 1035，含名字压缩与 EDNS）的权威 DNS 服务器，
带 Web 管理台，解决"机房故障改解析要 40 分钟"的问题：健康检查秒级摘除故障机房，
按客户端子网就近调度，全挂自动返回兜底地址。

## 功能对照

| 需求 | 实现 |
|---|---|
| A/AAAA/CNAME/MX/TXT/SRV/NS/SOA | `internal/dnsmsg` 全类型编解码，区文件导入导出（RFC 1035 master 格式，`internal/zone/masterfile.go`） |
| 按子网选最近机房 + 权重分流 | `internal/geo` 最长前缀匹配 + `internal/server.selectAddrs` 加权随机 |
| 健康检查摘除 + 兜底地址 | `internal/health` TCP/HTTP 主动探测；全挂返回区配置的 `fallback4/6`，未配兜底则 SERVFAIL |
| UDP 超长截断 + TCP 完整应答 | 超过客户端 EDNS 尺寸（无 EDNS 则 512）时只回问题段并置 TC=1；TCP 不截断 |
| 否定应答带 SOA | NXDOMAIN（名字不存在）与 NODATA（名字存在类型不存在）都在 authority 段带 SOA |
| 按网段限速防反射放大 | `internal/ratelimit`：每网段令牌桶 + 相同应答窗口计数，超限可截断（slip）或丢弃（drop） |
| 区顶点 ALIAS | 伪类型 ALIAS，服务端经上游递归解析目标并把地址内联进应答（`internal/alias`），TTL 跟随目标 |
| 子网→机房统计 | `internal/stats`，管理台"分配统计"页按地区/子网查看 |

## 运行

```sh
go build -o dnsd ./cmd/dnsd
./dnsd -config config.json
# DNS:  127.0.0.1:5354 (udp+tcp)
# 管理台: http://127.0.0.1:8054
```

管理台六个页签：域名区（含区文件导入导出、SOA/兜底/健康检查设置）、记录管理、
健康检查、解析日志、限速策略、分配统计。

配置见 `config.json`：`subnets` 定义子网→机房/地区映射，`datacenters` 定义机房权重，
`rate_limit` 定义限速策略（也可在页面实时改），`upstreams` 供 ALIAS 递归解析。

示例区 `data/zones/gym.example.zone` 启动时自动加载，页面上每次改动都会持久化回该目录。

## 测试

### 单元/集成测试

```sh
go test ./...
```

覆盖：报文往返编解码（含压缩、畸形指针/环路拒绝）、区文件解析导出往返、
地理调度、权重分布、健康摘除与兜底、NXDOMAIN/NODATA/REFUSED、
UDP 截断与 TCP 完整应答、限速 slip/drop、ALIAS 内联、CNAME 区内追击。

### dig 手工验证

```sh
dig @127.0.0.1 -p 5354 A     www.gym.example
dig @127.0.0.1 -p 5354 AAAA  api.gym.example
dig @127.0.0.1 -p 5354 CNAME app.gym.example      # 返回 CNAME + 区内追击的 A
dig @127.0.0.1 -p 5354 MX    gym.example
dig @127.0.0.1 -p 5354 TXT   gym.example
dig @127.0.0.1 -p 5354 SRV   _sip._tcp.gym.example
dig @127.0.0.1 -p 5354 NS    gym.example
dig @127.0.0.1 -p 5354 SOA   gym.example
dig @127.0.0.1 -p 5354 A     nope.gym.example     # NXDOMAIN + authority SOA
dig @127.0.0.1 -p 5354 AAAA  www.gym.example      # NODATA: NOERROR + authority SOA
dig @127.0.0.1 -p 5354 A     other.example        # REFUSED（非本服务区）

# 截断：big.gym.example 有 20 条 TXT，响应 >512
dig @127.0.0.1 -p 5354 TXT big.gym.example +noedns +ignore   # 看到 flags: ... tc
dig @127.0.0.1 -p 5354 TXT big.gym.example +noedns           # dig 自动改 TCP 拿全 20 条
dig @127.0.0.1 -p 5354 TXT big.gym.example +tcp              # TCP 直接拿全量
```

### 批量查询工具 dnsbench

```sh
go build -o dnsbench ./tools/dnsbench

# 批量：文件每行 "域名 类型"，统计 rcode/截断/时延
./dnsbench -server 127.0.0.1:5354 -file queries.txt -n 5000 -c 20
./dnsbench -server 127.0.0.1:5354 -file queries.txt -n 2000 -tcp

# 故障切换计时：100ms 轮询，应答集变化时打印
./dnsbench -server 127.0.0.1:5354 -watch lb.gym.example -type A -interval 100ms
```

### 模拟机房故障，测量切换时间

1. 页面"域名区 → 区设置"开启健康检查（如 TCP 18080，间隔 1s），设置兜底 IPv4。
2. 加一条 `lb.gym.example A 127.0.0.1`（dc=sh），本地起个监听模拟后端：
   `nc -l 18080`（或任何 TCP 服务）。
3. 跑 `./dnsbench -watch lb.gym.example -interval 100ms`，确认返回 127.0.0.1。
4. 杀掉监听进程，watch 打印切换到兜底地址的时刻。
   实测（1s 探测间隔 + 1s 超时）：故障后 **<1s** 解析结果切到兜底，恢复后 ~1s 切回。
   对比原来手动改服务商解析的 40 分钟。

### 限速验证

页面"限速策略"把相同应答上限设为 5、窗口 3s、动作"截断"，然后连续查同一名称：

```sh
for i in $(seq 12); do dig @127.0.0.1 -p 5354 A www.gym.example +noall +comments +ignore | grep -o 'flags: [a-z ]*'; done | sort | uniq -c
# 前 5 个完整应答，之后全是 tc 截断（动作选 drop 则超时无响应）
```

### 报文格式对照 RFC 检查

dig 能正常解析即说明线格式与 RFC 1035 互操作（dig 本身即参考实现）。抓包核对：

```sh
sudo tcpdump -i lo0 -w /tmp/dns.pcap udp port 5354 or tcp port 5354 &
# 跑上面的 dig / dnsbench
sudo tcpdump -r /tmp/dns.pcap -nn -X port 5354 | less
```

或用 Wireshark 打开 `dns.pcap`，对照 RFC 1035 §4 逐项检查：
头部 12 字节各字段、问题段、资源记录段（NAME/TYPE/CLASS/TTL/RDLENGTH/RDATA）、
名字压缩指针（0xC0 偏移）、TC 标志位、SOA 五个时间字段。单元测试
`internal/dnsmsg/msg_test.go` 已覆盖压缩指针、越界指针、环路指针等边界报文。

## 目录结构

```
cmd/dnsd            主程序（配置加载、组件装配、区持久化）
internal/dnsmsg     DNS 报文编解码（手写，含压缩与 EDNS）
internal/zone       区数据模型、存储、master 文件导入导出
internal/geo        子网→机房/地区最长前缀匹配
internal/health     TCP/HTTP 主动健康检查
internal/ratelimit  网段令牌桶 + 相同应答限制
internal/alias      ALIAS 目标递归解析（UDP，TC 时回退 TCP）
internal/server     查询流水线：限速→解析→调度→截断→日志
internal/stats      查询日志环 + 子网/地区机房分配统计
internal/web        管理 API + 内嵌单页管理台
tools/dnsbench      批量查询 / 故障切换计时工具
```
