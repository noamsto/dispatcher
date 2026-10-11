package gitguard

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func fingerprinted(masked, original string) string {
	sum := sha256.Sum256([]byte(original))
	return masked + " [sha256:" + hex.EncodeToString(sum[:])[:8] + "]"
}

func TestShowURL(t *testing.T) {
	// masked "" means the value cannot be masked without hiding the endpoint,
	// so it prints verbatim (Quote) with no fingerprint.
	for _, tc := range []struct {
		in     string
		proxy  bool
		masked string
	}{
		{"https://x-access-token:FAKETOKEN0686@github.com/o/r.git", false, "https://***@github.com/o/r.git"},
		{"http://u:fakepass0686@proxy.example:3128", false, "http://***@proxy.example:3128"},
		{"https://h.example/r?access_token=FAKETOKEN0686&x=1", false, "https://h.example/r?access_token=***&x=***"},
		{"https://h.example/r?FAKETOKEN0686", false, "https://h.example/r?***"},
		{"git@github.com:o/r.git", false, "***@github.com:o/r.git"},

		{"https://github.com/o/r", false, ""},
		{"ssh://[evil.example]:22@github.com/o/r", false, ""},
		{"attacker.example:x@github.com:o/r.git", false, ""},
		{"ssh://evil.example%2f@github.com/o/r.git", false, ""},
		{"ssh://u@github.com?q=@evil.example/o/r.git", false, ""},
		{"https://a@b@github.com/o/r", false, ""},
		{"ext::sh -c FAKETOKEN0686@x", false, ""},
		{"https://h.example/p#frag@x", false, ""},
		{"https://h.example/p@x", false, ""},

		{"u:fakepass0686@proxy.example:3128", true, "***@proxy.example:3128"},
		{"socks5://u:fakepass0686@h/", false, ""},
		{"socks5://u:fakepass0686@h/", true, "socks5://***@h/"},
		{"u@h.example:abc", true, ""},
		{"u@h.example:abc", false, "***@h.example:abc"},

		{"https://user:p%40ssFAKE@h.example/x", false, "https://***@h.example/x"},
		{"u:p%40FAKE@proxy.example:8080", true, "***@proxy.example:8080"},
		{"ssh://evil.example%2f@github.com/o/r", false, ""},

		{"https://\xffé:p@h.example/x", false, "https://***@h.example/x"},
		{"\xffé@h.example:x", false, "***@h.example:x"},
		{"evil\nhttps://u:p@h/", false, ""},
		{"https://u:p@h/\n", false, ""},
		{"HTTPS://U:P@H.example/X", false, "HTTPS://***@H.example/X"},
		{"https://h/?a=1&&b", false, "https://h/?a=***&&***"},
		{"https://h/?&", false, "https://h/?&"},
		{"https://h?x", false, ""},
	} {
		want := Quote(tc.in)
		if tc.masked != "" {
			want = fingerprinted(tc.masked, tc.in)
		}
		if got := ShowURL(tc.in, tc.proxy); got != want {
			t.Errorf("ShowURL(%q, %v) = %q, want %q", tc.in, tc.proxy, got, want)
		}
	}
	if got := ShowURL("https://h.example/p@x", false); got != "https://h.example/p@x" {
		t.Errorf("ShowURL kept no verbatim form: %q", got)
	}
}

func TestShowURLFingerprintsFullValue(t *testing.T) {
	a := ShowURL("https://x:FAKETOKEN0686A@github.com/o/r.git", false)
	b := ShowURL("https://x:FAKETOKEN0686B@github.com/o/r.git", false)
	prefixA, _, _ := strings.Cut(a, " [")
	prefixB, _, _ := strings.Cut(b, " [")
	if prefixA != prefixB || a == b {
		t.Errorf("a changed token must change only the suffix: %q vs %q", a, b)
	}
}

func TestShowKey(t *testing.T) {
	for _, tc := range []struct{ k, want string }{
		{"url.https://FAKETOKEN0686@git.example/.insteadof",
			"url." + fingerprinted("https://***@git.example/", "https://FAKETOKEN0686@git.example/") + ".insteadof"},
		{"credential.https://u:FAKETOKEN0686@h.example.helper",
			"credential." + fingerprinted("https://***@h.example", "https://u:FAKETOKEN0686@h.example") + ".helper"},
		{"diff.x://attacker.example@y.textconv", "diff.x://attacker.example@y.textconv"},
		{"url.ssh://evil.example%2f@github.com/.insteadof", Quote("url.ssh://evil.example%2f@github.com/.insteadof")},
		{"url.https://github.com/.insteadof", "url.https://github.com/.insteadof"},
		{"remote.origin.url", "remote.origin.url"},
		{"url.a b@h/.insteadof", `url.a\ b@h/.insteadof`},
	} {
		if got := ShowKey(tc.k); got != tc.want {
			t.Errorf("ShowKey(%q) = %q, want %q", tc.k, got, tc.want)
		}
	}
}

type pairCase struct {
	k, v    string
	aliases []string
	want    string
}

func TestShowPair(t *testing.T) {
	const remote = "https://evil.example:x@github.com/o/r.git"
	const token = "https://x:FAKETOKEN0686@github.com/o/r.git"
	rows := []pairCase{
		{"http.proxy", "u:fakepass0686@proxy.example:3128", nil,
			"http.proxy=" + fingerprinted("***@proxy.example:3128", "u:fakepass0686@proxy.example:3128")},
		{"http.proxy", "socks5://u:fakepass0686@proxy.example:1080", nil,
			"http.proxy=" + fingerprinted("socks5://***@proxy.example:1080", "socks5://u:fakepass0686@proxy.example:1080")},
		{"credential.helper", "!sh -c x@y", nil, "credential.helper=" + Quote("!sh -c x@y")},
		{"remote.origin.url", remote, nil,
			"remote.origin.url=" + fingerprinted("https://***@github.com/o/r.git", remote)},
		{"remote.origin.url", token, []string{"ssh://github.com/", "https://gitlab.example/"},
			"remote.origin.url=" + fingerprinted("https://***@github.com/o/r.git", token)},
	}
	for _, a := range []string{"", "https://", "https://evil"} {
		aliases := []string{"ssh://other.example/", a}
		rows = append(rows,
			pairCase{"remote.origin.url", remote, aliases, "remote.origin.url=" + Quote(remote)},
			pairCase{"remote.origin.pushurl", remote, aliases, "remote.origin.pushurl=" + Quote(remote)})
	}
	for _, tc := range rows {
		got := ShowPair(tc.k, tc.v, tc.aliases)
		if got != tc.want {
			t.Errorf("ShowPair(%q, %q, %q) = %q, want %q", tc.k, tc.v, tc.aliases, got, tc.want)
		}
		if strings.Contains(got, "fakepass0686") || strings.Contains(got, "FAKETOKEN0686") {
			t.Errorf("ShowPair(%q, %q) leaks the credential: %q", tc.k, tc.v, got)
		}
	}
}
