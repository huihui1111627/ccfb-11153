// digbench is a batch DNS query tool: load testing, correctness checks
// against expected answers, and failover switch-time measurement.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gyndns/internal/dnsmsg"
)

var (
	server  = flag.String("server", "127.0.0.1:5353", "DNS server address")
	file    = flag.String("file", "", "query file: lines of 'name type [client-subnet-ip]'")
	name    = flag.String("name", "", "single query name")
	qtype   = flag.String("type", "A", "single query type")
	subnet  = flag.String("subnet", "", "EDNS client subnet to send, e.g. 10.1.0.0/16")
	n       = flag.Int("n", 1, "queries per name (batch mode)")
	c       = flag.Int("c", 1, "concurrency")
	tcp     = flag.Bool("tcp", false, "use TCP")
	bufsize = flag.Int("bufsize", 0, "EDNS UDP size to advertise (0 = no EDNS)")
	expect  = flag.String("expect", "", "comma separated rdata the answer must contain (check mode)")
	watch   = flag.Duration("watch", 0, "failover watch mode: poll interval, report answer-set changes")
	timeout = flag.Duration("timeout", 2*time.Second, "query timeout")
)

type query struct {
	name   string
	qtype  uint16
	subnet string
}

func main() {
	flag.Parse()
	if *watch > 0 {
		watchMode()
		return
	}
	var queries []query
	if *file != "" {
		f, err := os.Open(*file)
		if err != nil {
			fatal(err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.Fields(line)
			if len(parts) < 2 {
				continue
			}
			t, ok := dnsmsg.StringToType(strings.ToUpper(parts[1]))
			if !ok {
				fatal(fmt.Errorf("unknown type %q", parts[1]))
			}
			q := query{name: dnsmsg.Fqdn(parts[0]), qtype: t}
			if len(parts) > 2 {
				q.subnet = parts[2]
			}
			queries = append(queries, q)
		}
		f.Close()
	} else if *name != "" {
		t, ok := dnsmsg.StringToType(strings.ToUpper(*qtype))
		if !ok {
			fatal(fmt.Errorf("unknown type %q", *qtype))
		}
		queries = []query{{name: dnsmsg.Fqdn(*name), qtype: t, subnet: *subnet}}
	} else {
		fatal(fmt.Errorf("give -file or -name"))
	}

	var (
		total  int64
		failed int64
		bad    int64
		latMu  sync.Mutex
		lat    []time.Duration
		rcodes = map[string]*int64{}
		rcMu   sync.Mutex
	)
	work := make(chan query)
	var wg sync.WaitGroup
	for i := 0; i < *c; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for q := range work {
				for j := 0; j < *n; j++ {
					resp, d, err := doQuery(q)
					atomic.AddInt64(&total, 1)
					latMu.Lock()
					lat = append(lat, d)
					latMu.Unlock()
					if err != nil {
						atomic.AddInt64(&failed, 1)
						fmt.Fprintf(os.Stderr, "FAIL %s %s: %v\n", q.name, dnsmsg.TypeString(q.qtype), err)
						continue
					}
					rcMu.Lock()
					cnt, ok := rcodes[dnsmsg.RcodeString(resp.RCode)]
					if !ok {
						cnt = new(int64)
						rcodes[dnsmsg.RcodeString(resp.RCode)] = cnt
					}
					atomic.AddInt64(cnt, 1)
					rcMu.Unlock()
					if *expect != "" && !checkExpect(resp, *expect) {
						atomic.AddInt64(&bad, 1)
						fmt.Fprintf(os.Stderr, "BAD %s %s: want one of [%s], got %v\n",
							q.name, dnsmsg.TypeString(q.qtype), *expect, answerStrings(resp))
					}
				}
			}
		}()
	}
	start := time.Now()
	for _, q := range queries {
		work <- q
	}
	close(work)
	wg.Wait()
	elapsed := time.Since(start)

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p := func(q float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		return lat[int(float64(len(lat)-1)*q)]
	}
	t := atomic.LoadInt64(&total)
	fmt.Printf("\n== 统计 ==\n")
	fmt.Printf("总查询 %d, 失败 %d, 应答不符 %d, 耗时 %s, QPS %.0f\n",
		t, atomic.LoadInt64(&failed), atomic.LoadInt64(&bad), elapsed.Round(time.Millisecond),
		float64(t)/elapsed.Seconds())
	if len(lat) > 0 {
		fmt.Printf("延迟 p50=%s p95=%s p99=%s max=%s\n", p(0.5), p(0.95), p(0.99), lat[len(lat)-1])
	}
	for rc, cnt := range rcodes {
		fmt.Printf("rcode %s: %d\n", rc, atomic.LoadInt64(cnt))
	}
	if atomic.LoadInt64(&failed)+atomic.LoadInt64(&bad) > 0 {
		os.Exit(1)
	}
}

