package zone

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"gyndns/internal/dnsmsg"
)

// ParseZoneFile parses an RFC 1035 master file. Supports $ORIGIN, $TTL,
// @, relative names, parentheses continuation, ';' comments and quoted
// strings. Routing metadata is not represented in the file format.
func ParseZoneFile(r io.Reader) (*Zone, error) {
	tokens, err := tokenize(r)
	if err != nil {
		return nil, err
	}
	var (
		origin   string
		defTTL   uint32 = 300
		prevName string
		z        *Zone
	)
	abs := func(name string) string {
		if name == "@" {
			if origin == "" {
				return "."
			}
			return origin
		}
		if strings.HasSuffix(name, ".") {
			return strings.ToLower(name)
		}
		if origin == "" {
			return strings.ToLower(name) + "."
		}
		return strings.ToLower(name) + "." + origin
	}

	i := 0
	for i < len(tokens) {
		tok := tokens[i]
		i++
		var name string
		switch {
		case strings.HasPrefix(tok, "\x01"): // logical line starting at column 0
			word := tok[1:]
			switch strings.ToUpper(word) {
			case "$ORIGIN":
				if i >= len(tokens) {
					return nil, fmt.Errorf("$ORIGIN without argument")
				}
				origin = abs(tokens[i])
				i++
				continue
			case "$TTL":
				if i >= len(tokens) {
					return nil, fmt.Errorf("$TTL without argument")
				}
				v, err := strconv.ParseUint(tokens[i], 10, 32)
				if err != nil {
					return nil, fmt.Errorf("bad $TTL %q", tokens[i])
				}
				defTTL = uint32(v)
				i++
				continue
			}
			name = abs(word)
		case strings.HasPrefix(tok, "\x00"): // logical line starting with whitespace
			if prevName == "" {
				return nil, fmt.Errorf("record without owner name before any named record")
			}
			name = prevName
			fields := []string{tok[1:]}
			more, ni := collectFields(tokens, i)
			i = ni
			fields = append(fields, more...)
			if err := addRecord(&z, name, fields, defTTL, abs); err != nil {
				return nil, err
			}
			prevName = name
			continue
		default:
			return nil, fmt.Errorf("unexpected token %q", tok)
		}
		fields, ni := collectFields(tokens, i)
		i = ni
		if err := addRecord(&z, name, fields, defTTL, abs); err != nil {
			return nil, err
		}
		prevName = name
	}
	if z == nil || len(z.Records) == 0 {
		return nil, fmt.Errorf("zone file contains no records")
	}
	for _, r := range z.Records {
		if r.Type == dnsmsg.TypeSOA {
			z.Name = r.Name
			break
		}
	}
	if z.Name == "" {
		if origin == "" {
			return nil, fmt.Errorf("cannot determine zone name (no SOA, no $ORIGIN)")
		}
		z.Name = origin
	}
	z.Index()
	return z, nil
}

// collectFields returns the tokens up to the next logical-line start.
func collectFields(tokens []string, i int) ([]string, int) {
	var fields []string
	for i < len(tokens) && !strings.HasPrefix(tokens[i], "\x00") && !strings.HasPrefix(tokens[i], "\x01") {
		fields = append(fields, tokens[i])
		i++
	}
	return fields, i
}

func addRecord(z **Zone, name string, fields []string, defTTL uint32, abs func(string) string) error {
	ttl := defTTL
	var rtype uint16
	rdataStart := -1
	for j := 0; j < len(fields) && j < 3; j++ {
		f := fields[j]
		if v, err := strconv.ParseUint(f, 10, 32); err == nil {
			ttl = uint32(v)
			continue
		}
		if strings.EqualFold(f, "IN") {
			continue
		}
		if t, found := dnsmsg.StringToType(strings.ToUpper(f)); found {
			rtype = t
			rdataStart = j + 1
		}
		break
	}
	if rdataStart < 0 {
		return fmt.Errorf("cannot parse record line: %s %s", name, strings.Join(fields, " "))
	}
	if rdataStart >= len(fields) {
		return fmt.Errorf("record %s missing rdata", name)
	}
	rdataFields := qualifyRDataFields(rtype, fields[rdataStart:], abs)
	if _, err := dnsmsg.ParseRData(rtype, rdataFields); err != nil {
		return fmt.Errorf("record %s: %w", name, err)
	}
	// TXT fields containing spaces must stay quoted in the stored
	// presentation form, otherwise re-parsing splits them apart.
	if rtype == dnsmsg.TypeTXT {
		for i, f := range rdataFields {
			if strings.ContainsAny(f, " \t") && !strings.HasPrefix(f, "\"") {
				rdataFields[i] = "\"" + f + "\""
			}
		}
	}
	if *z == nil {
		*z = &Zone{}
	}
	(*z).Records = append((*z).Records, &Record{
		Name:  name,
		Type:  rtype,
		TTL:   ttl,
		RData: strings.Join(rdataFields, " "),
	})
	return nil
}

