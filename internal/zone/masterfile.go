package zone

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ParseMaster parses an RFC 1035 master-format zone file into a Zone.
// Supported: $ORIGIN, $TTL, @, relative names, ( ) continuation, ; comments,
// quoted TXT strings, and the pseudo-type ALIAS at the apex.
// Per-record metadata is carried in trailing comments of the form
// "; dc=<name> weight=<n>" and is honored on import.
func ParseMaster(text string) (*Zone, error) {
	type entry struct {
		tokens   []string
		comment  string
		startsWS bool
	}
	var entries []entry
	var cur entry
	depth := 0
	var fb4, fb6 string
	flush := func() {
		if len(cur.tokens) > 0 {
			entries = append(entries, cur)
		}
		cur = entry{}
	}

	for _, line := range strings.Split(text, "\n") {
		// Tokenize the line, honoring quotes and comment start.
		var toks []string
		var sb strings.Builder
		inQuote := false
		hasTok := false
		comment := ""
		push := func() {
			if hasTok {
				toks = append(toks, sb.String())
				sb.Reset()
				hasTok = false
			}
		}
		i := 0
		for i < len(line) {
			c := line[i]
			switch {
			case inQuote:
				if c == '"' {
					inQuote = false
				} else {
					sb.WriteByte(c)
					hasTok = true
				}
			case c == '"':
				inQuote = true
				hasTok = true
			case c == ';':
				comment = strings.TrimSpace(line[i+1:])
				i = len(line)
				continue
			case c == ' ' || c == '\t' || c == '\r':
				push()
			case c == '(':
				push()
				depth++
			case c == ')':
				push()
				depth--
			default:
				sb.WriteByte(c)
				hasTok = true
			}
			i++
		}
		push()
		if len(toks) > 0 && len(cur.tokens) == 0 {
			cur.startsWS = len(line) > 0 && (line[0] == ' ' || line[0] == '\t')
		}
		cur.tokens = append(cur.tokens, toks...)
		if comment != "" {
			cur.comment = comment
			if strings.HasPrefix(comment, "fallback4=") {
				fb4 = strings.TrimPrefix(comment, "fallback4=")
			}
			if strings.HasPrefix(comment, "fallback6=") {
				fb6 = strings.TrimPrefix(comment, "fallback6=")
			}
		}
		if depth == 0 {
			flush()
		}
	}
	flush()

	var z *Zone
	origin := ""
	defTTL := uint32(300)
	lastName := ""

	abs := func(n string) string {
		if n == "@" {
			return origin
		}
		if strings.HasSuffix(n, ".") {
			return strings.TrimSuffix(strings.ToLower(n), ".")
		}
		if origin == "" {
			return strings.ToLower(n)
		}
		return strings.ToLower(n) + "." + origin
	}
	isTTL := func(s string) bool {
		_, err := strconv.ParseUint(s, 10, 32)
		return err == nil
	}

	for _, e := range entries {
		toks := e.tokens
		if len(toks) == 0 {
			continue
		}
		switch strings.ToUpper(toks[0]) {
		case "$ORIGIN":
			if len(toks) < 2 {
				return nil, fmt.Errorf("$ORIGIN needs an argument")
			}
			origin = strings.TrimSuffix(strings.ToLower(toks[1]), ".")
			if z == nil {
				z = NewZone(origin)
			}
			continue
		case "$TTL":
			if len(toks) < 2 {
				return nil, fmt.Errorf("$TTL needs an argument")
			}
			v, err := strconv.ParseUint(toks[1], 10, 32)
			if err != nil {
				return nil, fmt.Errorf("bad $TTL: %v", err)
			}
			defTTL = uint32(v)
			if z != nil {
				z.DefaultTTL = defTTL
			}
			continue
		}
		if z == nil {
			return nil, fmt.Errorf("record before $ORIGIN: %q", strings.Join(toks, " "))
		}

		// [name] [ttl] [IN] TYPE rdata...
		i := 0
		name := ""
		if !e.startsWS {
			name = abs(toks[0])
			i++
		} else {
			name = lastName
		}
		ttl := uint32(0)
		if i < len(toks) && isTTL(toks[i]) {
			v, _ := strconv.ParseUint(toks[i], 10, 32)
			ttl = uint32(v)
			i++
		}
		if i < len(toks) && strings.EqualFold(toks[i], "IN") {
			i++
		}
		if i < len(toks) && ttl == 0 && isTTL(toks[i]) { // ttl after class
			v, _ := strconv.ParseUint(toks[i], 10, 32)
			ttl = uint32(v)
			i++
		}
		if i >= len(toks) {
			return nil, fmt.Errorf("missing type in %q", strings.Join(toks, " "))
		}
		typ := strings.ToUpper(toks[i])
		i++
		rdata := toks[i:]
		if name == "" {
			return nil, fmt.Errorf("no owner name for %q", strings.Join(toks, " "))
		}
		lastName = name

		if typ == "SOA" {
			if len(rdata) < 7 {
				return nil, fmt.Errorf("SOA needs 7 rdata fields")
			}
			nums := make([]uint32, 5)
			for k := 0; k < 5; k++ {
				v, err := strconv.ParseUint(rdata[2+k], 10, 32)
				if err != nil {
					return nil, fmt.Errorf("bad SOA field: %v", err)
				}
				nums[k] = uint32(v)
			}
			z.SOA = SOA{
				MName: abs(rdata[0]) + ".", RName: abs(rdata[1]) + ".",
				Serial: nums[0], Refresh: nums[1], Retry: nums[2],
				Expire: nums[3], Minimum: nums[4],
				TTL: ttlOr(ttl, defTTL),
			}
			continue
		}
		if typ == "ALIAS" {
			if len(rdata) != 1 {
				return nil, fmt.Errorf("ALIAS needs exactly one target")
			}
			z.Records[name] = append(z.Records[name], Record{
				Name: name, Type: TypeALIAS, TTL: ttlOr(ttl, 0), Value: abs(rdata[0]) + ".",
			})
			continue
		}
		if _, ok := typeRdataFmt[typ]; !ok {
			return nil, fmt.Errorf("unsupported type %q", typ)
		}
		val, err := formatRdata(typ, rdata, abs)
		if err != nil {
			return nil, err
		}
		rec := Record{Name: name, Type: typ, TTL: ttl, Value: val}
		// Parse metadata comment: "dc=sh weight=10"
		for _, f := range strings.Fields(e.comment) {
			if strings.HasPrefix(f, "dc=") {
				rec.DC = strings.TrimPrefix(f, "dc=")
			}
			if strings.HasPrefix(f, "weight=") {
				if w, err := strconv.Atoi(strings.TrimPrefix(f, "weight=")); err == nil {
					rec.Weight = w
				}
			}
		}
		z.Records[name] = append(z.Records[name], rec)
	}
	if z == nil {
		return nil, fmt.Errorf("empty zone file (no $ORIGIN)")
	}
	if z.DefaultTTL == 0 {
		z.DefaultTTL = defTTL
	}
	z.Fallback4 = fb4
	z.Fallback6 = fb6
	return z, nil
}

