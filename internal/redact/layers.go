package redact

import (
	"regexp"
	"strings"
)

// ── Layer 1: Shannon entropy ────────────────────────────────────────────────

var (
	digestPrefix = regexp.MustCompile(`^sha(?:1|256|384|512)-`)
	embeddedUUID = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
)

const (
	entropyMinLen = 20
	entropyMin    = 3.5 // bits per character
)

// tokenChar marks base64/base64url characters.
var tokenChar = func() (t [256]bool) {
	for _, c := range []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/_-") {
		t[c] = true
	}
	return t
}()

// tokenRuns finds runs of at least entropyMinLen base64 characters, each with
// up to two '=' of padding.
func tokenRuns(text string) [][2]int {
	var out [][2]int
	for i := 0; i < len(text); {
		if !tokenChar[text[i]] {
			i++
			continue
		}
		j := i
		for j < len(text) && tokenChar[text[j]] {
			j++
		}
		if j-i >= entropyMinLen {
			k := j
			for k < len(text) && k < j+2 && text[k] == '=' {
				k++
			}
			out = append(out, [2]int{i, k})
		}
		i = j
	}
	return out
}

func entropySpans(text string) []span {
	var out []span
	for _, m := range tokenRuns(text) {
		tok := strings.TrimLeft(text[m[0]:m[1]], "/-_+")
		start := m[1] - len(tok)
		if digestPrefix.MatchString(tok) || strings.HasSuffix(text[:start], "h1:") {
			continue // npm/SRI integrity and Go module hashes are digests, not secrets
		}
		if randomLooking(tok) {
			out = append(out, span{start, m[1], Entropy})
		}
	}
	return out
}

// randomLooking decides whether a token is a random secret rather than a
// word, identifier, path, hash or id.
func randomLooking(tok string) bool {
	core := strings.TrimRight(tok, "=")
	// A UUID is an identifier, not a secret: judge what is left around it
	// ("claude-code/4b7a3c2e-…" in a session URI).
	if embeddedUUID.MatchString(core) {
		rest := embeddedUUID.ReplaceAllString(core, "")
		return len(strings.Trim(rest, "/-_+")) >= entropyMinLen && randomLooking(rest)
	}
	if len(core) < entropyMinLen || heldOut(core) || pureHex.MatchString(core) || isGitSHA(core) {
		return false
	}
	if !strings.ContainsAny(core, "0123456789") || strings.IndexFunc(core, isLetter) < 0 {
		return false
	}
	if pathLike(core) || wordy(core) {
		return false
	}
	return shannon(core) >= entropyMin
}

