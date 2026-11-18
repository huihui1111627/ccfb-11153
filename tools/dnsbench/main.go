// Command dnsbench is a batch DNS query tool used to test dnsd:
// it fires query lists at high concurrency, validates that responses
// parse, tallies rcodes, and in -watch mode measures how fast the
// answer set changes (failover switch time).
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dnsd/internal/dnsmsg"
)

type query struct {
	name  string
	qtype uint16
}

func main() {
	server := flag.String("server", "127.0.0.1:5354", "dns server address")
	file := flag.String("file", "", "query list file: lines of \"name type\"")
	conc := flag.Int("c", 10, "concurrency")
	count := flag.Int("n", 100, "total queries (batch mode)")
	useTCP := flag.Bool("tcp", false, "use TCP")
	watch := flag.String("watch", "", "watch this name for answer changes")
	wtype := flag.String("type", "A", "query type for -watch")
	interval := flag.Duration("interval", 100*time.Millisecond, "watch interval")
	timeout := flag.Duration("timeout", 2*time.Second, "per-query timeout")
	flag.Parse()

	if *watch != "" {
		watchMode(*server, *watch, *wtype, *interval, *timeout, *useTCP)
		return
	}
	if *file == "" {
		fmt.Fprintln(os.Stderr, "need -file or -watch")
		os.Exit(2)
	}
	qs, err := loadQueries(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	batchMode(*server, qs, *count, *conc, *timeout, *useTCP)
}

func loadQueries(path string) ([]query, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var qs []query
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || strings.HasPrefix(line, "#") {
			continue
		}
		t, ok := dnsmsg.NameToType[strings.ToUpper(f[1])]
		if !ok {
			return nil, fmt.Errorf("unknown type in line %q", line)
		}
		qs = append(qs, query{f[0], t})
	}
	if len(qs) == 0 {
		return nil, fmt.Errorf("no queries in %s", path)
	}
	return qs, nil
}

