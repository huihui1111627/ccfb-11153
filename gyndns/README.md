# gyndns — 连锁健身房会员 App 权威 DNS

为"三个机房 + 会员 App"场景写的权威 DNS 服务器:机房故障时解析结果秒级自动切换,
不再需要在服务商后台手动改解析。DNS 报文编解码为手写实现(无第三方依赖,仅标准库)。

## 功能对照

| 需求 | 实现 |
|---|---|
| A/AAAA/CNAME/MX/TXT/SRV/NS/SOA | `internal/dnsmsg` 全类型编解码,zone 文件导入导出(RFC 1035) |
| 按客户端子网选最近机房 | 记录可绑 CIDR 列表;客户端子网取 EDNS ECS(`dig +subnet=`),无 ECS 用源 IP |
| 按权重分流量 | 每条记录带权重,池内加权随机 |
| 健康检查失败自动摘除 | `internal/health` 周期探测(tcp/http),连续 2 次失败摘除,恢复自动加回 |
| 全挂返回兜底地址 | 记录标记 `backup`,常规地址全不可用才启用 |
| UDP 超长设 TC,TCP 拿完整结果 | 无 EDNS 按 512 字节截断设 TC;EDNS 按客户端宣告值(上限 1232);TCP 不截断 |
| 不存在域名返回带 SOA 否定应答 | NXDOMAIN / NODATA 都在 authority 段带 SOA,TTL 取 SOA minimum |
| 主改记录通知从,增量同步,对不上退全量 | 主侧变更写 journal 并发 NOTIFY;从 IXFR 拉增量,journal 覆盖不了自动 AXFR |
| 通配符 + CNAME 展开,链长/成环报错 | RFC 4592 通配符(含空非终端);CNAME 最多 8 层,成环/超长返回 SERVFAIL |
| 解析日志按域名/来源查,看选路过程 | 每次应答记录完整决策链(命中区→子网匹配→健康过滤→权重选择),页面可查 |

## 管理页面

`http://localhost:8053` 五个板块:**域名区**(建区/导入导出 zone 文件)、**记录管理**
(增删改 + 权重/子网/兜底/健康检查元数据)、**健康检查**(状态 + 强制上下线做故障演练)、
**解析日志**(按域名/来源过滤,展开看每条应答的决策过程)、**主从同步**(serial、
变更日志、手动触发 NOTIFY/同步)。

## 运行

```bash
# 主服务器(机房一)
go run ./cmd/gyndns -role master \
  -dns :5353 -http :8053 \
  -data master.json -zonefile testdata/gym.zone \
  -slaves "10.2.0.53:5353,10.3.0.53:5353"

# 从服务器(机房二/三)
go run ./cmd/gyndns -role slave \
  -dns :5353 -http :8054 \
  -data slave.json -master 10.1.0.53:5353 -zones gym.example.
```

主要参数:`-check-interval`(健康检查间隔,默认 5s)、`-refresh-interval`
(从服务器兜底轮询,默认 60s,防 NOTIFY 丢包)。

## 测试

```bash
go test ./internal/...     # 单元测试: 编解码边界报文/通配符/CNAME/选路/主从同步
./scripts/e2e.sh           # 端到端: 真实 dig + digbench, 26 项检查
```

e2e 覆盖:八种记录类型、NXDOMAIN/NODATA 带 SOA、通配符、CNAME 链展开与成环
SERVFAIL、`+subnet=` 子网选路、UDP 截断 TC + TCP 完整结果、AXFR、批量查询
(300 查询无失败)、主从 AXFR→NOTIFY→IXFR、故障切换计时、全机房故障兜底。

### digbench — 批量查询与故障计时工具

```bash
# 批量压测(查询文件每行: 域名 类型 [客户端子网IP])
digbench -server 127.0.0.1:5353 -file queries.txt -n 100 -c 16

# 故障切换计时: 100ms 轮询, 应答集变化时打印时间戳
digbench -server 127.0.0.1:5353 -name app.gym.example -subnet 10.1.0.0/16 -watch 100ms

# 校验应答内容
digbench -server 127.0.0.1:5353 -name app.gym.example -expect 10.1.0.10,10.2.0.10
```

实测(本机,健康检查间隔 1s):杀掉一个机房的监听后 **约 1.7s** 解析结果切换到
健康机房;对照原来手动改解析的 40 分钟。

### 报文格式对照 RFC 检查

- `internal/dnsmsg/interop_test.go`:把 dig 9.10.6 真实发出的查询报文(抓包存的
  hex)喂给我们的解码器,逐字段核对后再编码回去逐字节比对;
- golden test 按 RFC 1035 §4 手工构造期望字节流(含压缩指针 `0xC00C`),锁定
  编码器输出;
- e2e 里 dig 能正确解析我们所有应答(含截断后 `+tcp` 重试),即线上互操作验证。

## 结构

```
cmd/gyndns     服务器主程序
cmd/digbench   批量查询/故障计时工具
internal/dnsmsg  DNS 报文编解码(手写, 含压缩指针/EDNS/ECS)
internal/zone    区数据、通配符、CNAME 链、否定应答、zone 文件、变更 journal
internal/router  子网就近 + 权重 + 健康过滤 + 兜底
internal/health  健康检查器(tcp/http, 支持强制上下线演练)
internal/server  UDP/TCP 监听、截断、日志挂钩
internal/xfr     NOTIFY + IXFR/AXFR 主从同步
internal/qlog    解析日志环形缓冲(含决策链)
internal/web     管理页面与 JSON API
testdata/gym.zone  三机房示例区文件
scripts/e2e.sh     端到端验证(26 项)
```