func ttlOr(v, def uint32) uint32 {
	if v == 0 {
		return def
	}
	return v
}

// typeRdataFmt lists types we can parse from master files.
var typeRdataFmt = map[string]int{ // type -> fixed field count (0 = rest-of-line)
	"A": 1, "AAAA": 1, "CNAME": 1, "NS": 1, "PTR": 1,
	"MX": 2, "SRV": 4, "TXT": 0,
}

func formatRdata(typ string, f []string, abs func(string) string) (string, error) {
	need := typeRdataFmt[typ]
	if need > 0 && len(f) != need {
		return "", fmt.Errorf("%s needs %d rdata fields, got %d", typ, need, len(f))
	}
	switch typ {
	case "CNAME", "NS", "PTR":
		return abs(f[0]) + ".", nil
	case "MX":
		return f[0] + " " + abs(f[1]) + ".", nil
	case "SRV":
		return strings.Join(f[:3], " ") + " " + abs(f[3]) + ".", nil
	case "TXT":
		if len(f) == 0 {
			return "", fmt.Errorf("TXT needs rdata")
		}
		return strings.Join(f, " "), nil
	default:
		return f[0], nil
	}
}

// ExportMaster renders a zone in RFC 1035 master format. Record metadata
// (dc, weight) is appended as a trailing comment so it round-trips.
func ExportMaster(z *Zone) string {
	var b strings.Builder
	fmt.Fprintf(&b, "$ORIGIN %s.\n$TTL %d\n", z.Name, z.DefaultTTL)
	s := z.SOA
	fmt.Fprintf(&b, "@ %d IN SOA %s %s (\n", ttlOr(s.TTL, z.DefaultTTL), s.MName, s.RName)
	fmt.Fprintf(&b, "    %d ; serial\n    %d ; refresh\n    %d ; retry\n    %d ; expire\n    %d ; minimum\n)\n",
		s.Serial, s.Refresh, s.Retry, s.Expire, s.Minimum)
	if z.Fallback4 != "" {
		fmt.Fprintf(&b, "; fallback4=%s\n", z.Fallback4)
	}
	if z.Fallback6 != "" {
		fmt.Fprintf(&b, "; fallback6=%s\n", z.Fallback6)
	}

	names := make([]string, 0, len(z.Records))
	for n := range z.Records {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		owner := n
		if n == z.Name {
			owner = "@"
		} else {
			owner = strings.TrimSuffix(n, "."+z.Name)
		}
		for _, r := range z.Records[n] {
			ttl := r.TTL
			if ttl == 0 {
				ttl = z.DefaultTTL
			}
			meta := ""
			if r.DC != "" {
				meta += " dc=" + r.DC
			}
			if r.Weight > 0 {
				meta += fmt.Sprintf(" weight=%d", r.Weight)
			}
			if meta != "" {
				meta = " ;" + meta
			}
			val := r.Value
			if r.Type == "TXT" && !strings.HasPrefix(val, `"`) {
				val = `"` + strings.ReplaceAll(val, `"`, `\"`) + `"`
			}
			fmt.Fprintf(&b, "%s %d IN %s %s%s\n", owner, ttl, r.Type, val, meta)
		}
	}
	return b.String()
}
