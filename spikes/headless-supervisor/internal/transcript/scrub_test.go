package transcript

import (
	"encoding/json"
	"os"
	"os/user"
	"reflect"
	"regexp"
	"testing"
)

func TestScrub(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	name := u.Username
	host, _ := os.Hostname()
	home := Scrubber{Paths: []PathRule{
		{Prefix: "/home/alice", Placeholder: "$HOME"},
		{Prefix: "/home/alice/git/wt", Placeholder: "$WORK"},
	}}

	tests := []struct {
		name string
		s    Scrubber
		in   string
		want string
	}{
		{
			name: "home path becomes $HOME",
			s:    home,
			in:   `{"p":"/home/alice/notes.txt"}`,
			want: `{"p":"$HOME/notes.txt"}`,
		},
		{
			name: "longest prefix wins over home",
			s:    home,
			in:   `{"p":"/home/alice/git/wt/f.go"}`,
			want: `{"p":"$WORK/f.go"}`,
		},
		{
			name: "placeholder not rescanned by a shorter temp rule",
			s: Scrubber{Paths: []PathRule{
				{Prefix: "/tmp/tmp.X/state", Placeholder: "$STATE"},
				{Prefix: "/tmp", Placeholder: "$TMP"},
			}},
			in:   `{"p":"/tmp/tmp.X/state/x"}`,
			want: `{"p":"$STATE/x"}`,
		},
		{
			name: "temp root rewritten once",
			s: Scrubber{Paths: []PathRule{
				{Prefix: "/tmp/tmp.X/state", Placeholder: "$STATE"},
				{Prefix: "/tmp", Placeholder: "$TMP"},
			}},
			in:   `{"p":"/tmp/tmp.Y/other"}`,
			want: `{"p":"$TMP/tmp.Y/other"}`,
		},
		{
			name: "prefix must end on a path boundary",
			s:    Scrubber{Paths: []PathRule{{Prefix: "/tmp", Placeholder: "$TMP"}}},
			in:   `{"p":"/tmpfoo/x"}`,
			want: `{"p":"/tmpfoo/x"}`,
		},
		{
			name: "work and outside stay distinct",
			s: Scrubber{Paths: []PathRule{
				{Prefix: "/w/work", Placeholder: "$WORK"},
				{Prefix: "/w/outside", Placeholder: "$OUTSIDE"},
			}},
			in:   `{"a":"/w/work/f","b":"/w/outside/g"}`,
			want: `{"a":"$WORK/f","b":"$OUTSIDE/g"}`,
		},
		{
			name: "empty prefix rules ignored",
			s:    Scrubber{Paths: []PathRule{{Prefix: "", Placeholder: "$HOME"}}},
			in:   `{"p":"relative/x"}`,
			want: `{"p":"relative/x"}`,
		},
		{
			name: "authStatus dropped",
			s:    home,
			in:   `{"authStatus":"Pro plan","keep":1}`,
			want: `{"keep":1}`,
		},
		{
			name: "token shapes redacted",
			s:    home,
			in:   `{"a":"key sk-ant-api03-AbC_d-9 done","b":"Authorization: Bearer abc.def-ghi"}`,
			want: `{"a":"key [redacted] done","b":"Authorization: Bearer [redacted]"}`,
		},
		{
			name: "secret-named keys redacted",
			s:    home,
			in:   `{"api_key":"x","apiKey":"y","OAUTH_TOKEN":"z","password":"p","client_secret":"s","authorization":"a"}`,
			want: `{"OAUTH_TOKEN":"[redacted]","api_key":"[redacted]","apiKey":"[redacted]","authorization":"[redacted]","client_secret":"[redacted]","password":"[redacted]"}`,
		},
		{
			name: "token counters kept, token credentials redacted",
			s:    home,
			in:   `{"input_tokens":5,"cache_read_input_tokens":2,"tokens":9,"prompt_token_count":3,"token":"t","access_token":"a"}`,
			want: `{"access_token":"[redacted]","cache_read_input_tokens":2,"input_tokens":5,"prompt_token_count":3,"token":"[redacted]","tokens":9}`,
		},
		{
			name: "account, env and _dir keys dropped",
			s:    home,
			in:   `{"account":{"x":1},"env":{"A":"1"},"environment":{},"sessionDir":"/a","x_dir":"/b","keep":1}`,
			want: `{"keep":1}`,
		},
		{
			name: "email shape redacted",
			s:    home,
			in:   `{"m":"mail alice@example.com now"}`,
			want: `{"m":"mail [email] now"}`,
		},
		{
			name: "os username becomes $USER",
			s:    home,
			in:   `{"m":"user ` + name + ` here"}`,
			want: `{"m":"user $USER here"}`,
		},
		{
			name: "claude init reduced",
			s:    home,
			in: `{"type":"system","subtype":"init","session_id":"s1","model":"haiku","permissionMode":"default",` +
				`"claude_code_version":"2.1.0","capabilities":["a"],"cwd":"/home/alice","tools":["Bash","Read"],` +
				`"slash_commands":["x"],"agents":[],"plugins":[],"mcp_servers":[],"apiKeySource":"none"}`,
			want: `{"type":"system","subtype":"init","session_id":"s1","model":"haiku","permissionMode":"default",` +
				`"claude_code_version":"2.1.0","capabilities":["a"],"tools_count":2}`,
		},
		{
			name: "initialize reply reduced",
			s:    home,
			in: `{"type":"control_response","response":{"subtype":"success","request_id":"r1","response":` +
				`{"commands":[{"name":"a"},{"name":"b"}],"account":{"email":"x@y.io"}}}}`,
			want: `{"type":"control_response","response":{"subtype":"success","request_id":"r1"},"commands_count":2}`,
		},
		{
			name: "rate limit info reduced",
			s:    home,
			in:   `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour","resetsAt":1700,"overageStatus":"x"}}`,
			want: `{"type":"rate_limit_event","rate_limit_info":{"rateLimitType":"five_hour","resetsAt":1700,"status":"allowed"}}`,
		},
		{
			name: "more secret key names redacted, counters kept",
			s:    home,
			in:   `{"cookie":"c","db_credential":"d","maxTokens":4,"tokenCount":2,"total_token_count":1}`,
			want: `{"cookie":"[redacted]","db_credential":"[redacted]","maxTokens":4,"tokenCount":2,"total_token_count":1}`,
		},
		{
			name: "secret value shapes redacted",
			s:    home,
			in: `{"a":"sk-or-v1-abc123","b":"sk-proj-ABCDEFGHIJKLMNOPQRSTUVWX","c":"ghp_ABCDEFGHIJKLMNOPQRSTUV",` +
				`"d":"github_pat_11AAA_bbb","e":"AIzaSyA1234567890abcdefghijklmnopqrstu","f":"eyJhbGciOi.eyJzdWIi.sig_-x"}`,
			want: `{"a":"[redacted]","b":"[redacted]","c":"[redacted]","d":"[redacted]","e":"[redacted]","f":"[redacted]"}`,
		},
		{
			name: "env dump lines redacted",
			s:    home,
			in:   `{"o":"PATH=/bin\nANTHROPIC_API_KEY=abc\nGH_TOKEN=x y\nDB_PASSWORD=p"}`,
			want: `{"o":"PATH=/bin\nANTHROPIC_API_KEY=[redacted]\nGH_TOKEN=[redacted]\nDB_PASSWORD=[redacted]"}`,
		},
		{
			name: "hostname becomes $HOST",
			s:    home,
			in:   `{"m":"on ` + host + ` now"}`,
			want: `{"m":"on $HOST now"}`,
		},
		{
			name: "acp available commands dropped",
			s:    home,
			in: `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":` +
				`{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"a"},{"name":"b"}]}}}`,
			want: `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":` +
				`{"sessionUpdate":"available_commands_update","availableCommands":"[dropped: 2 commands]"}}}`,
		},
		{
			name: "acp session result inventory dropped",
			s:    home,
			in: `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"s1","models":{"currentModelId":"m1","availableModels":[{"modelId":"m1"}]},` +
				`"modes":{"currentModeId":"auto","availableModes":[{"id":"auto"}]},"configOptions":[{"id":"x"}]}}`,
			want: `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"s1","currentModelId":"m1","currentModeId":"auto"}}`,
		},
		{
			name: "claude inventory fields replaced by counts",
			s:    home,
			in: `{"type":"system","subtype":"status","slash_commands":["a","b"],"skills":["s"],"agents":[],"plugins":[{"n":1}],` +
				`"mcp_servers":[{"name":"x"}],"commands":[1,2,3]}`,
			want: `{"type":"system","subtype":"status","slash_commands":"[dropped: 2]","skills":"[dropped: 1]","agents":"[dropped: 0]",` +
				`"plugins":"[dropped: 1]","mcp_servers":"[dropped: 1]","commands":"[dropped: 3]"}`,
		},
		{
			name: "numbers keep precision",
			s:    home,
			in:   `{"n":12345678901234567890}`,
			want: `{"n":12345678901234567890}`,
		},
		{
			name: "bare string frame",
			s:    home,
			in:   `"see /home/alice/x"`,
			want: `"see $HOME/x"`,
		},
		{
			name: "sk- inside a word survives",
			s:    home,
			in:   `{"m":"risk-assessment-quarterly-report"}`,
			want: `{"m":"risk-assessment-quarterly-report"}`,
		},
		{
			name: "real-shaped sk- key is redacted",
			s:    home,
			in:   `{"m":"key=sk-abcdefghijklmnopqrstuvwxyz0123 end"}`,
			want: `{"m":"key=[redacted] end"}`,
		},
		{
			name: "github token at string start is redacted",
			s:    home,
			in:   `{"m":"ghp_abcdefghijklmnopqrstuvwxyz0123"}`,
			want: `{"m":"[redacted]"}`,
		},
		{
			name: "path rule needs a boundary before the prefix",
			s:    Scrubber{Paths: []PathRule{{Prefix: "/tmp", Placeholder: "$TMP"}}},
			in:   `{"a":"x/tmp/tmp","b":"/var/tmp/x","c":"/tmp/tmp"}`,
			want: `{"a":"x/tmp/tmp","b":"/var/tmp/x","c":"$TMP/tmp"}`,
		},
		{
			name: "acp rate limit meta reduced",
			s:    home,
			in: `{"method":"session/update","params":{"update":{"_meta":{"_claude/rateLimit":` +
				`{"status":"allowed","rateLimitType":"five_hour","resetsAt":5,"utilization":0.9,"overageStatus":"x"}}}}}`,
			want: `{"method":"session/update","params":{"update":{"_meta":{"_claude/rateLimit":` +
				`{"status":"allowed","rateLimitType":"five_hour","resetsAt":5}}}}}`,
		},
		{
			name: "credential_expires_at dropped",
			s:    home,
			in:   `{"a":{"credential_expires_at":123,"k":1}}`,
			want: `{"a":{"k":1}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.s.Scrub(json.RawMessage(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			assertSameJSON(t, got, tt.want)
		})
	}
}

func TestHostReplacedBeforeUser(t *testing.T) {
	p := pass{
		user: regexp.MustCompile(`\balice\b`),
		host: regexp.MustCompile(`(?i)\balice-laptop\b`),
	}
	if got, want := p.str("alice@alice-laptop alice"), "$USER@$HOST $USER"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestScrubRejectsInvalidJSON(t *testing.T) {
	if _, err := (&Scrubber{}).Scrub(json.RawMessage(`{"a":`)); err == nil {
		t.Fatal("want error for invalid frame")
	}
}

// assertSameJSON compares decoded values so key order and spacing do not matter.
func assertSameJSON(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not JSON: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