func isLetter(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') }

// pathLike reports a slash-separated path whose segments are words, names or
// numbers rather than random text.
func pathLike(s string) bool {
	if strings.Count(s, "/") < 2 {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if len(seg) >= 12 && !wordy(seg) {
			return false
		}
	}
	return true
}

// ── Layer 3: provider prefixes ──────────────────────────────────────────────

// providerPatterns match a known provider's token format quoted alone, where a
// vendored rule may want surrounding context. Each runs only when one of its
// literal prefixes occurs in the text.
var providerPatterns = compileAll([]providerPattern{
	{[]string{"sk-ant-"}, `sk-ant-(?:api|admin|oat|ort)[0-9]{2}-[A-Za-z0-9_-]{20,}`},
	{[]string{"sk-"}, `sk-(?:proj-|svcacct-|admin-|or-v1-)?[A-Za-z0-9_-]{20,}`},
	{[]string{"_live_"}, `(?:sk|rk|pk)_live_[A-Za-z0-9]{16,}`},
	{[]string{"_test_"}, `(?:sk|rk)_test_[A-Za-z0-9]{16,}`},
	{[]string{"whsec_"}, `whsec_[A-Za-z0-9+/=]{20,}`},
	{[]string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"}, `gh[pousr]_[A-Za-z0-9]{30,}`},
	{[]string{"github_pat_"}, `github_pat_[A-Za-z0-9_]{40,}`},
	{[]string{"gl"}, `gl(?:pat|ptt|dt|rt|oas|cbt|ft|imt|agent|soat)-[A-Za-z0-9_-]{20,}`},
	{[]string{"xox"}, `xox[abposre]-[A-Za-z0-9-]{10,}`},
	{[]string{"xapp-"}, `xapp-[0-9]-[A-Za-z0-9-]{10,}`},
	{[]string{"sb_secret_"}, `sb_secret_[A-Za-z0-9_-]{16,}`},
	{[]string{"sbp_"}, `sbp_[a-f0-9]{40}`},
	{[]string{"AKIA", "ASIA", "ABIA", "ACCA", "A3T"}, `(?:AKIA|ASIA|ABIA|ACCA|A3T[A-Z0-9])[A-Z2-7]{16}`},
	{[]string{"AIza"}, `AIza[0-9A-Za-z_-]{35}`},
	{[]string{"ya29."}, `ya29\.[0-9A-Za-z_-]{20,}`},
	{[]string{"1//0"}, `1//0[0-9A-Za-z_-]{30,}`},
	{[]string{"npm_"}, `npm_[A-Za-z0-9]{36}`},
	{[]string{"pypi-AgE"}, `pypi-AgE[A-Za-z0-9_-]{50,}`},
	{[]string{"hf_"}, `hf_[A-Za-z]{30,}`},
	{[]string{"gsk_"}, `gsk_[A-Za-z0-9]{40,}`},
	{[]string{"xai-"}, `xai-[A-Za-z0-9]{40,}`},
	{[]string{"r8_"}, `r8_[A-Za-z0-9]{30,}`},
	{[]string{"pplx-"}, `pplx-[A-Za-z0-9]{40,}`},
	{[]string{"dop_v1_", "doo_v1_", "dor_v1_"}, `do[por]_v1_[a-f0-9]{64}`},
	{[]string{"shp"}, `shp(?:at|ss|ca|pa)_[a-fA-F0-9]{32}`},
	{[]string{"lin_"}, `lin_(?:api|oauth)_[A-Za-z0-9]{30,}`},
	{[]string{"ATATT"}, `ATATT[A-Za-z0-9_=-]{30,}`},
	{[]string{"SG."}, `SG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}`},
	{[]string{"AGE-SECRET-KEY-1"}, `AGE-SECRET-KEY-1[0-9A-Z]{50,}`},
	{[]string{"hvs.", "hvb."}, `hv[sb]\.[A-Za-z0-9_-]{24,}`},
	{[]string{"dckr_pat_"}, `dckr_pat_[A-Za-z0-9_-]{20,}`},
	{[]string{"nfp_"}, `nfp_[A-Za-z0-9]{30,}`},
	{[]string{"fo1_"}, `fo1_[A-Za-z0-9_-]{30,}`},
	{[]string{"sntrys_"}, `sntrys_[A-Za-z0-9+/=_-]{40,}`},
	{[]string{"PMAK-"}, `PMAK-[a-f0-9]{24}-[a-f0-9]{34}`},
	{[]string{"rubygems_"}, `rubygems_[a-f0-9]{48}`},
	{[]string{"ops_eyJ"}, `ops_eyJ[A-Za-z0-9+/=]{100,}`},
})

type providerPattern struct {
	prefixes []string
	pattern  string
}

type compiledProvider struct {
	prefixes []string
	re       *regexp.Regexp
}

func compileAll(pats []providerPattern) []compiledProvider {
	out := make([]compiledProvider, len(pats))
	for i, p := range pats {
		// The token must not continue a longer word on either side. Group 1 is the token.
		out[i] = compiledProvider{p.prefixes, regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])(` + p.pattern + `)`)}
	}
	return out
}

func providerSpans(text string) []span {
	var out []span
	for _, p := range providerPatterns {
		if !containsAny(text, p.prefixes) {
			continue
		}
		for _, m := range p.re.FindAllStringSubmatchIndex(text, -1) {
			s, e := m[2], m[3]
			if heldOut(text[s:e]) {
				continue
			}
			out = append(out, span{s, e, ProviderToken})
		}
	}
	return out
}

func containsAny(text string, subs []string) bool {
	for _, s := range subs {
		if strings.Contains(text, s) {
			return true
		}
	}
	return false
}

// ── Layer 4: credentialed URIs ──────────────────────────────────────────────

// credURI matches scheme://user:password@host; group 1 is the password.
var credURI = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9+.-]{1,30}://[^\s/?#@:"'<>\x60]*:([^\s/?#@"'<>\x60]+)@[^\s/?#@"'<>\x60]`)

func credentialedURISpans(text string) []span {
	if !strings.Contains(text, "://") || !strings.Contains(text, "@") {
		return nil
	}
	var out []span
	for _, m := range credURI.FindAllStringSubmatchIndex(text, -1) {
		s, e := m[2], m[3]
		if heldOut(text[s:e]) || shortNumber.MatchString(text[s:e]) { // host:port@ confusion, not a password
			continue
		}
		out = append(out, span{s, e, CredentialedURI})
	}
	return out
}

// queryCred matches a credential query parameter inside a URL; group 1 is the value.
var queryCred = regexp.MustCompile(`[?&]([A-Za-z0-9_.-]+)=([^&#\s"'<>\x60]+)`)

func queryParamSpans(text string) []span {
	var out []span
	for _, m := range queryCred.FindAllStringSubmatchIndex(text, -1) {
		key, s, e := text[m[2]:m[3]], m[4], m[5]
		if credentialKey(key) && !heldOut(text[s:e]) {
			out = append(out, span{s, e, CredentialValue})
		}
	}
	return out
}

// ── Layer 5: connection strings ─────────────────────────────────────────────

var (
	jdbcPassword = regexp.MustCompile(`(?i)jdbc:[a-z0-9:]+[^\s"'\x60]*?[?&;:]password=([^&;\s"'\x60]+)`)
	// Semicolon form (ADO.NET, ODBC, Azure storage): only on a line that names a server.
	semicolonServer = regexp.MustCompile(`(?i)(?:^|[;"'\s])(?:server|data source|host|address|addr|user id|uid|initial catalog|database|driver|accountname|endpoint|defaultendpointsprotocol)\s*=`)
	semicolonSecret = regexp.MustCompile(`(?i)(?:^|[;"'\s])(?:password|pwd|accountkey|sharedaccesskey|sharedaccesssignature)\s*=\s*([^;"'\r\n]+)`)
	// Keyword DSN (libpq): host=… user=… password=…
	dsnHost     = regexp.MustCompile(`(?i)(?:^|\s|["'])(?:host|hostaddr)=\S`)
	dsnOther    = regexp.MustCompile(`(?i)(?:^|\s)(?:dbname|user|port|sslmode)=\S`)
	dsnPassword = regexp.MustCompile(`(?i)(?:^|\s)password=('(?:[^'\\]|\\.)*'|[^\s'"]+)`)
)

func connectionStringSpans(text, lower string) []span {
	var out []span
	add := func(s, e int) {
		v := text[s:e]
		if t := strings.Trim(v, "'"); len(t) < len(v) {
			s += strings.Index(v, t)
			v = t
		}
		v = strings.TrimRight(v, " \t")
		e = s + len(v)
		if !heldOut(v) {
			out = append(out, span{s, e, ConnectionString})
		}
	}
	if strings.Contains(lower, "jdbc:") {
		for _, m := range jdbcPassword.FindAllStringSubmatchIndex(text, -1) {
			add(m[2], m[3])
		}
	}
	if !containsAny(lower, []string{"password", "pwd", "accountkey", "sharedaccess"}) {
		return out
	}
	offset := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		ll := lower[offset : offset+len(line)]
		if !containsAny(ll, []string{"password", "pwd", "accountkey", "sharedaccess"}) {
			offset += len(line)
			continue
		}
		if strings.Contains(line, ";") && semicolonServer.MatchString(line) {
			for _, m := range semicolonSecret.FindAllStringSubmatchIndex(line, -1) {
				add(offset+m[2], offset+m[3])
			}
		}
		if dsnHost.MatchString(line) && dsnOther.MatchString(line) {
			for _, m := range dsnPassword.FindAllStringSubmatchIndex(line, -1) {
				add(offset+m[2], offset+m[3])
			}
		}
		offset += len(line)
	}
	return out
}

// ── Layer 6: credential key/value pairs ─────────────────────────────────────

// kvPair finds key<sep>value: KEY=value, key: value, "key": "value",
// --flag=value. Group 1 is the key, group 2 the separator.
var kvPair = regexp.MustCompile(`(?:^|[^A-Za-z0-9_./-])["']?([A-Za-z_][A-Za-z0-9_.-]{1,80})["']?([ \t]*(?::=|=|:)[ \t]*)`)

// flagPair finds --flag value with a space separator.
var flagPair = regexp.MustCompile(`(?:^|\s)--?([A-Za-z][A-Za-z0-9_-]{1,60})\s+`)

// credentialHints are substrings every credential key contains (lower case).
var credentialHints = []string{"pass", "pwd", "secret", "token", "key", "auth", "cred", "bearer", "cookie"}

// credentialValueSpans runs the pair patterns only over lines that could hold
// a credential key. lower is asciiLower(text).
func credentialValueSpans(text, lower string) []span {
	var out []span
	offset := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		if containsAny(lower[offset:offset+len(line)], credentialHints) {
			for _, sp := range credentialValueSpansLine(line) {
				out = append(out, span{sp.start + offset, sp.end + offset, sp.cat})
			}
		}
		offset += len(line)
	}
	return out
}