func exchange(server string, q query, id uint16, timeout time.Duration, useTCP bool) (*dnsmsg.Message, time.Duration, error) {
	req := &dnsmsg.Message{
		Header:    dnsmsg.Header{ID: id, RD: true},
		Questions: []dnsmsg.Question{{Name: dnsmsg.Normalize(q.name), Qtype: q.qtype, Qclass: dnsmsg.ClassIN}},
	}
	req.EDNSSize = 1232
	wire, err := req.Pack(true)
	if err != nil {
		return nil, 0, err
	}
	start := time.Now()
	proto := "udp"
	if useTCP {
		proto = "tcp"
	}
	conn, err := net.DialTimeout(proto, server, timeout)
	if err != nil {
		return nil, 0, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	if useTCP {
		wire = append([]byte{byte(len(wire) >> 8), byte(len(wire))}, wire...)
	}
	if _, err := conn.Write(wire); err != nil {
		return nil, 0, err
	}
	var raw []byte
	if useTCP {
		hdr := make([]byte, 2)
		if _, err := readFull(conn, hdr); err != nil {
			return nil, 0, err
		}
		raw = make([]byte, int(hdr[0])<<8|int(hdr[1]))
		if _, err := readFull(conn, raw); err != nil {
			return nil, 0, err
		}
	} else {
		buf := make([]byte, 65535)
		n, err := conn.Read(buf)
		if err != nil {
			return nil, 0, err
		}
		raw = buf[:n]
	}
	el := time.Since(start)
	msg, err := dnsmsg.Unpack(raw)
	if err != nil {
		return nil, el, fmt.Errorf("response did not parse: %v", err)
	}
	if msg.ID != req.ID {
		return nil, el, fmt.Errorf("id mismatch")
	}
	return msg, el, nil
}

func readFull(c net.Conn, b []byte) (int, error) {
	got := 0
	for got < len(b) {
		n, err := c.Read(b[got:])
		got += n
		if err != nil {
			return got, err
		}
	}
	return got, nil
}

func batchMode(server string, qs []query, total, conc int, timeout time.Duration, useTCP bool) {
	var sent, ok, failed, truncated int64
	rcodes := map[uint8]*int64{}
	var mu sync.Mutex
	rc := func(c uint8) *int64 {
		mu.Lock()
		defer mu.Unlock()
		if rcodes[c] == nil {
			rcodes[c] = new(int64)
		}
		return rcodes[c]
	}
	var latSum, latMax int64 // microseconds

	jobs := make(chan int, conc)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				q := qs[i%len(qs)]
				msg, el, err := exchange(server, q, uint16(i+1), timeout, useTCP)
				atomic.AddInt64(&sent, 1)
				if err != nil {
					atomic.AddInt64(&failed, 1)
					fmt.Printf("FAIL %s %s: %v\n", q.name, dnsmsg.TypeString(q.qtype), err)
					continue
				}
				atomic.AddInt64(&ok, 1)
				atomic.AddInt64(rc(msg.Rcode), 1)
				if msg.TC {
					atomic.AddInt64(&truncated, 1)
				}
				us := el.Microseconds()
				atomic.AddInt64(&latSum, us)
				for {
					old := atomic.LoadInt64(&latMax)
					if us <= old || atomic.CompareAndSwapInt64(&latMax, old, us) {
						break
					}
				}
			}
		}()
	}
	for i := 0; i < total; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	el := time.Since(start)

	fmt.Printf("\n=== %d queries in %v (%.0f qps) ===\n", sent, el.Round(time.Millisecond), float64(sent)/el.Seconds())
	fmt.Printf("ok=%d failed=%d truncated=%d\n", ok, failed, truncated)
	keys := make([]int, 0, len(rcodes))
	for c := range rcodes {
		keys = append(keys, int(c))
	}
	sort.Ints(keys)
	for _, c := range keys {
		fmt.Printf("rcode %d: %d\n", c, atomic.LoadInt64(rcodes[uint8(c)]))
	}
	if ok > 0 {
		fmt.Printf("latency avg=%.2fms max=%.2fms\n",
			float64(latSum)/float64(ok)/1000.0, float64(latMax)/1000.0)
	}
}

// watchMode polls one name and reports exactly when the answer set changes,
// which measures failure-detection-to-DNS-switch latency.
func watchMode(server, name, typ string, interval, timeout time.Duration, useTCP bool) {
	t, ok := dnsmsg.NameToType[strings.ToUpper(typ)]
	if !ok {
		fmt.Fprintln(os.Stderr, "unknown type", typ)
		os.Exit(2)
	}
	q := query{name, t}
	fmt.Printf("watching %s %s every %v — take an endpoint down and watch the switch time\n", name, typ, interval)
	prev := ""
	prevSince := time.Now()
	var id uint16
	for {
		id++
		msg, _, err := exchange(server, q, id, timeout, useTCP)
		var answers []string
		if err != nil {
			answers = []string{"<error: " + err.Error() + ">"}
		} else {
			for _, rr := range msg.Answers {
				answers = append(answers, rr.String())
			}
			if len(answers) == 0 {
				mark := ""
				if msg.TC {
					mark = " TC(truncated/slipped)"
				}
				answers = []string{fmt.Sprintf("<empty rcode=%d%s>", msg.Rcode, mark)}
			}
			sort.Strings(answers)
		}
		cur := strings.Join(answers, "\n  ")
		if cur != prev {
			now := time.Now()
			if prev != "" {
				fmt.Printf("[%s] ANSWER CHANGED after %v in previous state:\n  %s\n",
					now.Format("15:04:05.000"), now.Sub(prevSince).Round(time.Millisecond), cur)
			} else {
				fmt.Printf("[%s] initial:\n  %s\n", now.Format("15:04:05.000"), cur)
			}
			prev = cur
			prevSince = now
		}
		time.Sleep(interval)
	}
}
