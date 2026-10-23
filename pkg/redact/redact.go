package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type Category string

const (
	CatPhone    Category = "PHONE"
	CatEmail    Category = "EMAIL"
	CatIDCard   Category = "IDCARD"
	CatBankCard Category = "BANKCARD"
	CatIP       Category = "IP"
	CatMAC      Category = "MAC"
	CatJWT      Category = "JWT"
	CatSecret   Category = "SECRET"
)

type detector struct {
	category Category
	priority int
	match    *regexp.Regexp
	validate func(string) bool
}

var detectors = []detector{
	{category: CatSecret, priority: 100, match: regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)},
	{category: CatSecret, priority: 99, match: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{category: CatSecret, priority: 99, match: regexp.MustCompile(`\bg(?:hp|ho|hu|hs|hr)_[A-Za-z0-9]{36}\b`)},
	{category: CatSecret, priority: 99, match: regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`)},
	{category: CatSecret, priority: 99, match: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)},
	{category: CatJWT, priority: 95, match: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)},
	{category: CatIDCard, priority: 90, match: regexp.MustCompile(`\b\d{17}[\dXx]\b`), validate: validIDCard},
	{category: CatBankCard, priority: 89, match: regexp.MustCompile(`\b\d{13,19}\b`), validate: validBankCard},
	{category: CatPhone, priority: 88, match: regexp.MustCompile(`\b1[3-9]\d{9}\b`)},
	{category: CatEmail, priority: 80, match: regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)},
	{category: CatIP, priority: 70, match: regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`), validate: validIPv4},
	{category: CatMAC, priority: 70, match: regexp.MustCompile(`\b[0-9A-Fa-f]{2}(?:[:-][0-9A-Fa-f]{2}){5}\b`)},
}

type Redactor struct {
	reverse map[string]string
}

func New() *Redactor {
	return &Redactor{reverse: map[string]string{}}
}

func (r *Redactor) Mask(s string) string {
	spans := r.spans(s)
	if len(spans) == 0 {
		return s
	}
	var b strings.Builder
	prev := 0
	for _, sp := range spans {
		b.WriteString(s[prev:sp.start])
		b.WriteString(r.placeholder(sp.category, sp.value))
		prev = sp.end
	}
	b.WriteString(s[prev:])
	return b.String()
}

func (r *Redactor) Restore(s string) string {
	if len(r.reverse) == 0 || !strings.ContainsAny(s, "<\\") {
		return s
	}
	for placeholder, value := range r.reverse {
		if strings.Contains(s, placeholder) {
			s = strings.ReplaceAll(s, placeholder, value)
		}
		escaped := `\u003c` + placeholder[1:len(placeholder)-1] + `\u003e`
		if strings.Contains(s, escaped) {
			quoted, _ := json.Marshal(value)
			s = strings.ReplaceAll(s, escaped, string(quoted[1:len(quoted)-1]))
		}
	}
	return s
}

const MaxPlaceholderLen = 64

type Stream struct {
	r       *Redactor
	pending string
}

func (r *Redactor) Stream() *Stream { return &Stream{r: r} }

func (s *Stream) Write(piece string) string {
	s.pending += piece

	cut := len(s.pending)
	if i := strings.LastIndexByte(s.pending, '<'); i >= 0 && !strings.Contains(s.pending[i:], ">") && len(s.pending)-i < MaxPlaceholderLen {
		cut = i
	}
	out := s.r.Restore(s.pending[:cut])
	s.pending = s.pending[cut:]
	return out
}

func (s *Stream) Flush() string {
	out := s.r.Restore(s.pending)
	s.pending = ""
	return out
}

func (r *Redactor) spans(s string) []span {
	var found []span
	for _, d := range detectors {
		for _, loc := range d.match.FindAllStringIndex(s, -1) {
			v := s[loc[0]:loc[1]]
			if d.validate != nil && !d.validate(v) {
				continue
			}
			found = append(found, span{start: loc[0], end: loc[1], priority: d.priority, category: d.category, value: v})
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].start != found[j].start {
			return found[i].start < found[j].start
		}
		if found[i].priority != found[j].priority {
			return found[i].priority > found[j].priority
		}
		return found[i].end > found[j].end
	})
	kept := make([]span, 0, len(found))
	last := -1
	for _, sp := range found {
		if sp.start < last {
			continue
		}
		kept = append(kept, sp)
		last = sp.end
	}
	return kept
}

func (r *Redactor) placeholder(cat Category, value string) string {
	sum := sha256.Sum256([]byte(value))
	hexSum := hex.EncodeToString(sum[:])
	for n := 8; ; n += 4 {
		p := fmt.Sprintf("<%s:%s>", cat, hexSum[:n])
		if existing, ok := r.reverse[p]; !ok || existing == value {
			r.reverse[p] = value
			return p
		}
	}
}

type span struct {
	start, end int
	priority   int
	category   Category
	value      string
}

func validIDCard(s string) bool {
	if len(s) != 18 {
		return false
	}
	weights := [17]int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}
	check := [...]byte{'1', '0', 'X', '9', '8', '7', '6', '5', '4', '3', '2'}
	sum := 0
	for i := 0; i < 17; i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return false
		}
		sum += int(c-'0') * weights[i]
	}
	last := s[17]
	if last >= 'a' && last <= 'z' {
		last = last - 'a' + 'A'
	}
	return check[sum%11] == last
}

func validBankCard(s string) bool {
	sum := 0
	double := false
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		if c < '0' || c > '9' {
			return false
		}
		d := int(c - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

func validIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 || (len(p) > 1 && p[0] == '0') {
			return false
		}
		n := 0
		for _, c := range p {
			n = n*10 + int(c-'0')
		}
		if n > 255 {
			return false
		}
	}
	return true
}
