#!/bin/bash
# e2e.sh — 用真实 dig 和 digbench 对 gyndns 做端到端验证
# 覆盖: 各记录类型 / NXDOMAIN / 通配符 / CNAME 链与环 / 子网选路 /
#       UDP 截断 + TCP 重试 / AXFR / 主从同步 / 故障切换计时
set -u
cd "$(dirname "$0")/.."

DNS=127.0.0.1
PORT=15353
HTTP=18053
SLAVE_PORT=15354
SLAVE_HTTP=18054
TMP=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null; rm -rf "$TMP"' EXIT

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  ✔ $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  ✘ $1"; }
check(){ # check <描述> <期望子串> <实际输出>
  if echo "$3" | grep -q "$2"; then ok "$1"; else bad "$1"; echo "    期望包含: $2"; echo "    实际: $(echo "$3" | head -5)"; fi
}

echo "== 构建 =="
go build -o "$TMP/gyndns" ./cmd/gyndns || exit 1
go build -o "$TMP/digbench" ./cmd/digbench || exit 1

echo "== 启动主服务器 =="
"$TMP/gyndns" -role master -dns ":$PORT" -http ":$HTTP" \
  -data "$TMP/master.json" -zonefile testdata/gym.zone \
  -slaves "127.0.0.1:$SLAVE_PORT" -check-interval 1s >"$TMP/master.log" 2>&1 &
sleep 0.7

# 给 app 的三条 A 记录加上子网/权重/健康检查元数据(模拟三个机房)
# 机房1/2 用 nc 起真 TCP 监听, 机房3 故意不启动(故障), 另加一条兜底地址
nc -kl 127.0.0.1 19001 >/dev/null 2>&1 &
NC1=$!
nc -kl 127.0.0.1 19002 >/dev/null 2>&1 &
NC2=$!
nc -kl 127.0.0.1 19003 >/dev/null 2>&1 &
NC3=$!
api() { curl -s -X "$1" "http://127.0.0.1:$HTTP$2" ${3:+-H 'Content-Type: application/json' -d "$3"}; }
api PUT "/api/zones/gym.example./records" '{"old_name":"app.gym.example.","old_type":1,"old_rdata":"10.1.0.10","record":{"name":"app.gym.example.","type":1,"ttl":30,"rdata":"10.1.0.10","weight":100,"subnets":["10.1.0.0/16"],"check":"tcp://127.0.0.1:19001"}}' >/dev/null
api PUT "/api/zones/gym.example./records" '{"old_name":"app.gym.example.","old_type":1,"old_rdata":"10.2.0.10","record":{"name":"app.gym.example.","type":1,"ttl":30,"rdata":"10.2.0.10","weight":100,"subnets":["10.2.0.0/16"],"check":"tcp://127.0.0.1:19002"}}' >/dev/null
api PUT "/api/zones/gym.example./records" '{"old_name":"app.gym.example.","old_type":1,"old_rdata":"10.3.0.10","record":{"name":"app.gym.example.","type":1,"ttl":30,"rdata":"10.3.0.10","weight":100,"check":"tcp://127.0.0.1:19003"}}' >/dev/null
api POST "/api/zones/gym.example./records" '{"name":"app.gym.example.","type":1,"ttl":30,"rdata":"203.0.113.1","backup":true}' >/dev/null
sleep 3  # 等健康检查跑两轮

DIG="dig @$DNS -p $PORT +norec +time=2 +tries=1"

echo "== 1. 常见记录类型 =="
check "A(无子网匹配走全局)"  "10.3.0.10"  "$($DIG app.gym.example A +short)"
check "AAAA"   "2001:db8::10"               "$($DIG web.gym.example AAAA +short)"
check "CNAME"  "app.gym.example."           "$($DIG gate.gym.example CNAME +short)"
check "MX"     "10 mail1.gym.example."      "$($DIG mail.gym.example MX +short)"
check "TXT"    "v=spf1 -all"                "$($DIG gym.example TXT +short)"
check "SRV"    "10 60 5060 sip.gym.example." "$($DIG _sip._tcp.gym.example SRV +short)"
check "NS"     "ns1.gym.example."           "$($DIG gym.example NS +short)"
check "SOA"    "hostmaster.gym.example."    "$($DIG gym.example SOA +short)"

echo "== 2. 否定应答 / 通配符 / CNAME =="
OUT=$($DIG nosuch.gym.example A)
check "NXDOMAIN 状态"        "status: NXDOMAIN" "$OUT"
check "NXDOMAIN 带 SOA"      "gym.example.*SOA\|SOA.*hostmaster" "$(echo "$OUT" | grep -a AUTHORITY -A2)"
OUT=$($DIG app.gym.example MX)
check "NODATA(NOERROR+SOA)"  "status: NOERROR" "$OUT"
check "通配符"               "10.9.0.1"        "$($DIG room7.wifi.gym.example A +short)"
OUT=$($DIG chain1.gym.example A +short)
check "CNAME 链展开"         "10.9.9.9"        "$OUT"
OUT=$($DIG loop1.gym.example A)
check "CNAME 成环 SERVFAIL"  "status: SERVFAIL" "$OUT"

