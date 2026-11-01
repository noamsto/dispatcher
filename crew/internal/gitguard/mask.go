package gitguard

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// The classes keep bash's C-locale reading: a byte >= 0x80 is neither space nor
// cntrl, so the negated classes accept it — as Go does, matching it as a rune
// or as U+FFFD. Perl-mode compilation keeps ^ and $ on the whole string, as
// glibc's ERE does; CompilePOSIX would make them line anchors.
const (
	userChar    = `[^\]@/?#\[\\[:space:][:cntrl:]]`
	scpUserChar = `[^\]@/?#\[\\%:[:space:][:cntrl:]]`
	pathChar    = `[A-Za-z0-9._~!$&'()*+,;=:@%/-]`
)

var (
	urlRE = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*)://((` + userChar + `*)@)?([A-Za-z0-9.-]+)(:[0-9]+)?((/` +
		pathChar + `*)(\?(` + pathChar + `|\?)*)?)?$`)
	scpRE = regexp.MustCompile(`^(` + scpUserChar + `+)@([A-Za-z0-9.-]+):(` + pathChar + `*)$`)
)

// ShowURL renders s with URL userinfo and query values masked and a
// fingerprint of the full value appended, or Quote(s) when s is not a URL it
// can mask without hiding the endpoint. proxy reads s as a proxy value: a
// scheme-less value is http://, socks schemes are allowed, scp form is not.
func ShowURL(s string, proxy bool) string {
	if !strings.ContainsAny(s, "@?") {
		return Quote(s)
	}
	masked, ok := maskURL(s, proxy)
	if !ok {
		return Quote(s)
	}
	return masked + " [sha256:" + fingerprint(s) + "]"
}

func fingerprint(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

func maskURL(s string, proxy bool) (string, bool) {
	str, pre := s, ""
	// git and curl read a scheme-less proxy as http://<value>.
	if proxy && !strings.Contains(s, "://") {
		pre = "http://"
		str = pre + s
	}
	if m := urlRE.FindStringSubmatch(str); m != nil {
		switch strings.ToLower(m[1]) {
		case "http", "https", "ftp", "ftps":
		case "ssh", "git+ssh", "ssh+git":
			// git url_decode()s an ssh url before splitting off the host.
			if strings.Contains(m[3], "%") {
				return "", false
			}
		case "socks4", "socks4a", "socks5", "socks5h":
			if !proxy {
				return "", false
			}
		default:
			return "", false
		}
		query := strings.TrimPrefix(m[8], "?")
		if m[2] == "" && query == "" {
			return "", false
		}
		var b strings.Builder
		b.WriteString(m[1] + "://")
		if m[2] != "" {
			b.WriteString("***@")
		}
		b.WriteString(m[4] + m[5] + m[7])
		if query != "" {
			b.WriteByte('?')
			for {
				part, rest, more := strings.Cut(query, "&")
				if k, _, ok := strings.Cut(part, "="); ok {
					b.WriteString(k + "=***")
				} else if part != "" {
					b.WriteString("***")
				}
				if !more {
					break
				}
				query = rest
				b.WriteByte('&')
			}
		}
		return strings.TrimPrefix(b.String(), pre), true
	}
	if !proxy {
		if m := scpRE.FindStringSubmatch(s); m != nil {
			return "***@" + m[2] + ":" + m[3], true
		}
	}
	return "", false
}

// ShowKey renders k, its URL subsection masked for the keys that embed one. An
// unmaskable subsection that may carry userinfo or a query is withheld behind
// its fingerprint.
func ShowKey(k string) string {
	kd, _ := showKey(k)
	return kd
}

// showKey is ShowKey and whether the subsection was withheld.
func showKey(k string) (string, bool) {
	if !match(urlSubsectionKeys, k, "") {
		return Quote(k), false
	}
	sec, _, _ := strings.Cut(k, ".")
	variable := k[strings.LastIndexByte(k, '.')+1:]
	sub := strings.TrimSuffix(strings.TrimPrefix(k, sec+"."), "."+variable)
	masked := ShowURL(sub, false)
	if masked != Quote(sub) {
		return sec + "." + masked + "." + variable, false
	}
	if !strings.ContainsAny(sub, "@?") {
		return Quote(k), false
	}
	return sec + ".<url withheld sha256:" + fingerprint(sub) + ">." + variable, true
}

// Rewritten reports whether an insteadOf alias prefixes url.
func Rewritten(url string, aliases []string) bool {
	for _, a := range aliases {
		if strings.HasPrefix(url, a) {
			return true
		}
	}
	return false
}

// ShowPair renders `key=value`, masking a redirect value's URL credentials
// unless one of aliases rewrites it.
func ShowPair(k, v string, aliases []string) string {
	val := Quote(v)
	if IsRedirect(k, v) {
		switch {
		case globAny(k, "http.proxy", "http.*.proxy", "remote.*.proxy"):
			val = ShowURL(v, true)
		// git contacts the rewritten url, whose host the mask could hide.
		case globAny(k, "remote.*.url", "remote.*.pushurl") && Rewritten(v, aliases):
		default:
			val = ShowURL(v, false)
		}
	}
	return ShowKey(k) + "=" + val
}

func globAny(s string, patterns ...string) bool {
	for _, p := range patterns {
		if glob(p, s) {
			return true
		}
	}
	return false
}