func credentialValueSpansLine(text string) []span {
	var out []span
	for _, m := range kvPair.FindAllStringSubmatchIndex(text, -1) {
		key, sep := text[m[2]:m[3]], text[m[4]:m[5]]
		if !credentialKey(key) || fileName(key) {
			continue
		}
		// Comparisons (==, =>), and paths (Type::method) are code, not assignments.
		if rest := text[m[5]:]; strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, ">") || strings.HasPrefix(rest, ":") {
			continue
		}
		if s, e, ok := valueAt(text, m[5]); ok {
			// A bare word after "key: " or "key = " is prose ("Bearer = session
			// token", "on a token: write …"); an assignment without spaces is not.
			if proseWord.MatchString(text[s:e]) && (strings.Contains(sep, ":") || strings.TrimSpace(sep) != sep) {
				continue
			}
			out = append(out, span{s, e, CredentialValue})
		}
	}
	for _, m := range flagPair.FindAllStringSubmatchIndex(text, -1) {
		if credentialKey(text[m[2]:m[3]]) && !strings.HasPrefix(text[m[1]:], "-") {
			if s, e, ok := valueAt(text, m[1]); ok {
				out = append(out, span{s, e, CredentialValue})
			}
		}
	}
	return out
}

// proseWord is a value made only of letters and word separators.
var proseWord = regexp.MustCompile(`^[A-Za-z]+(?:[-_][A-Za-z]+)*$`)