// qualifyRDataFields makes domain-name rdata fields absolute.
func qualifyRDataFields(rtype uint16, fields []string, abs func(string) string) []string {
	out := make([]string, len(fields))
	copy(out, fields)
	var nameIdx []int
	switch rtype {
	case dnsmsg.TypeNS, dnsmsg.TypeCNAME, dnsmsg.TypePTR:
		nameIdx = []int{0}
	case dnsmsg.TypeMX:
		nameIdx = []int{1}
	case dnsmsg.TypeSRV:
		nameIdx = []int{3}
	case dnsmsg.TypeSOA:
		nameIdx = []int{0, 1}
	}
	for _, idx := range nameIdx {
		if idx < len(out) {
			out[idx] = abs(out[idx])
		}
	}
	return out
}

// tokenize splits a master file into tokens. Parentheses join physical lines
// into one logical line; ';' starts a comment; double quotes group strings.
// The first token of each logical line is prefixed: "\x01" when the line
// began at column 0, "\x00" when it began with whitespace (owner name
// inherited from the previous record).
func tokenize(r io.Reader) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	inQuote := false
	depth := 0
	lineHasToken := false
	startsWithSpace := false

	flushToken := func() {
		if cur.Len() == 0 {
			return
		}
		tok := cur.String()
		cur.Reset()
		if !lineHasToken {
			if startsWithSpace {
				tok = "\x00" + tok
			} else {
				tok = "\x01" + tok
			}
			lineHasToken = true
		}
		tokens = append(tokens, tok)
	}
	endLine := func() {
		flushToken()
		if depth == 0 {
			lineHasToken = false
			startsWithSpace = false
		}
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !lineHasToken && len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			startsWithSpace = true
		}
		for pos := 0; pos < len(line); pos++ {
			c := line[pos]
			switch {
			case inQuote:
				if c == '"' {
					inQuote = false
				} else {
					cur.WriteByte(c)
				}
			case c == ';':
				break // comment: rest of line ignored; handled by endLine
			case c == '"':
				inQuote = true
			case c == '(':
				flushToken()
				depth++
			case c == ')':
				flushToken()
				if depth > 0 {
					depth--
				}
			case c == ' ' || c == '\t':
				flushToken()
			default:
				cur.WriteByte(c)
			}
			if c == ';' {
				break
			}
		}
		endLine()
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	flushToken()
	return tokens, nil
}

// ExportZoneFile renders the zone as an RFC 1035 master file.
func ExportZoneFile(z *Zone) string {
	var b strings.Builder
	fmt.Fprintf(&b, "$ORIGIN %s\n$TTL 300\n", z.Name)
	recs := z.SortedRecords()
	sort.SliceStable(recs, func(i, j int) bool {
		if (recs[i].Type == dnsmsg.TypeSOA) != (recs[j].Type == dnsmsg.TypeSOA) {
			return recs[i].Type == dnsmsg.TypeSOA
		}
		return false
	})
	for _, r := range recs {
		name := r.Name
		if name == z.Name {
			name = "@"
		}
		if r.Type == dnsmsg.TypeSOA {
			if d, err := dnsmsg.ParseRDataString(dnsmsg.TypeSOA, r.RData); err == nil {
				soa := d.(*dnsmsg.SOA)
				fmt.Fprintf(&b, "%s %d IN SOA %s %s (\n\t\t%d ; serial\n\t\t%d ; refresh\n\t\t%d ; retry\n\t\t%d ; expire\n\t\t%d ; minimum\n)\n",
					name, r.TTL, soa.NS, soa.Mbox, soa.Serial, soa.Refresh, soa.Retry, soa.Expire, soa.MinTTL)
				continue
			}
		}
		fmt.Fprintf(&b, "%s %d IN %s %s\n", name, r.TTL, dnsmsg.TypeString(r.Type), r.RData)
	}
	return b.String()
}
