# Removing internal/jsonv

`internal/jsonv` is an ordered JSON value type with jq 1.8.2 semantics: a
stream decoder with NaN/Infinity extensions, number-literal text, jq-style
duplicate-key and invalid-UTF-8 handling, byte-identical compact and pretty
output, and `$JQ_COLORS` palettes. It exists for byte equality with the bash
`crew`. The contract is now value-equal JSON plus exact exit codes and
agent-facing text (#821, `docs/crew-go-port.md` "Output contract"), and
`crew.sh` is gone. The goal (#1043) is to replace jsonv with `encoding/json`
or typed structs wherever byte-exact jq formatting was its only reason, and to
keep it (or a much smaller replacement) only where its semantics are
load-bearing or it bridges into gojq (`internal/jqrun`).

This doc is the decomposition: what each importer needs, what breaks on a
switch, and the order to convert in. Line numbers are as of the commit that
introduced this doc.

## Classes

- (a) bytes only: jsonv builds or prints a row or file whose consumers read
  it by value. Replace with a struct and `encoding/json`.
- (b) semantics: the code depends on a decode or compare behaviour that
  `encoding/json` lacks (NaN, literal text, torn tail, surrogate rule, key
  order inside a string).
- (c) gojq bridge: the value only crosses `jqrun.Run`/`Compile`/`Program.Run`
  or a seam typed `jsonv.Value` (`bus.Event`, `probe.BusRows`, `marks`, ...).
  Follows the seam, no semantics of its own.
- (t) test support: exists for tests only (`testjson`).

A file with several classes lists the dominant one first.

## Semantics measured

Measured with a throwaway test against `encoding/json` (Unmarshal into `any`,
Decoder with and without `UseNumber`, Marshal, Encoder with
`SetEscapeHTML(false)`), then deleted. "Go" below is `encoding/json`.

| semantic                                                                   | jsonv                                                                    | Go                                                                                                                                          | value-equal?                                       | pinned by                                                                                                                                                                                                                                                      |
| -------------------------------------------------------------------------- | ------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `NaN`, `nan`, `-nan` literal                                               | parses to float NaN, prints `null`                                       | syntax error, 0 values                                                                                                                      | no (accept vs reject)                              | status `TestFoldsMatchJQ` corpus (vs real jq); inbox `TestSince` "NaN never", `TestGreaterThanNumber`; jqrun `TestToGoNaNStaysFloat`, `TestComputedNumbersStayComputed`, `TestJQFromJSON`; roster `TestFold` (detail `NaN`); sessions `TestFold` "NaN ts keys" |
| `NaN1`                                                                     | error                                                                    | error                                                                                                                                       | yes (both reject)                                  | status diff corpus `nan1`; jsonv `TestDecodeStream`                                                                                                                                                                                                            |
| `Infinity`, `-Infinity`                                                    | parses, prints `+-1.7976931348623157e+308`                               | syntax error                                                                                                                                | no                                                 | status diff corpus; inbox `TestSince` "Infinity always"; roster `TestFold` (`tier`/`ts` Infinity); sessions `TestFold` (age_s clamp)                                                                                                                           |
| big integer `18446744073709551617`                                         | verbatim                                                                 | `18446744073709552000` (float64); verbatim only with `UseNumber`                                                                            | no, unless `UseNumber`                             | inbox `TestSince`, `TestPrintsTheRowUnchanged`; log `TestFiltersOneCrewByteForByte`; jqrun `TestNumberLiteralsRoundTripExactly`; sessions `TestFold` ("big literal ts")                                                                                        |
| `1E+1000`, `100000000000000000000000000001`                                | verbatim                                                                 | error (`cannot unmarshal ... float64`) or `1e+29`; verbatim with `UseNumber`                                                                | no, unless `UseNumber`                             | sessions `TestFold` "huge ts"; inbox `TestSince` "huge exponent"; status diff corpus                                                                                                                                                                           |
| `1.0`, `1.50`, `-0.0`                                                      | verbatim                                                                 | `1`, `1.5`, `-0`; verbatim with `UseNumber`                                                                                                 | no, unless `UseNumber`                             | sessions `TestFold` ("1 and 1.0 tie"); inbox `TestPrintsTheRowUnchanged`                                                                                                                                                                                       |
| `1e2`                                                                      | prints `1E+2` (canonical decNumber text)                                 | `100`; `UseNumber` keeps `1e2`                                                                                                              | yes with `UseNumber` (same number, different text) | inbox `TestPrintsTheRowUnchanged` and log (`1e3` -> `1E+3`); jqrun (`1.0e3` -> `1.0E+3`)                                                                                                                                                                       |
| duplicate keys `{"a":1,"b":2,"a":3}`                                       | first position, last value                                               | last value; map loses order (Marshal sorts), struct is last-wins                                                                            | yes by value; no by order                          | jsonv `TestDecodeStreamDuplicateKeysKeepFirstPositionLastValue`; log `c1Msg` (must select `c1`); inbox `TestPrintsTheRowUnchanged`                                                                                                                             |
| invalid UTF-8: lone `\xff`, overlong `\xc0\x80`                            | 1 and 2 U+FFFD                                                           | 1 and 2 U+FFFD                                                                                                                              | yes                                                | jsonv `TestDecodeStream`                                                                                                                                                                                                                                       |
| invalid UTF-8: truncated lead `\xe2\x82`, encoded surrogate `\xed\xa0\x80` | 1 U+FFFD each (jq `jvp_utf8_next`)                                       | decode: 2 and 3; Marshal emits the six-character `\ufffd` escape per bad byte                                                               | no                                                 | jsonv decode_test (`\xe2\x82A`, `\xed\xa0\x80`), encode_test; none against Go                                                                                                                                                                                  |
| `"\ud83d"`, `"\ud83dx"` (lone high surrogate escape)                       | error                                                                    | string with U+FFFD, no error                                                                                                                | no                                                 | status `TestSeamsRefuseLoneHighSurrogates`, `TestPiReviewerVerdicts`, `TestFoldsMatchJQ`; stack `TestFoldRecord`; jqrun `TestJQFromJSON`                                                                                                                       |
| `"\udc00"` (lone low surrogate)                                            | U+FFFD                                                                   | U+FFFD                                                                                                                                      | yes                                                | status diff corpus                                                                                                                                                                                                                                             |
| torn tail `{"a":1}\n{"b":`                                                 | `DecodeStreamPrefix`: 1 value + `*SyntaxError`; `DecodeStream`: 0 values | Decoder loop: 1 value + `io.ErrUnexpectedEOF`                                                                                               | prefix equal; error type differs                   | bus `TestReadEventsDecodeError` (asserts `*jsonv.SyntaxError`); log `TestTornTailKeepsThePrefixAndExitsFive`; status diff torn lines                                                                                                                           |
| trailing garbage `{"a":1} x`; two values `1 2`                             | prefix 1 value + error; stream 2 values; `ParseOne` rejects              | Decoder loop: same values, other error text; `Unmarshal` rejects                                                                            | yes                                                | status diff corpus; resolve `TestRowsSkipsWhatItCannotDecode`; await `TestUnparseableLinesAreSkipped`                                                                                                                                                          |
| BOM prefix                                                                 | stripped                                                                 | invalid-character error                                                                                                                     | no                                                 | status diff corpus; bus `TestReadEventsStreamShapes` "bom"                                                                                                                                                                                                     |
| `<`, `>`, `&`, U+2028, U+2029 in a string                                  | raw                                                                      | `Marshal`: `\u003c` `\u003e` `\u0026` `\u2028` `\u2029`; `SetEscapeHTML(false)`: `<>&` raw, U+2028/9 still escaped as `\u2028` and `\u2029` | yes by value                                       | inbox `TestPrintsTheRowUnchanged`, log (`<b> &` raw); none for U+2028/9                                                                                                                                                                                        |
| DEL `0x7f`                                                                 | `\u007f`                                                                 | raw `0x7f`                                                                                                                                  | yes by value                                       | jsonv encode_test; inbox and log byte tests                                                                                                                                                                                                                    |
| map vs struct field order                                                  | insertion order                                                          | map: sorted; struct: declaration order                                                                                                      | yes by value; no by bytes                          | hold `TestAddRowMatchesTheBuilder`, stall `TestPostRow`, reply `TestOversizedBodyIsElided`                                                                                                                                                                     |
| `$JQ_COLORS`, pretty                                                       | 8-field SGR palette, `-C` walker, indent                                 | none; `MarshalIndent("", "  ")` matches jq's indent                                                                                         | structure yes; colour not applicable               | jsonv `testdata/jqcolors.tsv`; main `TestRunMapsFoldErrorsToExitCodes` and the "honours JQ_COLORS" case; log, crews, await, hold, inbox, report tests assert `Failed to set $JQ_COLORS\n` (retro asserts its absence)                                          |

Verdict per semantic. "Helper" means a small function that survives the
conversion; D1 and D2 are owner decisions (last section).

- NaN, Infinity, `-nan` as input tokens. Load-bearing today in the bus
  decoder (`ReadEventsTolerant`), `probe.BusRows`, `resolve.Rows`, the
  per-line `fromjson?` readers (`await`, `ledger`), inbox's `.ts` compare and
  the jq-oracle fold corpus. No shipped writer emits them: `jq -c` prints NaN
  and `-nan` as `null` and clamps infinity (checked with jq 1.8.2), and the
  Go writers clamp. Only a hand-edited or foreign line carries the token, so
  dropping tolerance costs availability, not correctness: with
  `encoding/json` such a line becomes a decode error (the existing torn-tail
  path) or a skipped line, and an `await`/`resolve` consumer silently loses
  that row where jq kept it. Decision D1. Independent of D1, the output side
  must keep clamping (NaN to `null`, +-Inf to +-`1.7976931348623157e308`),
  because gojq computes both and `json.Marshal` errors on them
  (jqrun `TestComputedNumbersStayComputed`, roster `TestFold`).
- Number-literal text and big integers. Load-bearing for inbox
  (`greaterThanNumber`/`decimalGreater` compare `.ts` literal text exactly
  against `--since`; `TestSince` "bignum ts beats a small since" fails under
  float64), for the sessions fold ordering (`TestFold` "big literal ts") and
  for jqrun's hand-off (`toGo` sends `json.Number(NumberText())`, so big
  ints order exactly in gojq). Replace with `Decoder.UseNumber` and
  `json.Number` end to end: never decode a bus row into bare `float64`. The
  decimal compare stays as a local function over `json.Number` text.
  `1e2` -> `1E+2` is a text difference only (same number); the byte-pinning
  tests (inbox, log, jqrun) become value compares.
- Duplicate keys. Value-equal under `map`/struct decoding (last value wins,
  like jq). Only a `RawMessage` pass-through keeps both keys; every reader
  (jq, Go) then picks the last, so it is value-equal too. log's
  `TestFiltersOneCrewByteForByte` becomes a `jq -S` compare. Replace.
- Invalid UTF-8. Differs only on truncated-lead and encoded-surrogate sequences
  (measured rows above); a lone bad byte agrees. Reach: shipped producers emit
  valid UTF-8 (`jq` repairs `--arg` bytes; Go writers take argv and file
  names). Accept for (a) rows, with the per-package note. Not acceptable
  blindly in the waiver gate (`ledger.jqArg`, which compares ids as jq holds
  them). Do not use `strings.ToValidUTF8` there: it collapses a run of bad
  bytes into one U+FFFD, which is neither jq's rule nor Go's per-byte rule.
- `ParseOne` / `_jqfromjson`. The lone-high-surrogate error and `-nan` ->
  `null` are the rule that stops a forged row (`"cut":"\ud83d"`) from
  reading as a record (stack `TestFoldRecord`) or a seam verdict (status
  `TestSeamsRefuseLoneHighSurrogates`); gojq's and `encoding/json`'s
  `fromjson` accept the surrogate. `ParseOne` also takes what `encoding/json`
  rejects: `Infinity`/`-Infinity`/`inf` (+-Inf `float64`, printed clamped),
  every NaN spelling (`nan`, `-nan`, `snan`, zero-payload `nan0` -> NaN
  `float64`, printed `null`, `type` stays `"number"`; `nan1` errors),
  `+1`/`.5`/`01` and a leading BOM. status `diff_test` feeds `Infinity`,
  `-Infinity`, `nan0`, `snan`, a BOM line and `"n":Infinity` through
  `_jqfromjson` in `TestFoldsMatchJQ` (:84, :86, :96), which skips without jq
  on PATH, so a too-narrow helper passes locally and fails in CI. Keep (small
  helper): the pre-scan must cover the unpaired `\uD800`-`\uDBFF` escape,
  all of those number and NaN spellings and the BOM before decoding with
  `UseNumber`; otherwise keep the jsonv parser for this one call.
- Key order inside a `body` string. `hold`'s rows carry a JSON document as
  string text, so its key order is part of the value: `jq -S` cannot excuse
  it. A map re-sorts and fails `TestAddRowMatchesTheBuilder`. Replace with
  ordered structs, with `*string` for the always-emitted nullable fields
  (`plan`, `mcp`, `shape`, `spec` print `null`) and `SetEscapeHTML(false)` on
  the inner marshal. That flag does not make the string text jq-compatible
  (DEL raw, U+2028/9 escaped, per-byte invalid-UTF-8 repair; see the hold
  entry): either a jq-style string encoder for the inner body or accept the
  differences, D4. A consumer reading the body by `fromjson` sees the same
  value for DEL and U+2028/9, but not for invalid UTF-8 (a truncated-lead or
  encoded-surrogate sequence in an argv title is one U+FFFD under jsonv, one
  per byte under Go); the `FitLine` shrink point moves. `status.msg` passes the
  caller's text through as a string (no re-encode), and `bus.Shrink` re-encodes JSON text leaf by leaf
  with source key order kept (bus `TestShrinkKeepsJSONShape`), so Shrink needs
  an order-preserving token walker, not `any`.
- Torn-tail prefix decoding. Same values as a Decoder loop; the error type
  differs. Replace with a Decoder loop that returns the decoded prefix and
  maps `*json.SyntaxError` / `io.ErrUnexpectedEOF` to `bus.DecodeError`
  (other errors stay `OpenError`); `TestReadEventsDecodeError` moves off
  `*jsonv.SyntaxError`. Exit codes do not move (log exit 5, status, crews,
  fleet).
- `$JQ_COLORS` and pretty output. Pretty is `MarshalIndent("", "  ")`:
  retro, rate and reply tests only pin the prefix, suffix and the `[\n  "done"\n]`
  agent text, which it matches. Colour (`main_test.go` coloured `[]` cases,
  `log` `TestColourComesFromThePalette`) and the `Failed to set $JQ_COLORS`
  warning (seven test files) are the part with no `encoding/json` equivalent.
  The port doc lists TTY colouring and the warning count as not mirrored.
  Decision D2.
- HTML escaping and raw-line greps. `Marshal` escapes `<>&`, which is
  value-equal but changes byte length: `bus.FitLine` measures the encoded
  line against `LineMax` (4096) and re-shrinks, so any row built inside
  `FitLine` (hold add/release, reply, status msg/status) must marshal with
  `SetEscapeHTML(false)` or the shrink point moves (`<` is 1 byte raw, 6
  escaped); U+2028/9 escape either way (3 bytes to 6), a value-equal change
  nothing pins. Rows that skip `FitLine` (prwatch, stack, reap, nudge, stall
  post) need no flag. Raw greps in bats expect a compact line with no space
  after the colon: `tests/crew.bats` `'"stream":"heartbeat"'` and
  `'"stream":"hold_due"'` (9319-9322, 9929), `'"kind":"reap"'` (3565, 4361,
  4438, 4487, 4524), `tests/dispatch.bats` `'"kind":"claim-issue"'` (8639). `encoding/json`
  satisfies all of them; a `MarshalIndent` or a `jq`-style spaced printer
  would not.

## Inventory

54 non-test importers, from
`rg -l '"github.com/noamsto/dispatcher/crew/internal/jsonv"' crew --type go | rg -v _test`
at the PR head (55 before `prwatch` was converted). In path-sorted order. Each
entry: class, evidence (symbols and what for), breakage if switched (b/c uses),
verdict. "Moves with" names the seam the verdict waits on; the Conversion plan
orders them.

- `crew/internal/adopt/adopt.go` — (c), (b-lite). `claimRows` (:292-296)
  feeds `ev.Raw` of every `bus.ReadEvents` event to `jqrun.Run(claimsProgram)`;
  `fieldText` (:403-418) renders jq `@tsv` text (`NumberText`, compact JSON for
  containers). Breaks: `bus.Event.Raw` type; adopt_test `TestClaimRows` and
  "a numeric and a string issue are one issue" (group_by on `tostring`); as
  float64, `"issue":73.0` stringifies `73`, not jq's `73.0` (unpinned).
  Verdict: keep until `bus.Event` and jqrun move; then small helper
  (`fieldText` over gojq output).
- `crew/internal/await/await.go` — (c), (b). `Encode` per delivered row
  (:109-112); `due()` decodes each line with `DecodeStream` and skips unless
  exactly one value (:184-190), the arm's `-R` + `fromjson?`; `marks.Read`
  result is a jqrun var, output `Elems()` goes to `marks.Record` (:192-196,
  :119). Breaks: `TestUnparseableLinesAreSkipped` (torn, two-value, blank,
  `null`) passes under `json.Unmarshal`; a line with `NaN`/`Infinity`/BOM is
  dropped by `encoding/json` where `fromjson?` kept it (unpinned); `JQ_COLORS`
  tests (await_test 530-548). Verdict: replace after `marks` is retyped; per-line
  decode follows D1.
- `crew/internal/bus/append.go` — (b). `Shrink` (:105-120) decodes text as a jq
  stream (`DecodeStream`), `jq -e` last-value container test, re-encodes each
  value with leaves cut (`shrinkLeaves`, :143-166) via `Append`, `Object`,
  `Member`, `Str`, `Kind*`. Output is a string leaf inside a bus `body`, so its
  bytes are part of the value. Breaks: `TestShrinkStreamBranch` ("jq NaN
  literal" `[NaN]` -> `[null]`, "two documents", "trailing junk"),
  `TestShrinkKeepsJSONShape` (key order id,released,n,k; `1234567890` kept; a
  map reorders, float64 loses big ints); Marshal escapes `<>&`/U+2028 and
  leaves DEL raw. `Append(path, line)` and `FitLine` use no jsonv. Verdict:
  keep; small helper (token walk with `Decoder.Token` + `UseNumber`,
  `SetEscapeHTML(false)`) if D1 drops NaN and the divergences in duplicate
  keys and DEL escaping are accepted.
- `crew/internal/bus/bus.go` — (b), (c). `Event.TS`, `Event.Body`, `Event.Raw`
  are `jsonv.Value` (:123-131); `ReadEventsTolerant` (:181-193) is
  `DecodeStreamPrefix` plus `*jsonv.SyntaxError` -> `*DecodeError`; `newEvent`
  (:134-148) extracts string fields (non-string -> ""). Breaks: inbox
  `TestSince` "NaN never"/"Infinity always" write the row to the log and expect
  exit 0 (NaN row would end the tail as a decode error); `TestReadEventsDecodeError`
  asserts the `*jsonv.SyntaxError` type; `TestReadEventsRawRoundTrip` needs
  source key order and `{"b":1,"a":2}` kept (`RawMessage`, not `any`);
  `TestReadEventsFields` keeps `TS`/`Body` raw; `TestReadEventsStreamShapes`
  "bom" needs a manual trim; big-int/`1.50` literals need `RawMessage` or
  `UseNumber` (log `c1Msg`). Verdict: keep until D1; then small helper
  (Decoder loop + `UseNumber` + BOM trim, `Raw json.RawMessage`, prefix on
  error). Sixteen internal packages and `main.go` read `Raw`; see the plan for
  the staged `Doc` field.
- `crew/internal/clock/clock.go` — (c). `NowF` returns `jsonv.Num(...)`
  (:74-79); consumers: hold (jqrun `now` var, hold.go :216, :260) and
  rosterrender (`AsFloat` to int64). Breaks: only `clock_test`
  `TestRealMSAndNowFPrecision`, whose `compact()` text assertion (`1700000000.0005`)
  becomes a float compare; gojq already receives `float64`. Verdict: replace
  (`NowF() float64`).
- `crew/internal/crews/crews.go` — (c). Builds `meta` rows and a stats var from
  `Object`/`Member`/`Str`/`Bool`/`Null` (:114-146) and re-feeds jqrun output as
  a var (:152); no decode, no encode of its own. Breaks: nothing named
  (`TestTableTornTailKeepsPrefixIDsWithoutStats` and
  `TestTableFinalPassFailureIsExit5WithNoRows` are bus prefix semantics).
  Verdict: replace (`[]any` of `map[string]any`; `pid`/`alive` nil when unknown)
  after `bus.Event` moves.
- `crew/internal/fleet/fleet.go` — (a), (c); colour (b). `Options.Colors
*jsonv.Palette` (:39), `crewRow.Val` with `Set("repo"/"crew_id"/"stale")`
  (:107-111, :139), `Encode` with `Indent` + `Colors` (:126-128), `table()`
  readers (:187-230), `e.Raw` rows (:91-94). Breaks: nothing; no fleet test
  pins colour or indent (`jsonRows` parses with `encoding/json`);
  `TestTornTailKeepsPrefix` is bus semantics. Colour is only fed from main.go
  (:923-925). Verdict: replace (`[]map[string]any`, `MarshalIndent`); drop
  `Colors` per D2.
- `crew/internal/hold/hold.go` — (c) read side, (a) write side, (b) body key
  order. Write: `addRow` (:386) and `release` (:435) build `ts`, five strings
  and a `body` that is JSON text (`id`, `wait{engine,window,resets_at}`,
  `task{ref,branch,tier,engine,model,effort,plan,mcp,draft,shape,title,spec}`),
  both through `bus.FitLine`; helpers `tsValue`/`digitsLiteral`/`orNull`. Read:
  jqrun bridge `folds.call`/`outstanding`/`Outstanding` (:136-176), `holdVars`,
  `render` (:271, :534, :543). Breaks: a map-built body re-sorts keys and fails
  `TestAddRowMatchesTheBuilder` (exact body string);
  `TestAddLongTitleFitsAndKeepsItsShape`, `TestReleaseLongIDShrinksTheLeaf`
  (FitLine byte length: needs `SetEscapeHTML(false)`); `resets_at` is bounded by
  `ParseInt` (test :173-178), so `int64` is exact; the inner body is text, and
  `SetEscapeHTML(false)` fixes only `<>&`: Go still writes DEL (`0x7f`) raw
  where jsonv writes `\u007f`, escapes U+2028/9 where jsonv writes them raw,
  and repairs invalid UTF-8 per byte (titles and ids come from argv, :386-415)
  where jsonv follows jq. `TestAddRowMatchesTheBuilder` pins only key order, so
  nothing settles it (D4); the old bats suite read the body with
  `fromjson` (by value). `TestHostileBodiesExitFive`, `TestCorruptBusExitsFive`
  pin the read side. Verdict: split. Write side replace (ordered structs,
  `*string` for nullable fields, `digitsLiteral` stays; the inner string
  encoder per D4); read side keep until
  jqrun takes `any`, then `Outstanding([]any, crew) (any, error)`.
- `crew/internal/identity/identity.go` — (c), (b), (a). `At`/`Palette`/
  `Recorded`/`RecordedAll`/`For`/`Assign` return `jsonv.Value`; `recordedIn`
  (:107) reads kind/branch/name/tmux as strings and passes `color` through as
  any JSON; `occupied` (:235) reads `ev.Raw.Get("crew_id")`, `ev.TS.AsFloat`;
  `heldName` (:306) applies jq `// ""` truthiness; `emit` (:382) prints
  compact. Breaks: `TestRecorded` ("color passes through as any json"
  `{"a":[1,2]}`, "non-string name" `7`/`null`, "null and non-object events are
  skipped"): typed string fields would reject or drop those rows;
  `heldName` on `"name":false`; roster and rosterrender splice the values into
  jqrun inputs. Verdict: keep until `bus.Event` and jqrun move; then small
  helper (`Identity{Name, Tmux string; Color json.RawMessage}`, `Palette()
[]string`); `where` (`identity.At(...).Get("name")`) moves with it.
- `crew/internal/inbox/inbox.go` — (b), (c). `greaterThanNumber`/`decimalGreater`/
  `decimal` (:246-377) compare `.ts` literal text exactly and use jsonv `Kind`
  order as jq's total order (string, array, object beat numbers; null and
  booleans lose); `pastMarks` (:209-232) runs `.ts > ($marks[.from] // 0)`
  through `jqrun.Run` with `map[string]jsonv.Value{"marks": held}`; rows print
  via `Encode` (:131). Breaks: `TestSince` ("bignum ts beats a small since",
  "fraction past a double's digits", "huge exponent" `1e999999999`, "NaN never",
  "Infinity always", "string/array/object ts outranks a number"),
  `TestDecimalGreater`, `TestGreaterThanNumber` (uses `testjson.MustParse("NaN")`),
  `TestPrintsTheRowUnchanged` (bytes: `1E+3`, `\u007f`, collapsed duplicate
  `crew_id`), the `TestUndelivered*` jqrun cases. float64 fails the bignum,
  fraction and exponent cases. Verdict: keep; shrinks to a `json.Number` text
  compare plus a type-rank switch once `bus.Event.Raw` and `marks.Read` move,
  and if D1 drops the NaN/Infinity cases.
- `crew/internal/jqrun/jqrun.go` — (c), (b); the bridge itself. `Run`/`Compile`/
  `Program.Run` take and return `Value` (:52, :69, :102); `toGo` (:136-182)
  hands `NumberText()` over as `json.Number` and NaN/clamped infinity as
  `float64`; `fromGo` (:184-233) rebuilds numbers from gojq text and sorts
  keys; `ParseOne` (:35) backs `_jqfromjson` (other non-test callers:
  reap folds.go and release.go, stack.go `parseJSON`, piagentdir.go); `Array`
  (:57). Breaks: signatures to `any` break nothing in the file; its tests
  re-pin on the helper: `TestNumberLiteralsRoundTripExactly` (12345678901234567891,
  `1e1000`: needs `UseNumber`), `TestComputedNumbersStayComputed` (NaN prints
  `null`, +-Inf clamp: `json.Marshal` errors), `TestToGoNaNStaysFloat`,
  `TestJQFromJSON` (lone `\ud83d` errors, `-nan` -> `null`, `\udc00` ->
  U+FFFD), `TestJQFromJSONIsOptIn`; `TestFromGoSortsObjectKeys` becomes free.
  Verdict: keep, shrink: `Run`/`Compile`/`Program.Run` over `any` in and out;
  keep the `json.Number`/`float64` normaliser, the clamping marshal and a
  `ParseOne` equivalent for `_jqfromjson`.
- `crew/internal/ledger/ledger.go` — (b). `waived` (:616-637) is the arm's
  waiver jq: per line `DecodeStream` (:622), exactly one object, `field` (:645)
  string tests, `body` (:652) `| tostring` where a non-string body is its
  compact JSON text, matched by clause and negation scanning; `jqArg` (:640)
  is `jsonv.Str(s).AsString()` (jq `--arg` repair); `Append` (:660). Breaks:
  `ledger_test` "object body is its JSON text" (`{"note":"waive AC3"}` must
  waive via the `tostring` text) and the jq-oracle `waiver` fuzz (4000 logs,
  body `{"n":<text>,"k":[1,1.50,null,true]}`, two-value line, `"body":false`):
  a map re-marshal reorders keys and prints `1.50` as `1.5`;
  `RawMessage` + `json.Compact` keeps order and literal for that corpus but
  diverges from jq on `1e3` -> `1E+3`, `\u007f`, `<>&`, duplicate keys and
  invalid UTF-8. Security relevant (a waiver and negation gate). Verdict: small
  helper (one-object-per-line Decoder check, `RawMessage` body +
  `json.Compact`, per-byte repair for `jqArg`) only if the oracle fuzz stays
  green on that corpus; else keep. Needs D3.
- `crew/internal/log/log.go` — (a), (b) colour. Filters `Raw` rows by
  `crew_id` and re-encodes (:59-80) via `Encode` with `Options{Colors}`; the
  package doc says it keeps source key order and number literals. Breaks:
  `TestFiltersOneCrewByteForByte` (`1e3` -> `1E+3`, `-0.0`, `1e1000`, DEL as
  `\u007f`: bytes; fails under `RawMessage`, passes as a `jq -S` compare;
  `18446744073709551617` and `1.50` still need `RawMessage`/`json.Number`),
  `TestColourComesFromThePalette`, `TestJQColorsWarnsOnceWhenTheLogExists`;
  `TestNonObjectRowsFollowJqLastInputRule` and the torn-tail exit 5 test are
  bus semantics. tests/crew.bats :2597-2622 already compares with `jq -s`.
  Verdict: replace once `bus.Event` exposes `Raw` as `RawMessage` or `any` plus
  the marshaller; drop `Colors` per D2.
- `crew/internal/marks/marks.go` — (c), (b); (a) write. `Read` (:104-119) and
  `Record` (:141-168) run `read.jq` (`map(objects) | add // {}`) and
  `record.jq` (`reduce ... max`, jq total order) through `jqrun.Run` with
  `[]jsonv.Value` in and a `Value` out; the write is `Append` + `"\n"`;
  `DecodeStream`, `Object()`. Breaks: `TestRead` (torn tail -> `{}`, non-objects
  dropped), `TestRecord` ("a string mark beats a number ts", "a null/numeric/
  missing from loses the write" = gojq type error leaves the file unchanged),
  `TestRecordMatchesJq`/`TestReadMatchesJq` (oracle against real jq: `1e3`,
  `1000.0`, `9.5`). The jq programs are the contract; typing them in Go
  re-implements jq `max`, `+` and index errors. Verdict: keep (c); only the
  final marshal is replaceable. After jqrun takes `any`: `Read() any`,
  `Record(msgs []any)`.
- `crew/internal/nudge/arm.go` — (c), (a), (b). `Options.Rows` (:63) is
  `[]jsonv.Value`; `fold` (:447) runs `joined.jq`/`already.jq` through jqrun
  with `Str`/`Num` vars; `textOf`/`numOf`/`isNull` read the fold row
  (:495-507); `waitRow` (:423) is the `nudge_wait` row: ten strings plus a
  `msg_ts` number literal from `ParseNumber(a.ts)` and optional `rc`. No
  FitLine, no `body`. Breaks: the folds consume tolerant-decoded rows
  (`Rows` is `probe.BusRows`); `msg_ts` is `a.ts`, the unread scan's text
  output (arm.go :295-297), digit-checked upstream: `waitRow` runs only after
  `Pane` refused with "live turn or no idle input box" (:372-384, and `end`
  only once `waiting` is set), and `Pane` refuses a non-digit `MsgTS` first
  (nudge.go :99). Leading zeros pass (a foreign string ts `"0017"` sorts above
  every number in `unread.jq`'s `max_by(.ts)`): `ParseNumber` prints `17`,
  `json.Number("0017")` makes Marshal error, and `null` would break
  `joined.jq`'s `(.msg_ts // 0) >= $m`, so a joiner misses the resolved row;
  digit strings beyond `int64` stay exact only as a literal
  (unpinned); arm_test pins `"msg_ts":1700000000000`, `"rc":2` substrings.
  Verdict: split. Folds keep (c); `waitRow` small helper (struct;
  `msg_ts` as `json.Number(digitsLiteral(a.ts))`, as in nudge.go).
- `crew/internal/nudge/nudge.go` — (a), (c). Exported `Deps.Events func()
([]jsonv.Value, error)` (:78) and `Deps.Append func(jsonv.Value) error`
  (:79); `nudgeRow` (:215) is ts, nine strings and a `msg_ts` literal (its
  comment already says key order is free); `foldNewest` (:379) calls
  `sessions.Fold` and takes `.Elems()[len-1]`; `isTrue`/`textOf` (:435, :440).
  Breaks: `msg_ts` here is digit-checked (`allDigits`, :99) but may carry
  leading zeros (`0017...` prints `17...` under jsonv; `json.Number("0017...")`
  is invalid and Marshal errors, so use `digitsLiteral`) and overflow as
  above; `foldNewest` needs a `Value` from `sessions.Fold`; nudge_test fakes hold `[]jsonv.Value`
  (:98-132, :641) and pin `"msg_ts":1700000000000` and watchdog `"msg_ts":0`.
  Nothing outside the package touches `Deps.Events/Append` (stall uses
  `PaneDeps`). Verdict: small helper (row struct, `Append` takes the struct or
  `any`; `Events` stays jsonv-typed until `sessions.Fold` changes).
- `crew/internal/nudge/wire.go` — (c), (a). `PaneDeps.Events` (:50-63)
  (`bus.ReadEvents` -> `ev.Raw`), `Append` (:65) (`Append` compact ->
  `bus.Append`), `Default.Rows` (:88) (`probe.BusRows`). Exercised only through
  bats and the harness (no wire test). Breaks: the element type of
  `Events`/`Rows` that `sessions.Fold`/`jqrun.Run` accept. Verdict: keep;
  mechanical follow-on of nudge.go and arm.go.
- `crew/internal/piagentdir/piagentdir.go` — (a), (b), (c). `seedSettings`
  (:213-242) reads `settings.json` as one object (else `Object()`), `Get`/`Set`
  `defaultProjectTrust` and an order-preserving `extensions` append; `seedAuth`
  (:271) requires the last stream value to be an object; `seedModels` (:290)
  `ParseOne(string(out))` then `jqrun.Run(clashProg, ...)` (:302) and
  `prog.Run(settings)` (:316); `pretty` uses `Indent`. Breaks:
  `TestDispatchConfigFailures` (`not json` -> exit 2 and exact text),
  `TestAuthRefusals`, `TestIdempotent` (second seed writes nothing: output must
  be byte-stable; a sorted-key encoder is), `TestSettingsClash`,
  `TestExtensions`, `TestMatchesBash`. `ParseOne` rejects a lone high surrogate
  that `encoding/json` accepts (unpinned; settings come from Go config); a
  `settings.json` with `NaN` resets to `{}` under `encoding/json`
  (implausible; pi writes it). Verdict: small helper (`map[string]json.RawMessage`
  for settings so unknown keys and number literals round-trip, `MarshalIndent`;
  keep the exit-2 text); folds follow jqrun.
- `crew/internal/rate/burn.go` — (a), (b) small. `burnRules` (:30-62), `burnWeight`/
  `byEffortEntry` (:67-104) read `.burnClasses` from dispatch-config JSON;
  `jqText` (:112-131) prints a number's literal text and `argjson` (:139-146)
  re-parses it, emulating `--argjson`, so weight `1.50` keeps its text. Breaks:
  `TestBurnTableOracle`, `TestBurnSlashedModelID`, `TestBurnRulesEdgeCases`
  (weight Kind compare, no-default byEffort -> class `null`),
  `TestBurnRulesNoTable` compare weights as floats and pass under
  `float64`/`json.Number`; the tests themselves build jsonv values (:23, :225-234,
  :262) and re-type. A quoted `weight` like `".5"` or `"nan"` re-parses to a
  number under jsonv's `argjson`, and a malformed one nulls only that weight
  (:139-146); with `Weight json.Number` it makes the whole `Unmarshal` fail
  (`invalid number literal`), and a non-string `class` (`jqText` renders `3` as
  `"3"`, :44-45) or `match` is an `UnmarshalTypeError`: one bad rule would drop
  every rule (unpinned).
  `byEffort` lookup is first-truthy-match over ordered members, and keys are
  unique after duplicate folding. jsonv also skips a non-object row (:37-39),
  drops a rule whose `match` is not a string (:41-43, :55-57) and ignores a
  non-object `byEffort` (:51). Verdict: replace, but decode `burnClasses` as
  `[]json.RawMessage` and each row as `map[string]json.RawMessage`, parsing
  each field with `jqText`/`argjson` semantics and skipping rows and fields
  that do not fit, so a bad row or field degrades only itself; or record the
  divergence.
- `crew/internal/rate/rate.go` — (c), (a). Store read whole with `DecodeStream`
  (:153-158), `Array()` fallback (:163), `deduped.Elems()` (:170), `Bool`/`Str`
  vars (:171-173), `--json` printed with `Encode` + `Indent` (:184). Breaks:
  `TestJSONAggregatesCarryValueKN` (prefix `[\n  {\n` at :162:
  `MarshalIndent("", "  ")` passes), `TestStoreFailuresFoldToEmpty` ("unparseable"
  and "bare number" fold to `[]`: keep stream error -> `[]`; a Decoder loop does),
  `TestDuplicateRunIDsFoldLastWins` (jq group logic, not key folding). Store
  numbers are jq-written and finite. Verdict: replace.
- `crew/internal/rate/sweep.go` — (c), (a). Store `DecodeStream`, `costmap`
  `Object`/`Set` (:168-200), `DecodeStream(settings)` (:174), `plan`/`lockedMerge`
  jsonv vars (:235-275), `reconcile` patches (:286-398), `DecodeStream` of `gh`
  stdout (:406), `patch()` from `[]jsonv.Member` (:417-424), `foldStore`
  (:427-440), `appendBatch` writes `Append(batch, row, Options{})` + `\n`
  (:447-452), `fieldText`/`jqText`/`truthy`/`memberOf` (:525-546). Breaks:
  nothing by value (tests parse appended lines with `encoding/json`, :176);
  `TestFoldStoreTornFileIsEmpty` (torn store -> `[]`, :621),
  `TestSweepSweptAtIsMergeTimeClock` (float seconds\*1000 stays integral);
  `Marshal` escapes `<>&` in the appended store lines (value-equal; use an
  Encoder with `SetEscapeHTML(false)` in `appendBatch` to keep the store text
  stable). Verdict: replace (`any` + the clamping marshal; the `Member` var
  builder becomes `map[string]any`).
- `crew/internal/rate/sweepall.go` — (c). `DecodeStream(store)` (:93), `Str`
  var (:97), `n.AsFloat()` (:101) over `runsProgram`. Breaks: nothing;
  `sweepall_test` "a torn line is jq's error" (want 0) holds under a Decoder
  error -> 0. Verdict: replace.
- `crew/internal/reap/folds.go` — (c), (b). `terminalStates` `Array` (:33),
  `readRaws` (:44, `bus.ReadEvents` -> `Raw`), `latest`/`idleRows`/`claimed`/
  `postedSince`/`prFromList` all `jqrun.Run`, `cells`/`rawLines` read output via
  `Elems`/`AsString`; `ParseNumber(r.idle)` (:90) is `--argjson idle`
  (digits, `ParseInt`-checked at reap.go:272), `ParseOne(ts)` (:119) is jq
  `fromjson` of the status-ts text (failure -> "may have posted" -> keep the
  worktree), `DecodeStream(prs)` (:138) parses `gh pr list` text. Breaks:
  every fold input and output is jsonv; `postedSince` with ts `NaN`/`nan`
  (jq accepts, `encoding/json` rejects: returns true instead of comparing) or a
  lone-high-surrogate ts (jq rejects, Go accepts); `prFromList` on text with
  NaN or concatenated documents. No test names a ts/NaN/surrogate edge;
  `TestCrossRepoPRIgnoredThenGitHubPR`, `TestFindPRFailures`,
  `TestIdleFoldFailureIsSilent`, `TestCandidateFoldOnCorruptBus` pin the
  fold-failure paths. Verdict: keep until jqrun is `any`-typed; then
  `ParseOne` -> the `ParseOne` helper (surrogate rule), the rest replace.
- `crew/internal/reap/forget.go` — (c). `readRaws` + `jqrun.Run(forgetProg,
raws, now, {"b": Str(branch)})`, result via `cells` (:38-42). Breaks: jqrun
  var and input type only (remove_test :353 `f.run().forget(rb)`). Verdict: keep;
  one reference, moves with folds.go.
- `crew/internal/reap/occupants.go` — (a). `OccupantsJSON` (:72, exported;
  array of `{window,name,pane|null,command,engine,panes[{pane,command}]}`) and
  `RunOccupants` (:104) `Append` + `"\n"`; no `body`, no FitLine. Breaks:
  only a nil slice must print `[]` not `null` (`TestRunOccupants` :104) and an
  empty pane prints `null` (use `*string`); tests compare by value
  (`TestOccupantsJSON` :90); tests/crew.bats 845-886 and tests/reap-race.bats
  125, 145 run `crew occupants`. Verdict: replace (non-nil `[]occupantJSON`).
- `crew/internal/reap/release.go` — (a) row, (c)+(b) transcript filter. Row
  (:92-104): `kind:"release"` with `ts`, `branch`, `session`, `state`, `windows`
  `[]string`, appended with no FitLine; `newerUserTurn` (:165-182) compiles
  `humanProgram` (`jqrun.Compile`), then per transcript line `ParseOne(line)` ->
  `prog.Run(row)` (jq `fromjson?` and per-row error isolation). Breaks: row
  tests are value-equal (`TestReleaseWritesRowAndKills` :450, with a "row is not
  compact" check :486); `prog.Run` takes a `Value`; a transcript line with a
  lone high surrogate or a NaN token decodes differently under `encoding/json`
  (jq skips the line; transcripts are worker-writable and unbounded; unpinned);
  `TestReleaseTranscriptUserTurn`, `TestReleaseTranscriptNewestFileWins`.
  Verdict: split. Row replace (struct, `windows` non-nil); transcript filter
  keep until jqrun is `any`, then the `ParseOne` helper.
- `crew/internal/reap/remove.go` — (a). `reapRow` (:238) is `kind:"reap"` with
  `ts`, `branch`, `pr`, `pr_state`, `worktree`, optional `issues` (issue mode)
  and `discarded` (via `row.Set`); `postReaped` (:263) writes a status row whose
  `body` is a nested object `{state,detail,source}`, not a string. No FitLine.
  Breaks: `discarded` must still print when `""` (discard_test :93 expects
  `"discarded":""`): pointer, not `omitempty`; `issues` absent unless issue
  mode; remove_test greps the raw line for `"kind":"reap"` (:84-92), identical
  under a compact marshal (tests/crew.bats 3565, 4361, 4438, 4487, 4524
  `! grep`). `worktree` and `discarded` are paths (invalid bytes repaired differently, unreachable
  from a valid path). Verdict: replace.
- `crew/internal/reply/reply.go` — (a), (c), (b). Row (:99-107) built inside
  `bus.FitLine`: `ts` (int64 `RealMS`), `crew_id`, `from`, `to`, `kind`
  `"msg"`, `body` last (plain text, shrunk by `Shrink`); `text` (:262) renders
  a decoded session row like `jq -r .key` (string bare, `NumberText`, containers
  as `Append` with `Indent`); `newestSession` (:207) bridges `sessions.Fold`;
  `compact` (:281). Breaks: `TestOversizedBodyIsElided` asserts the line ends
  `elidedMarker+"}` (bytes): a struct with `body` last passes, a map fails;
  FitLine length needs `SetEscapeHTML(false)`; `TestRefusals` "state is not a
  string" expects `[\n  "done"\n]` (agent-facing; `MarshalIndent("", "  ")`
  matches); `text()` on a container or number state depends on how the fold
  result is decoded (needs `UseNumber`; only `["done"]` is pinned). Verdict:
  split. Row replace; `resolve`/`text` keep until `sessions.Fold` returns `any`,
  then a small `jqText(any)` helper.
- `crew/internal/report/report.go` — (c). Forwards `ev.Raw` rows into jqrun
  (:64-68) with `map[string]jsonv.Value{"crew": Str}` (:70) and reads one string
  back (:72). Breaks: nothing in the package; `bus.Event.Raw` type only
  (`TestTornTailLosesEveryRow` is bus-level). Verdict: replace, blocked on
  `bus.Event`.
- `crew/internal/resolve/resolve.go` — (c), (b) light. `Rows() []jsonv.Value`
  (exported, :59-77) mimics `jq -R 'fromjson?'` with `DecodeStream` per line
  (:70); `Resolve(rows []jsonv.Value, ...)`, `Str` vars, `out.Kind()/Elems/
AsString`, `jqrun.Run(program, rows, 0, vars)`; fold is `resolve.jq`
  (`group_by(.branch) | map(max_by(.ts))`). Breaks: `TestRowsSkipsWhatItCannotDecode`
  (blank and two-value lines skipped) holds under `Unmarshal` + a trailing-data
  check; a NaN row is dropped (D1); `TestRowShapes`, `TestNewestRowPerBranchInBranchOrder`
  are gojq semantics. Callers: where (where.go :203) and nudge arm.go (:231).
  Verdict: replace at the boundary (`Rows` -> `[]any` via `UseNumber`, vars
  `map[string]any`), contingent on jqrun's signature.
- `crew/internal/retro/retro.go` — (c), (a). Raws from `ev.Raw` (:74-76), `Bool`
  vars `want_report`/`want_json` (:78-81), `Encode(stdout, out, Options{Indent:
true})` (:92), only for `--report --json`. Breaks: `TestReportJSONIsFoldedAndOrdered`
  (prefix `{\n  `, suffix `}\n`) passes under `MarshalIndent("", "  ")` and
  re-parses with `encoding/json`; the clamping marshal matters only if a bus
  number is echoed (none observed). Verdict: replace, after `bus.Event`.
- `crew/internal/roster/live.go` — (a). `Stale`/`Drawn`/`worktreeGone`/
  `rowBranch`/`rowState` take a `Value` (:52, :71, :88, :95, :103) and read
  `branch`, `state` strings and `age_s` float. Breaks: nothing; live_test builds
  rows with `ParseOne` (:20) and would build `map[string]any`; `age_s` of
  +-1.797e308 gives the same float via `AsFloat` or `json.Number.Float64`.
  Verdict: replace (`map[string]any`, or `{Branch, State string; AgeS
*float64}` decoded once at `Fold`'s boundary).
- `crew/internal/roster/roster.go` — (c), (b). `Fold(events []jsonv.Value, ...)
(jsonv.Value, error)` (:52), `Str` var (:54), `base.Elems()` (:61),
  `identityMap` (:83-96), `resolveExited` with `*jsonv.Value` `Get`/`Set`/`Or`
  (:102-121); rows leave jqrun, are mutated in Go (`state`, `exit_suspect`) and
  re-enter jqrun, so number text and non-finite numbers round-trip twice;
  depends on `identity.RecordedAll`/`For`. Breaks: roster `TestFold` "a computed
  infinity reads back as a literal" (:499, `Infinity` in `ts`/`tier` ->
  `1.7976931348623157E+308`, `age_s` -> `-1.797E+308`) and "an overflowing
  literal in detail reads back as a literal" (:509,
  `[1e1000000000,-1e1000000000,NaN,1.5,100]` -> `[+-1.797e308,null,1.5,100]`):
  inputs decode via `DecodeStream` (needs D1) and the marshal must clamp; the
  `E+308` spelling is a text artefact that becomes a value compare. Verdict:
  replace behind the clamping marshal; `Fold` returns `[]map[string]any`
  (`row["state"] = prev`). Moves with identity.
- `crew/internal/rosterrender/model.go` — (c), (a), (b). `emptyModel`
  `Object`/`Array`/`Member` (:50-55), `model()` raws and a five-element input
  array of pending, holds, `identityIDs`, `roleList` (:71-108),
  `identityIDs`/`roleList` (:117-161), `roleEngine` (:178, :197-205)
  `DecodeStream` of worker-writable `roles.json` plus `member()`, `renderText`
  (:209-225) `DecodeStream(hexJSON)` + `identity.Palette()` var, `liveCount`
  `AsFloat` (:232-244). Breaks: `TestRoleEngineIsOnlyAKnownEngine` (:242-295)
  passes under `encoding/json` (malformed -> `"?"`, symlink refused); a
  `roles.json` with `NaN`/`+1` reads `"?"` under `encoding/json` where jsonv read
  the engine (unpinned); `TestLiveCountIsTheModelNotItsPrintedForm` (:137)
  needs a float read from `any`. `hold.Outstanding`, `identity.*` and
  `roster.Fold` are cross-package inputs. Verdict: replace (`roleEngine` ->
  `map[string]struct{Agent string}`, `hexJSON` -> typed map) after hold,
  identity and roster.
- `crew/internal/sessions/sessions.go` — (c), (b) via data. `Fold(events
[]jsonv.Value, ...) jsonv.Value` (:21), `Str` vars (:22-25): a one-line
  wrapper over `jqrun.Run`. Breaks: `TestFold` "big literal ts reorder exactly"
  and "tie keeps file order" (sessions_test :240-247: 12345678901234567890 vs
  `...891` need `json.Number`), "huge ts" (:267-270, `1e1000` vs `1e18`, want
  prints `1E+18`: a text artefact that becomes a value compare), "NaN ts keys:
  gojq's NaN order reverses them" (:411-415: needs NaN-tolerant decode and
  NaN -> `null`). Callers: main.go (:859), nudge (nudge.go :384), reply (:214).
  Verdict: replace (signature `Fold([]any, ...) (any, error)`); the decode in
  tests and bus follows D1.
- `crew/internal/stack/align.go` — (a). `layerRow` (:312-329) builds the flat
  stdout row (`Object`/`Member`/`Num`/`Str`/`Bool`/`Null`, `nullWhenEmpty`);
  `pr()` (:95-112) reads `gh pr view` JSON (`parseJSON`, `isFalse`,
  `jqTostring`, `jqString`). No string-embedded JSON, no FitLine; the doc says
  value-equal (:306). Breaks: nothing in-repo (tests read rows with
  `encoding/json`); only gh JSON with a lone-high-surrogate branch name or a
  non-integer `number` (`1.0`: typed `int64` decode fails and prints the "from a
  fork" message instead of "no usable number"), neither reachable from gh.
  Verdict: replace (typed gh struct; row struct with `*string`/`*int64` in the
  order `pr, branch, parent, parent_pr, cut, mode, ready, directive, reason`).
- `crew/internal/stack/cut.go` — (a). `recordRow` (:100-107) `Object`/`Num`/`Str`
  (`ts` from `UnixMilli`), gh read via `parseJSON`/`isFalse`/`jqString`
  (:26-30); row is flat (`ts, crew_id, kind=stack_cut, branch, parent, cut`),
  appended directly, no FitLine, no `body`; the doc says value-equal (:94);
  consumers `newest.jq`/`record.jq`. Breaks: nothing (`int64` ts prints
  `1760000000123`, not `1.76e12`). Verdict: replace.
- `crew/internal/stack/stack.go` — (c), (a) helpers. `newest` (:185) and
  `record` (:223) are `jqrun.Run(newestProg|recordProg, stringValues(lines), 0,
vars, WithJQFromJSON())`; rows enter gojq as strings and `_jqfromjson?`
  decodes them inside jq, so jsonv does not parse bus rows here;
  `stringValues` (:273), `member`/`jsonvAppend` (:292-294), `isFalse`,
  `jqString`, `jqTostring` (`NumberText`, `Append` for containers), `parseJSON`
  (`ParseOne` + `KindObject`). Breaks: `TestFoldRecord` (forged `"cut":"\ud83d"`
  must not read as a record), `TestFoldUnreadableBus`,
  `TestCutOnlyForgedRowsMeansNoCut`: only because jqrun's `_jqfromjson` still
  needs `ParseOne`'s surrogate rule. Verdict: small helper (strings in, `any`
  out; drop `member`/`jsonvAppend`/`jqTostring` with align and cut).
- `crew/internal/stall/budget.go` — (a), (b) light. `cacheValue` (:32-41)
  `DecodeStream` with an exactly-one-value check; `lastTry` (:49-58)
  `Get("fetched_epoch")`, `AsFloat`, floor, `0 <= f < 1e12`; `creditsCover`
  (:195-203) `.engines[$e].credits_cover == true` via `Get`/`KindTrue`. Breaks:
  nothing under `any`/`map[string]any` (budget_test :229-236: `1700000000.9`
  floors, `1.7e9`, `"1700000000"` -> 0, `-5` -> 0, `1000000000000` -> 0, torn
  `{"fetched_epoch":17` -> 0; :185-188 `credits_cover` true/false/`"true"`/other
  engine). A typed struct would fail the whole decode on a wrong-typed unrelated
  field where jq read per field. Verdict: replace (decode to `any`, walk with
  type assertions).
- `crew/internal/stall/bus.go` — (c). `refresh` (:29-35) `jqrun.Run(refreshProgram,
rows, 0, vars)` over `[]jsonv.Value` from `w.p.BusRows`; `field` (:54)
  `At`/`AsString`; `tsMS` parses `.ts|tostring` text. Breaks: the stubbed
  `BusRows` seam in `stall_test` (:125), `probe_detectors_test` (:917) and two
  other test files; a fractional or exponent ts (`1.7e12`) prints as literal text
  in jq `tostring` but as `1700000000000` under float64 (`tsMS` handles both).
  Verdict: keep until jqrun and `probe.BusRows` take `any`; then small helper.
- `crew/internal/stall/post.go` — (a). `postRow` (:52-64) `Object`/`Member`/
  `Num`/`Str`, nested `body` object `{state,detail,source}`, then `Append` ->
  `bus.Append`; no FitLine or Shrink (`detail` is short watchdog text). Breaks:
  `TestPostRow` (:34) compares BYTE-exact
  `{"ts":...,"crew_id":...,"from":...,"to":...,"kind":"status","body":{...}}`
  with a raw `é`: a map (sorted keys) or default HTML escaping of `<>&` in
  `detail` fails it; a struct in that field order with `SetEscapeHTML(false)`
  passes; `TestPostRowMatchesArmJQ` is value-equal. Verdict: replace (struct,
  `int64` ts).
- `crew/internal/stall/probe/probe.go` — (c). `Probes.BusRows func(...)
([]jsonv.Value, bool)` (:46), a field type only; rows reach jqrun in stall,
  unread and nudge. Breaks: nothing here; stall tests stub the field with
  `[]jsonv.Value` (stall_test :125, release_test :268, probe_detectors_test
  :917, cancel_test :172). Verdict: replace (retype with tail.go's result).
- `crew/internal/stall/probe/tail.go` — (b) light, (c) via return type. `BusRows`
  (:38) reverse-scans the log tail: a line is a row iff `DecodeStream` yields
  exactly one value (`fromjson?`, :130); `Get("ts")`/`AsFloat` for the floor
  (:140); returns `[]jsonv.Value`. Breaks: `TestBusRowsRunFloor` (:51, string
  ts, no ts, torn tail, blank line) passes under `Unmarshal` into
  `any`/`RawMessage` with a trailing-data check; a NaN/Infinity row is dropped
  where `fromjson?` kept it (unpinned); the `wholeLogRead` (:264) and `tsOf`
  (:116) test helpers use jsonv. Verdict: small helper (one line to
  `json.Decoder` + `UseNumber`, rows as `[]any`, ts via `json.Number.Float64`)
  once jqrun accepts `any`; the decoder choice is D1.
- `crew/internal/stall/probe_detectors.go` — (c). `nudged` (:473-478)
  `jqrun.Run(nudgedProgram, rows, 0, {c,to: Str; m: Num})`, `out.Kind() ==
KindTrue`; `nudged.jq` is `any(.[]|objects; .crew_id==$c and .kind=="nudge"
and .to==$to and (.msg_ts//0)>=$m)`. Breaks: the `BusRows` signature (stubs
  in probe_detectors_test :917); `float64` is exact for a ms-epoch `m`.
  Verdict: keep (blocked on jqrun and `BusRows`); then small helper.
- `crew/internal/status/gate.go` — (c). `check` vars (`Str` x4, :77-82); `seam`
  (:112-130) passes each raw log line as `jsonv.Str(line)` to `g.o.Fold` and
  reads `AsFloat()==1`; jq's `_jqfromjson` decodes (seam.jq, deslop.jq,
  plan.jq). Breaks: the exported `Options.Fold` seam type (status.go :57) and the
  status diff corpus if jqrun's `_jqfromjson` stops using `ParseOne`;
  `TestSeamsRefuseLoneHighSurrogates` (:691), `TestPiReviewerVerdicts` (:634),
  `TestFoldErrors` (:744), `TestFoldsMatchJQ`. Verdict: keep until jqrun
  converts; then small helper (strings in, `float64` out).
- `crew/internal/status/msg.go` — (a), (b) at the FitLine seam. `RunMsg` builds
  the row (:43-50, `Object`/`Member`/`Str`/`Num` via `compact`); `body` is
  `jsonv.Str(b)`, a STRING holding caller JSON text (e.g.
  `{"seam":"review","review_mode":"full"}`), so the text passes through as one
  string; `post()` -> `bus.FitLine(build, full)` (status.go :127) measures
  ENCODED bytes against `LineMax` and calls `bus.Shrink`. Breaks: the encoded
  length moves where Go escapes `<>&` (6 bytes vs 1) and U+2028/9, and on
  invalid UTF-8 repair; `TestMsgLongBodyElided` (msg_test :97, `len(row) <=
bus.LineMax` plus marker, units x/é/✓) and bus append_test hold with
  `SetEscapeHTML(false)`; `TestMsgJSONBodyStaysText` (:40) holds with
  `Body string`; nothing pins the byte length at the boundary. Verdict: replace
  (struct with `Body string`, encoder with EscapeHTML off, `ts` computed once
  outside the builder as status.go :113 does).
- `crew/internal/status/status.go` — (a) rows, (b) `lastPosted`, (c) `Fold`
  seam. `RunStatus` builder (:185-204: ordered body object, conditional
  `detail`/`pr_url`/`restamp`, `Bool(true)`), `stamp` (:113), `member`/`compact`
  (:120-122), `Options.Fold` type and default `jqrun.Run(..., WithJQFromJSON())`
  (:57, :75), `lastPosted`/`bodyFields`/`interpolate` (:243-281) over
  `bus.ReadEventsTolerant` events (`ev.Raw.Kind()`, `Raw.Get("from")`,
  `Body.Kind()`, `pr.Or(Str(""))`, `compact` of a non-string). `detail` goes
  through FitLine/Shrink. Breaks: `TestTerminalDedupe` (status_test :299): "numeric
  pr_url renders as json" (:321, `{"pr_url":12}` -> key `exited\t12`; float64 of
  `12.0`/`1e1` would print `12`/`10`, jq prints the literal), "false pr_url is
  absent" (:319), "row after a parse error is unread" (:328), "newline in state:
  last line wins" (:329); `TestRowValues`, `TestRowOmitsEmptyDetailAndPR`,
  `TestLongDetailElided` are value-equal. Verdict: small helper (row builders ->
  structs with `SetEscapeHTML(false)`; `lastPosted` keeps jq truthiness and
  tolerance over `RawMessage`/`json.Number` once `bus.Event` moves; `Fold`
  becomes `func(prog string, lines []string, vars map[string]any) (any, error)`).
- `crew/internal/stream/files.go` — (a), (b) light. `member`, `encode`
  (compact), `decodeOne` (exactly one value), `decodeLines` (jq `-e -s` over a
  batch; any bad line fails all), `fieldText` (string, or a number's own text via
  `NumberText`, else `FormatComputed`), `digitsField` (digits-only -> `int64`).
  Breaks: `TestFieldTextAndDigits` (stream_test :325: `1700000000000` stays
  digits, `1.5` -> "1.5", null/object/missing -> absent): a `float64` decode
  turns a `1.7e12` tick into digits the jq-text rule rejects; use `UseNumber`.
  NaN: `FormatComputed(NaN)` -> "null" (not digits), `encoding/json` errors:
  both read as no tick. Verdict: replace (`Decoder.UseNumber`, strict single
  value; `fieldText` over `map[string]json.RawMessage`).
- `crew/internal/stream/fleet.go` — (a). Heartbeat line `encode(Object(stream,
crew, quiet_s, ts))` (:210-215); `encode(pid)` inside a stderr message (:377,
  via `laneState`). Breaks: tests/crew.bats :9319-9322 and :9929
  (`grep '"stream":"heartbeat"'`, `'"stream":"hold_due"'`) need a compact line
  with the `stream` key (any compact encoder; a map is fine); fleet_test
  :726-740 reads via a map. The agent-facing stderr `per-repo stream alive (pid
%s)` (:377) prints jsonv's number text (`4242`, `null`; pid `00` prints `0`,
  `007` prints `7`, a 20-digit text stays), so use a leading-zero-stripping text
  helper, not `uint64` (edge only, unpinned). Verdict: replace.