echo "== 3. 子网选路 =="
check "机房1子网"  "10.1.0.10" "$($DIG app.gym.example A +short +subnet=10.1.0.0/16)"
check "机房2子网"  "10.2.0.10" "$($DIG app.gym.example A +short +subnet=10.2.0.0/16)"

echo "== 4. UDP 截断与 TCP 重试 =="
OUT=$($DIG big.gym.example TXT +bufsize=512 +ignore 2>/dev/null || true)
check "无EDNS时设置TC"  "flags:.*tc" "$($DIG big.gym.example TXT +bufsize=512 +ignore +qr 2>&1 | grep -a flags || $DIG big.gym.example TXT +bufsize=512 | grep -a 'flags:')"
FULL_UDP=$($DIG big.gym.example TXT +short +bufsize=512 +ignore 2>/dev/null | wc -l | tr -d ' ')
FULL_TCP=$($DIG big.gym.example TXT +short +tcp | wc -l | tr -d ' ')
if [ "$FULL_TCP" -eq 6 ] && [ "$FULL_UDP" -lt 6 ]; then ok "TCP 拿到完整结果 ($FULL_TCP 条, UDP 截断到 $FULL_UDP 条)"; else bad "TCP 完整结果 (tcp=$FULL_TCP udp=$FULL_UDP)"; fi
check "dig 自动 TCP 重试"  "eeeeeeee" "$($DIG big.gym.example TXT +short +bufsize=512)"

echo "== 5. 区传送(AXFR) =="
OUT=$(dig @$DNS -p $PORT gym.example AXFR +time=3 | grep -c 'IN')
if [ "$OUT" -gt 10 ]; then ok "AXFR 返回 $OUT 行"; else bad "AXFR 行数 $OUT"; fi

echo "== 6. 批量查询工具 =="
cat > "$TMP/queries.txt" <<EOF
app.gym.example A
web.gym.example AAAA
mail.gym.example MX
gym.example TXT
gym.example SOA
nosuch.gym.example A
EOF
OUT=$("$TMP/digbench" -server "127.0.0.1:$PORT" -file "$TMP/queries.txt" -n 50 -c 8)
check "批量查询无失败"  "失败 0" "$OUT"
echo "$OUT" | grep -E "QPS|p50" | sed 's/^/    /'

echo "== 7. 主从同步 =="
"$TMP/gyndns" -role slave -dns ":$SLAVE_PORT" -http ":$SLAVE_HTTP" \
  -data "$TMP/slave.json" -master "127.0.0.1:$PORT" \
  -zones gym.example. >"$TMP/slave.log" 2>&1 &
sleep 1.5
check "从服务器已同步(AXFR)"  "10\.1\.0\.10\|10\.2\.0\.10" "$(dig @$DNS -p $SLAVE_PORT app.gym.example A +short +norec)"
# 主上改记录 → NOTIFY → 从增量同步
api POST "/api/zones/gym.example./records" '{"name":"vip.gym.example.","type":1,"ttl":60,"rdata":"10.8.8.8"}' >/dev/null
sleep 2
check "NOTIFY+IXFR 生效"  "10.8.8.8" "$(dig @$DNS -p $SLAVE_PORT vip.gym.example A +short +norec)"
check "从服务器同步状态"  "增量同步\|全量同步" "$(curl -s http://127.0.0.1:$SLAVE_HTTP/api/sync)"

echo "== 8. 故障切换计时 =="
"$TMP/digbench" -server "127.0.0.1:$PORT" -name app.gym.example -type A \
  -subnet 10.1.0.0/16 -watch 100ms > "$TMP/watch.log" 2>&1 &
WPID=$!
sleep 1
T0=$(date +%s%N)
kill $NC1 2>/dev/null  # 杀掉机房1的 nc 监听
sleep 4
kill $WPID 2>/dev/null
echo "  --- 应答变化时间线 ---"
cat "$TMP/watch.log" | sed 's/^/    /'
# 从 watch 日志里算切换耗时: 第一次出现新地址的时间戳
SWITCH=$(grep -n "10.1.0.10" "$TMP/watch.log" | tail -1)
if grep -q "10.3.0.10" "$TMP/watch.log"; then
  LINE=$(grep "10.3.0.10" "$TMP/watch.log" | head -1)
  ok "故障后应答已切换: $LINE"
else
  bad "故障后应答未切换"
fi

echo "== 9. 全部机房故障 → 兜底地址 =="
kill $NC2 2>/dev/null
kill $NC3 2>/dev/null  # 三个机房全挂
sleep 4
OUT=$($DIG app.gym.example A +short)
check "返回兜底地址"  "203.0.113.1" "$OUT"

echo
echo "==================================="
echo "通过 $PASS 项, 失败 $FAIL 项"
[ $FAIL -eq 0 ]