// fileName reports a key that is really a file name ("auth.rs" in "auth.rs:12-30").
func fileName(key string) bool {
	i := strings.LastIndexByte(key, '.')
	if i < 0 {
		return false
	}
	switch strings.ToLower(key[i+1:]) {
	case "rs", "go", "ts", "tsx", "js", "jsx", "mjs", "cjs", "py", "rb", "java", "kt", "swift", "c", "h",
		"cc", "cpp", "hpp", "cs", "md", "json", "toml", "yaml", "yml", "sh", "zsh", "sql", "txt", "lock", "html", "css":
		return true
	}
	return false
}

var authScheme = regexp.MustCompile(`(?i)^(?:bearer|basic|token|digest|apikey|key)\s+`)

// valueAt reads the value starting at byte i: a quoted string or a run up to
// whitespace or punctuation that ends a value. It returns the span to redact.
func valueAt(text string, i int) (int, int, bool) {
	if i >= len(text) || text[i] == '<' {
		return 0, 0, false // <placeholder>
	}
	var s, e int
	switch q := text[i]; q {
	case '"', '\'', '`':
		s = i + 1
		e = s
		for e < len(text) && text[e] != q && text[e] != '\n' {
			if text[e] == '\\' {
				e++
			}
			e++
		}
		if e > len(text) {
			e = len(text)
		}
	default:
		s = i
		e = s
		for e < len(text) && !strings.ContainsRune(" \t\r\n,;&)}]>\"'`", rune(text[e])) {
			e++
		}
		// An auth scheme ("Bearer abc…") is part of the value; keep the scheme.
		if loc := authScheme.FindStringIndex(text[s:min(len(text), s+16)]); loc != nil {
			s += loc[1]
			e = s
			for e < len(text) && !strings.ContainsRune(" \t\r\n,;&)}]>\"'`", rune(text[e])) {
				e++
			}
		}
	}
	v := text[s:e]
	if loc := authScheme.FindStringIndex(v); loc != nil {
		s += loc[1]
		v = text[s:e]
	}
	if strings.ContainsAny(v, " \t") {
		return 0, 0, false // quoted prose or code (`Authorization: Bearer <token>`), not one value
	}
	if len(v) < 4 || heldOut(v) || shortNumber.MatchString(v) || numberRange.MatchString(v) ||
		dottedIdent.MatchString(v) || strings.ContainsAny(v, "()[]{}") || strings.HasPrefix(v, "$") ||
		strings.HasPrefix(v, "<") || strings.HasPrefix(v, "&") || strings.HasPrefix(v, "*") {
		return 0, 0, false
	}
	return s, e, true
}