- `crew/internal/stream/stream.go` — (a), (b) light, one (c). `status` line
  (:245-251), `laneState` (:258-278: `ParseNumber(holder)`, `Null`, `Num`),
  heartbeat/error/hold_due/reap lines (:599-720), `writeTick` (:806), `emit`
  (:814-818, `line.Set("repo", ...)`), `printBatch` (:826-832: decode -> `Set`
  repo -> encode, verbatim on failure), `printReap` (:755, `fieldText`
  suppression key), `holdIDs` (:878), `wantsReap` (:853-863: `decodeLines` +
  `jqrun.Run(wantsReapProg, rows, 0, nil)` + `Truthy()`). Row bodies are plain
  strings or objects; `holds` embeds `crew hold due --json` output unchanged
  (`json.RawMessage`). Breaks: `TestStatusHolders` (:133, pid `4294967295`,
  `0`, `abc` -> null), `TestWantsReap` (:270: "not json" -> false, two lines
  one terminal, a pr-watch `msg` whose `body` is a JSON string parsed by
  gojq's `fromjson?`; `wants_reap.jq` returns false on an index error, which a
  port must mirror), `TestFieldTextAndDigits`; `printBatch` would re-emit
  verbatim, unlabelled, a batch jsonv accepts and `encoding/json` rejects (NaN,
  lone surrogate): the inner batch is produced by `crew watch` itself, so
  unreachable. Verdict: replace everything but `wantsReap` (small helper, or a
  typed Go port of `wants_reap.jq`).