// watchMode polls one name and reports exactly when the answer set changes
// (failover switch-time measurement).
func watchMode() {
	if *name == "" {
		fatal(fmt.Errorf("watch mode needs -name"))
	}
	t, ok := dnsmsg.StringToType(strings.ToUpper(*qtype))
	if !ok {
		fatal(fmt.Errorf("unknown type"))
	}
	q := query{name: dnsmsg.Fqdn(*name), qtype: t, subnet: *subnet}
	start := time.Now()
	var prev string
	fmt.Printf("开始监视 %s %s, 间隔 %s\n", q.name, *qtype, *watch)
	for {
		resp, _, err := doQuery(q)
		var cur string
		if err != nil {
			cur = "ERROR: " + err.Error()
		} else {
			cur = fmt.Sprintf("%s %s", dnsmsg.RcodeString(resp.RCode), strings.Join(answerStrings(resp), ","))
		}
		if cur != prev {
			fmt.Printf("[%8.3fs] %s\n", time.Since(start).Seconds(), cur)
			prev = cur
		}
		time.Sleep(*watch)
	}
}

func doQuery(q query) (*dnsmsg.Message, time.Duration, error) {
	m := &dnsmsg.Message{
		Header:    dnsmsg.Header{ID: uint16(rand.Uint32()), RD: true},
		Questions: []dnsmsg.Question{{Name: q.name, Type: q.qtype, Class: dnsmsg.ClassIN}},
	}
	sub := q.subnet
	if sub == "" {
		sub = *subnet
	}
	if *bufsize > 0 || sub != "" {
		size := *bufsize
		if size == 0 {
			size = 1232
		}
		m.SetEDNS(uint16(size))
	}
	if sub != "" {
		ip, ipnet, err := net.ParseCIDR(sub)
		if err != nil {
			return nil, 0, fmt.Errorf("bad subnet: %w", err)
		}
		ones, _ := ipnet.Mask.Size()
		var fam uint16 = 1
		addr := ip.To4()
		if addr == nil {
			fam = 2
			addr = ip.To16()
		}
		nbytes := (ones + 7) / 8
		data := []byte{byte(fam >> 8), byte(fam), byte(ones), 0}
		data = append(data, addr[:nbytes]...)
		opt := m.Additionals[len(m.Additionals)-1].Data.(*dnsmsg.OPT)
		opt.Options = append(opt.Options, dnsmsg.EDNSOption{Code: dnsmsg.EDNSOptionECS, Data: data})
	}
	buf, err := m.Pack(0)
	if err != nil {
		return nil, 0, err
	}
	start := time.Now()
	if *tcp {
		return queryTCP(buf, start)
	}
	return queryUDP(buf, start)
}

func queryUDP(buf []byte, start time.Time) (*dnsmsg.Message, time.Duration, error) {
	conn, err := net.DialTimeout("udp", *server, *timeout)
	if err != nil {
		return nil, 0, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(*timeout))
	if _, err := conn.Write(buf); err != nil {
		return nil, 0, err
	}
	resp := make([]byte, 65535)
	n, err := conn.Read(resp)
	d := time.Since(start)
	if err != nil {
		return nil, d, err
	}
	m, err := dnsmsg.Unpack(resp[:n])
	return m, d, err
}

func queryTCP(buf []byte, start time.Time) (*dnsmsg.Message, time.Duration, error) {
	conn, err := net.DialTimeout("tcp", *server, *timeout)
	if err != nil {
		return nil, 0, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(*timeout))
	out := append([]byte{byte(len(buf) >> 8), byte(len(buf))}, buf...)
	if _, err := conn.Write(out); err != nil {
		return nil, 0, err
	}
	var lbuf [2]byte
	if _, err := readFull(conn, lbuf[:]); err != nil {
		return nil, 0, err
	}
	resp := make([]byte, int(lbuf[0])<<8|int(lbuf[1]))
	if _, err := readFull(conn, resp); err != nil {
		return nil, 0, err
	}
	d := time.Since(start)
	m, err := dnsmsg.Unpack(resp)
	return m, d, err
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := conn.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func answerStrings(m *dnsmsg.Message) []string {
	var out []string
	for _, a := range m.Answers {
		out = append(out, a.Data.String())
	}
	sort.Strings(out)
	return out
}

func checkExpect(m *dnsmsg.Message, expect string) bool {
	want := map[string]bool{}
	for _, e := range strings.Split(expect, ",") {
		want[strings.TrimSpace(e)] = true
	}
	for _, a := range m.Answers {
		if want[a.Data.String()] {
			return true
		}
	}
	return false
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "digbench:", err)
	os.Exit(2)
}