// credentialPairs are adjacent key segments that name a credential.
var credentialPairs = map[[2]string]bool{
	{"api", "key"}: true, {"access", "key"}: true, {"private", "key"}: true,
	{"secret", "key"}: true, {"client", "secret"}: true, {"session", "key"}: true,
	{"signing", "key"}: true, {"encryption", "key"}: true, {"master", "key"}: true,
	{"account", "key"}: true, {"app", "key"}: true, {"license", "key"}: true,
	{"service", "key"}: true, {"admin", "key"}: true, {"deploy", "key"}: true,
}

// credentialWords are key segments that name a credential on their own.
var credentialWords = map[string]bool{
	"password": true, "passwd": true, "passphrase": true, "pwd": true, "pass": true,
	"secret": true, "token": true, "apikey": true, "auth": true, "authorization": true,
	"credential": true, "credentials": true, "creds": true, "bearer": true,
	"privatekey": true, "accesskey": true, "secretkey": true, "clientsecret": true,
	"accesstoken": true, "refreshtoken": true, "idtoken": true, "authtoken": true,
	"sessiontoken": true, "xapikey": true, "cookie": true, "secrets": true, "passwords": true,
}

// notCredentialTail are final key segments that describe a credential rather
// than hold it: token_count, secret_name, auth_url, password_length.
var notCredentialTail = map[string]bool{
	"count": true, "counts": true, "type": true, "types": true, "limit": true, "length": true,
	"len": true, "size": true, "id": true, "ids": true, "url": true, "uri": true, "urls": true,
	"endpoint": true, "path": true, "file": true, "dir": true, "name": true, "names": true,
	"ttl": true, "expiry": true, "expires": true, "expiration": true, "in": true, "at": true,
	"timeout": true, "header": true, "field": true, "prefix": true, "env": true, "var": true,
	"usage": true, "budget": true, "max": true, "min": true, "hash": true, "kind": true,
	"mode": true, "method": true, "provider": true, "scheme": true, "enabled": true,
	"required": true, "rotation": true, "version": true, "policy": true, "store": true,
	"source": true, "location": true, "format": true, "help": true, "label": true,
	"hint": true, "description": true, "placeholder": true, "set": true, "present": true,
	"configured": true, "ref": true, "arn": true,
}

// credentialKey decides whether a key names a credential value.
func credentialKey(key string) bool {
	segs := keySegments(key)
	if len(segs) == 0 {
		return false
	}
	lower := strings.ToLower(key)
	if lower == "pwd" || lower == "oldpwd" {
		return false // the shell's working directory
	}
	if notCredentialTail[segs[len(segs)-1]] {
		return false
	}
	for i, s := range segs {
		if s == "tokens" || s == "tokenizer" || s == "author" || s == "authority" || s == "authors" {
			return false
		}
		if credentialWords[s] {
			return true
		}
		if i+1 < len(segs) && credentialPairs[[2]string{s, strings.TrimSuffix(segs[i+1], "s")}] {
			return true
		}
	}
	return credentialWords[strings.Join(segs, "")]
}

// keySegments splits snake_case, kebab-case, dotted and camelCase keys into
// lower-case words: "AWS_SECRET_ACCESS_KEY" → aws secret access key,
// "clientSecret" → client secret.
func keySegments(key string) []string {
	var segs []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			segs = append(segs, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(key)
	for i, r := range rs {
		switch {
		case r == '_' || r == '-' || r == '.':
			flush()
		case i > 0 && r >= 'A' && r <= 'Z' && rs[i-1] >= 'a' && rs[i-1] <= 'z':
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return segs
}