- `crew/internal/testjson/testjson.go` — (t). `MustParse`/`Compact` (:16-58) over
  `DecodeStream`, `Append`, `Kind*`, `Num`, `Array`/`Object`/`Member`,
  `Elems`/`Members`/`AsFloat`: a key-order- and literal-free comparison helper
  for the value-equal contract; twelve test files import it (inbox_test :579
  `MustParse("NaN")`, marks_test :42, :387, :424). Breaks: n/a (tests only);
  `MustParse("NaN"/"Infinity")` in inbox_test (:579-581) and the `jsonv.Value` literals in marks and ledger tests
  need a replacement (`json.Unmarshal` + `reflect.DeepEqual`, or a canonical
  `Marshal` of `any`). Verdict: replace last, after its consumers: a `~10`-line
  `Equal(t, got, want string)` over `any` with `UseNumber`; the NaN cases go
  with D1.
- `crew/internal/unread/unread.go` — (c), (b). `rows` from `probe.BusRows`
  (:72), vars `Str`/`Num` (:73-81, `jsonv.Num(float64(r.T0))`), `marks.Read` as
  `$marks` (:82), `out.AsString()`: `unread.jq` compares `.ts > ($marks[.from] //
0)` and prints `min_by(.ts).ts` by string interpolation, so literal text and
  type ordering reach the output. Breaks: no test uses NaN, big or float ts
  (unread_test rows are `%d`, :42, :46); `TestScan` "junk lines drop out, a torn
  tail alone" (:164) and "a bare-string row silences the scan" (:171) depend on
  `BusRows` dropping exactly what `fromjson?` dropped; a ts of `1.0e3` prints
  `1.0E+3` via `json.Number` text, `1000` via `float64`. Verdict: replace after
  `probe.BusRows` and `marks.Read` are retyped; the decoder follows D1.
- `crew/internal/watch/watch.go` — (c), (a), (b) number text. `Encode(stdout,
out, Options{})` (:158); `fold()` (:267-291) raws, `states` `Array(Str...)`,
  `Num(float64(c.since))`, `Kind()==KindObject`; `cursorText` (:294-300) `Get
("cursor")` + `Encode`. The stdout batch `{"cursor","events":[raw rows]}` is
  jqrun output written verbatim; the cursor file holds the cursor's number text
  and `seedCursor` reads it back as all-digits. Breaks: watch_test :149
  expects cursor file `1785951264000` (`float64` prints the same for integers <
  1e21); a `ts` of `1e18`/`1.5e12` prints `1E+18` (`json.Number` text) or
  `1000000000000000000` (`float64`): only the former fails the digits check
  (unpinned); a NaN bus row makes `bus.ReadEvents` fail, so the fold reports
  unreadable (D1); test helpers (:831-854, :1002-1010) drive jqrun with jsonv.
  Verdict: replace behind the marshaller; `cursorText` -> `json.Number.String()`
  (small helper if a float must print as an integer).
- `crew/main.go` — (b) colour and validity, (a), (c). `jsonv.ParseJQColors` at
  :536, :550, :568, :584, :634 (validity only: crews, inbox, await, watch,
  hold), :808 (log palette), :846 (roster/sessions), :923 (fleet);
  `Options{Indent: pretty}` + `Encode` (:850-871); a `[]jsonv.Value` callback in
  `fold()` (:855, :978-990); `jsonv.Array()` for the unreadable-sessions branch
  (:866). `.ok` feeds `JQColorsInvalid` and the `Failed to set $JQ_COLORS`
  stderr line; the palette feeds `-C`. Breaks: main_test :100 ("sessions
  unreadable log is coloured on a terminal", stdout `\x1b[1;39m[]\x1b[0m\n`),
  :101 ("honours JQ_COLORS", `\x1b[4;35m[]\x1b[0m\n`), :102 and :188-196 (bad
  `JQ_COLORS` warning), plus crews_test :180-186, report_test :264-276,
  log_test :248 and :255, await_test :530-548, inbox_test :426-447. Pretty/compact
  itself breaks nothing (`[]`, roster pretty). Verdict: small helper for the
  validity check (and the palette only if colour stays, D2); everything else
  replace.

## Converted

`internal/prwatch/prwatch.go` (this PR). Was all class (a): 8 lines (16
occurrences) of jsonv, one row builder (`ts` `int64` from `clock.RealMS`,
`crew_id`, `from` `"pr-watch:"+PR`, `to` `"dispatcher:"+crew`, `kind` `"msg"`,
`body` the child's stdout minus a trailing newline, a plain string), written
with `bus.Append`, not `FitLine`/`Shrink`, no length cap. No jsonv type in the
exported API. Now `encoding/json` with a struct in the same key order.

Consumers of the row, none reads its bytes:

| consumer                                                                              | reads                                                                                                                                                          |
| ------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `crew/internal/stream/wants_reap.jq` :13-16 (gojq)                                    | by value: `.kind`, `.from \| startswith("pr-watch:")`, `.body \| fromjson?`, `.changed`, `.state.state`; stream_test :281-283 uses literal rows                |
| `internal/prwatch/prwatch_test.go` (`TestEvent`, the `from` checks)                   | by value: `json.Unmarshal` into a map, `body` compared to the event exactly (incl. quote, backslash, `<>&`, control, U+2028, non-ASCII); `from` via `testjson` |
| `tests/pr-watch.bats` :244-247                                                        | `jq -r` with `.body \| fromjson \| .changed[0]` (by value)                                                                                                     |
| `tests/harness/shellspec/case-runner.sh` :490-493                                     | same jq, by value                                                                                                                                              |
| `tests/harness/pytest/test_manifest.py` :283-287                                      | `json.loads(line)`, `json.loads(row["body"])`                                                                                                                  |
| `tests/harness/go/harness_test.go` :641-646 (`eventRow` :686-708)                     | `json.Unmarshal` per line; `body` must be a string                                                                                                             |
| `tests/harness/assertions.tsv` :116                                                   | doc row of the bats line, not run against bytes                                                                                                                |
| generic bus readers (`watch`, `await`, `inbox`, `unread`, `log` via `bus.ReadEvents`) | by value; `crew log` prints lines verbatim for display only                                                                                                    |
| `DISPATCHER_PROTOCOL.md` :1456-1470 (four copies under `adapters/`)                   | prose telling the dispatcher the body is event JSON with `changed[]`; read as text, no byte contract                                                           |

`docs/crew-go-port.md` mentions only the child's error wording (:724, :967).

Disposition:

- HTML escaping (`<`, `>`, `&`, U+2028/9) is value-equal and the row skips
  `FitLine`, so its byte length is not measured; no decision needed.
- Invalid UTF-8 repair differs: jq repairs a truncated or encoded-surrogate
  sequence to one U+FFFD, `encoding/json` emits one per byte. The shipped
  producer, `adapters/core/pr-watch.sh` (nix `writeShellApplication`,
  flake.nix :159-162), emits its single success line through `jq -nc` (:211-217;
  `--arg` bytes are repaired, `--argjson` values are `jq -c` fingerprints built at
  :114-137), so stdout is valid UTF-8. A timeout prints nothing on stdout (:226
  is stderr) and a failure exits non-zero, so no row is posted. Only a foreign
  `pr-watch` earlier on PATH or an invalid-UTF-8 argv PR value could reach the
  divergence; accepted.

## Conversion plan

One PR per row (or per "moves with" set), in this order. Targets: typed
structs; `encoding/json`; `any` is gojq's form (`json.Number` for number
literals, `float64` for NaN and +-Inf, `string`, `bool`, `nil`, `[]any`,
`map[string]any`).

Judgment call: `jqrun.Run` and `bus.Event` are read by about 25 packages, so
changing them in place would force one big PR. Steps 2 and 3 therefore add
`any`-typed entry points and `Event.Doc` beside the jsonv-typed ones (jqrun's
`toGo`/`fromGo` already are the adapters), so each package converts alone. The
decoder swap in the bus and the jsonv-typed entry points in jqrun are removed
last, which isolates D1 to one step.

| order | package      | target                                                                                                                                                                                                                                            | depends on / moves with                                                               | notes                                                                                                                                                                                                                                                      |
| ----- | ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 0     | prwatch      | done: struct + `encoding/json`                                                                                                                                                                                                                    | none                                                                                  | see Converted                                                                                                                                                                                                                                              |
| 1     | clock        | `NowF() float64`                                                                                                                                                                                                                                  | none                                                                                  | callers hold (:216, :260) wrap `jsonv.Num(f)` until step 18; rosterrender reads the float; test reads a float compare                                                                                                                                      |
| 2     | jqrun        | A: `any` entry points beside the jsonv ones, export the normaliser; B (step 34): delete the jsonv-typed ones                                                                                                                                      | none for A; B after 4-32                                                              | keep the `json.Number`/`float64` normaliser, the clamping marshal and the `ParseOne` equivalent for `_jqfromjson`; re-pin `TestNumberLiteralsRoundTripExactly`, `TestComputedNumbersStayComputed`, `TestToGoNaNStaysFloat`, `TestJQFromJSON` on the helper |
| 3     | bus          | A: `Event.Doc any` (gojq form) next to `Raw`/`TS`/`Body`; B (step 33): swap the decoder to a Decoder loop (or the tolerant helper), drop `Raw`/`TS`/`Body`                                                                                        | A after 2; B after 4-32 and D1                                                        | B keeps prefix-on-error, `DecodeError` mapping, BOM trim, `UseNumber`; `Shrink` becomes an order-preserving token walker with `SetEscapeHTML(false)` (or stays on jsonv)                                                                                   |
| 4     | stack        | align/cut: typed gh struct + row structs, `int64` ts, field order `pr, branch, parent, parent_pr, cut, mode, ready, directive, reason`; stack.go: strings in, `any` out                                                                           | stack.go after 2; align and cut can land before                                       | `_jqfromjson` stays in jqrun; drop `member`/`jsonvAppend`/`jqTostring`                                                                                                                                                                                     |
| 5     | stream       | structs, `json.RawMessage` for `holds`, `json.Number` ticks                                                                                                                                                                                       | `wantsReap` after 2                                                                   | `leading-zero-stripping` pid text helper for stderr; `fieldText` over `map[string]json.RawMessage`; heartbeat line stays compact (bats greps)                                                                                                              |
| 6     | rate         | burn rules decoded as `[]json.RawMessage` rows of `map[string]json.RawMessage`, parsed per field, `any` for the store, `MarshalIndent`                                                                                                            | after 2, 3 (A)                                                                        | `appendBatch` with `SetEscapeHTML(false)`; non-nil `[]` for empty folds                                                                                                                                                                                    |
| 7     | piagentdir   | `map[string]json.RawMessage` settings, `MarshalIndent`; keep exit-2 text                                                                                                                                                                          | folds after 2                                                                         | `ParseOne` -> `Unmarshal` into `any`; surrogate rule not needed (Go config)                                                                                                                                                                                |
| 8     | sessions     | `Fold([]any, ...) (any, error)`                                                                                                                                                                                                                   | after 2, 3                                                                            | callers nudge (nudge.go :384), reply (:214), main.go (:859) adapt at the call site; tests: `TestFold` cases re-decode with `UseNumber`                                                                                                                     |
| 9     | resolve      | `Rows() []any` (`UseNumber`), `Resolve([]any, ...)`                                                                                                                                                                                               | after 2; callers where (:203), nudge arm.go (:231)                                    | decoder per line follows D1                                                                                                                                                                                                                                |
| 10    | reply        | row struct `{TS int64; CrewID, From, To, Kind, Body string}`, `body` last, `SetEscapeHTML(false)` for FitLine; `text()` -> `jqText(any)`                                                                                                          | row any time; `text()` and the session lookup after 3 (A), 8                          | `TestOversizedBodyIsElided` suffix test pins field order                                                                                                                                                                                                   |
| 11    | marks        | `Read() any`, `Record(msgs []any)`, `json.Marshal` for the write                                                                                                                                                                                  | after 2; moves with callers inbox (call sites), await, unread                         | jq programs unchanged                                                                                                                                                                                                                                      |
| 12    | stall/probe  | `BusRows(...) ([]any, bool)` (`UseNumber`), ts via `json.Number.Float64`                                                                                                                                                                          | after 2; moves with stall, unread, nudge (`Rows`) and the four test stubs             | decoder per line follows D1                                                                                                                                                                                                                                |
| 13    | stall        | budget: `any` + type assertions; post: struct (`ts, crew_id, from, to, kind, body{state,detail,source}`), `SetEscapeHTML(false)`; bus.go and probe_detectors.go: `[]any` rows                                                                     | budget and post any time; rest after 12                                               | `TestPostRow` is byte-exact: keep field order and raw `é`                                                                                                                                                                                                  |
| 14    | unread       | `any` rows and marks                                                                                                                                                                                                                              | after 11, 12                                                                          | `TestScan` junk-line cases depend on the decoder (D1)                                                                                                                                                                                                      |
| 15    | nudge        | row structs (`msg_ts` as `json.Number(digitsLiteral(ts))` in both nudge.go and arm.go `waitRow`), `Append` takes the struct, `Events`/`Rows` -> `[]any`                                                                                           | after 8, 9, 12                                                                        | arm.go folds keep gojq; `rc` via `strconv.Itoa`; wire.go is the mechanical follow-on                                                                                                                                                                       |
| 16    | await        | `any` rows, `marks` any                                                                                                                                                                                                                           | after 11                                                                              | per-line decode follows D1; drop `Options` colour per D2                                                                                                                                                                                                   |
| 17    | identity     | `Identity{Name, Tmux string; Color json.RawMessage}`, `Palette() []string`, `For() Identity`, `Assign`                                                                                                                                            | after 3 (A), 2; moves with roster, rosterrender, where                                | `TestRecorded` keeps "color passes through as any json" (RawMessage), non-string name rejects                                                                                                                                                              |
| 18    | hold         | write: ordered `holdRow`/`holdBody` structs, `*string` for `plan`, `mcp`, `shape`, `spec`, `SetEscapeHTML(false)` on both the body marshal and the row; read: `Outstanding([]any, crew) (any, error)`                                             | write any time; read after 1, 2, 3 (A)                                                | body is JSON text in a string: key order pinned by `TestAddRowMatchesTheBuilder`; `resets_at` is `int64`; string encoder per D4                                                                                                                            |
| 19    | roster       | `Fold([]any, ...) ([]map[string]any, error)`; live.go over `map[string]any` or `{Branch, State string; AgeS *float64}`                                                                                                                            | after 17, 2, 3                                                                        | clamping marshal for `TestFold` infinity cases; consumers fleet, rosterrender, main.go                                                                                                                                                                     |
| 20    | rosterrender | `model` -> `map[string]any`; `roleEngine` -> `map[string]struct{Agent string}`; `hexJSON` -> typed map                                                                                                                                            | after 1, 17, 18, 19                                                                   | `liveCount` reads a float from `any`; NaN `roles.json` case follows D1                                                                                                                                                                                     |
| 21    | fleet        | `[]map[string]any`, `MarshalIndent`, drop `Options.Colors` (D2)                                                                                                                                                                                   | after 3, 19                                                                           | `table()` reads from `any`                                                                                                                                                                                                                                 |
| 22    | crews        | `[]any` meta rows (`pid`/`alive` nil when unknown)                                                                                                                                                                                                | after 3                                                                               | stats stay `any`                                                                                                                                                                                                                                           |
| 23    | report       | `any` rows                                                                                                                                                                                                                                        | after 3                                                                               | none                                                                                                                                                                                                                                                       |
| 24    | retro        | `any` rows, `MarshalIndent`                                                                                                                                                                                                                       | after 3                                                                               | prefix `{\n  ` and suffix `}\n` tests pass                                                                                                                                                                                                                 |
| 25    | reap         | occupants: non-nil `[]occupantJSON`, `*string` pane; remove: struct with `*string` `discarded` and optional `issues`; release row: struct with non-nil `windows`; folds and transcript filter: `any`                                              | rows any time; folds, forget, `newerUserTurn` after 2, 3                              | `discarded:""` must still print; `ParseOne(ts)` -> `ParseOne` helper; `! grep '"kind":"reap"'` bats stay valid                                                                                                                                             |
| 26    | adopt        | `claimRows` over `ev.Doc`, `fieldText` over gojq output                                                                                                                                                                                           | after 2, 3                                                                            | `"issue":73.0` prints `73` under float64: use `json.Number`                                                                                                                                                                                                |
| 27    | status       | msg/status rows -> structs (`Body string` for msg; ordered body struct for status), `SetEscapeHTML(false)`; `Options.Fold` -> `func(prog string, lines []string, vars map[string]any) (any, error)`; `lastPosted` over `RawMessage`/`json.Number` | rows any time with the FitLine rule; gate.go and `Fold` after 2; `lastPosted` after 3 | `TestTerminalDedupe` needs the number's own text; `TestFoldsMatchJQ` moves with jqrun                                                                                                                                                                      |
| 28    | ledger       | `RawMessage` body + `json.Compact`, per-byte repair for `jqArg`                                                                                                                                                                                   | none                                                                                  | only if the oracle fuzz stays green (D3); else keep jsonv                                                                                                                                                                                                  |
| 29    | watch        | `any` + marshaller; `cursorText` via `json.Number.String()`                                                                                                                                                                                       | after 2, 3                                                                            | test helpers re-type with jqrun                                                                                                                                                                                                                            |
| 30    | log          | print `Event.Doc`/`Raw` through the marshaller; drop `Options.Colors` (D2)                                                                                                                                                                        | after 3                                                                               | `TestFiltersOneCrewByteForByte` becomes a `jq -S` compare; big ints stay `json.Number`                                                                                                                                                                     |
| 31    | inbox        | `json.Number` text compare + type-rank switch; marks any; print via the marshaller                                                                                                                                                                | after 3, 11; D1 for the NaN/Infinity `TestSince` cases                                | `TestPrintsTheRowUnchanged` becomes value-equal                                                                                                                                                                                                            |
| 32    | main.go      | validity check only (or drop with D2); `fold()` callbacks over `any`; pretty via `MarshalIndent`                                                                                                                                                  | after 21, 30 and every `ParseJQColors` user                                           | main_test coloured cases deleted or kept per D2                                                                                                                                                                                                            |
| 35    | testjson     | `Equal(t, got, want string)` over `any` with `UseNumber`, or delete                                                                                                                                                                               | after step 34                                                                         | NaN/Infinity cases go with D1                                                                                                                                                                                                                              |

Steps 33 and 34 are the B halves of rows 3 and 2: 33 swaps the bus decoder
(the only step that changes NaN behaviour on bus rows, D1); 34 deletes the
jsonv-typed jqrun entry points. Row 35 follows.

## What jsonv must still provide

After step 35 the importers are gone and what survives depends on D1 and D2:

- Kept in either case (about 100 lines, moved into `jqrun` or a small
  `jsonval` package): a clamping marshaller (NaN to `null`, +-Inf to
  +-`1.7976931348623157e308`, `json.Number` verbatim, no HTML escaping) and a
  `ParseOne`-equivalent for `_jqfromjson` (lone-high-surrogate error; `nan`,
  `-nan`, `snan`, `nan0` to NaN `float64`; `Infinity`/`inf` to +-Inf; BOM trim;
  `+1`/`.5`/`01`; or the jsonv parser itself). Both are pinned by jqrun tests,
  the status/stack surrogate tests and the jq-oracle `TestFoldsMatchJQ`
  (skipped without jq on PATH).
- If D1 keeps NaN/Infinity tolerance on bus rows: a stream decoder producing
  `any` with `json.Number` literals, BOM trim, `+1`/`.5`/`01` tolerance and the
  torn-tail prefix (today's `decode.go` parser retargeted from `Value` to
  `any`, about 340 lines).
- If D2 keeps the `Failed to set $JQ_COLORS` warning: `ParseJQColors` as a
  validity check only (about 15 lines).

Deletable: the `Value`/`Kind`/`Member` model and its accessors, `Object`/`Array`
builders, dup-key folding (maps do it), the decNumber canonicaliser
(`parseDecimal`, `1e2` -> `1E+2`), `FormatComputed`, the invalid-UTF-8 repair
on encode (`validUTF8`, unless D3 keeps it for `ledger`), the pretty and
colour encoder (about 170 lines) if colour is dropped, and the
`DefaultPalette`/`Palette` API with it.

Decisions for the owner:

- D1. Must a bus row, `fromjson?`-style per-line input or `gh`/store text
  containing `NaN`, `Infinity` or `-nan` still decode? No shipped writer
  emits them (`jq -c` writes `null`; the Go writers clamp), so dropping costs
  only hand-edited or foreign lines: a NaN row in the bus becomes a decode
  error for the tail after it (as a torn tail does today), and per-line readers
  (`await`, `resolve`, `probe.BusRows`, `unread`) skip it. Dropping removes the
  inbox "NaN never"/"Infinity always" cases, the roster and sessions NaN input
  cases and the `Shrink` `[NaN]` branch, and lets the bus decoder be a Decoder
  loop.
- D2. Drop `$JQ_COLORS` colouring and the `Failed to set $JQ_COLORS` warning?
  The port doc already lists TTY colouring and the warning count as not
  mirrored; seven test files (main, report, log, crews, inbox, await, hold)
  assert the warning, `retro_test` asserts none, `main_test` asserts coloured
  `[]`, and `internal/watch/watch.go` (:142) also emits the warning (no test).
- D3. `ledger`'s waiver gate: convert only if the jq-oracle fuzz stays green on
  the current corpus (it covers `1.50`, key order, a two-value line and
  `"body":false`, not invalid UTF-8 or `1e3`), accepting duplicate-key, DEL and
  invalid-UTF-8 differences in a security-relevant check, or keep jsonv for
  this one file.
- D4. `hold`'s inner `body` string (JSON text in a string): add a jq-style
  string encoder (DEL as `\u007f`, U+2028/9 raw, jq's invalid-UTF-8 repair) or
  accept `encoding/json`'s differences: DEL and U+2028/9 are value-equal after
  `fromjson`, invalid-UTF-8 repair is not (argv titles), and all three change
  the string text and the `FitLine` length at the boundary.
