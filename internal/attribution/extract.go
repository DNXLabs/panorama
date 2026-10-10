// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package attribution

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"regexp"
	"strings"
)

// maxScan bounds how much of a body is examined from each end.
//
// A one-million-token context is a few megabytes, and running patterns over all
// of it on the request path would spend the latency budget this gateway
// advertises. Two windows rather than one because the two things worth finding
// live at opposite ends: the environment block is written once at the start of
// a conversation, and the output of git and gh arrives in the most recent
// tool results at the end. A match straddling the middle of a very large body
// is missed, which costs an attribution rather than producing a wrong one.
const maxScan = 1 << 20 // 1 MiB

// maxAnchorTries bounds how many occurrences of an anchor are examined before
// giving up. Documentation and prose mention github.com; the first few
// occurrences are where a remote actually is.
const maxAnchorTries = 4

// fingerprintText is how much of a conversation's opening goes into its
// fingerprint. Long enough that two different conversations rarely share it,
// short enough to hash in microseconds.
const fingerprintText = 1024

// jsonText matches a run of characters inside a JSON string value.
//
// This is the detail that makes the whole package work, and it is easy to get
// wrong. The bodies scanned here are JSON, so the text inside them is escaped:
// a newline is the two characters \ and n, not a newline. A pattern written
// against the text a human sees - "Current branch: main\nMain branch: main" -
// matches nothing. Terminating a capture at the next quote or backslash is what
// reads one field out of escaped JSON without unescaping the document, and it
// works unchanged on unescaped text because a raw newline is excluded too.
const jsonText = `[^"\\\n\r]`

var (
	// The environment block current Claude Code writes into its prompt. The
	// spelling has changed between versions - an older build wrote "Working
	// directory:" inside an <env> block, a current one writes "Primary working
	// directory:" under a heading - so both are matched. An extractor pinned to
	// one spelling does not fail loudly when a client is upgraded; it silently
	// stops attributing, which is worse.
	branchPattern = regexp.MustCompile(`Current branch:\s*(` + jsonText + `+)`)
	// The one capture that has to cross a backslash.
	//
	// A Windows path arrives as C:\\Users\\dev\\src\\panorama - every separator a
	// backslash, escaped because the body is JSON. jsonText stops at the first
	// one, which is right for a branch name and here captured the drive letter
	// and nothing else: every session on Windows recorded its repository as
	// "C:", collapsing an organization's Windows engineers into one bogus
	// checkout. An escaped pair is admitted; a lone backslash still ends the
	// capture, so \n terminates it as before.
	cwdPattern = regexp.MustCompile(
		`(?:P|p)?(?:rimary )?[Ww]orking directory:\s*((?:` + jsonText + `|\\\\)+)`)

	// Codex writes its working directory into an environment_context block in
	// the input it sends, as an element rather than a sentence:
	// <cwd>/Users/dev/src/panorama</cwd>. Same escaping rules, so the same
	// capture, ended by the closing tag's angle bracket.
	codexCwdPattern = regexp.MustCompile(`<cwd>((?:[^"\\\n\r<]|\\\\)+)</cwd>`)

	// git status output, which arrives inside tool results.
	onBranchPattern = regexp.MustCompile(`On branch (` + jsonText + `+)`)

	// A git remote, as written by git remote -v, git clone, or a URL in gh
	// output. The non-greedy repository name with an optional .git suffix is
	// what stops "panorama.git" becoming the repository name.
	remotePattern = regexp.MustCompile(
		`(?:github|gitlab)\.com[:/]([A-Za-z0-9._-]+/[A-Za-z0-9._-]+?)(?:\.git)?(?:[^A-Za-z0-9._/-]|$)`)

	// A pull request URL, which gh pr view and gh pr create both emit.
	pullRequestPattern = regexp.MustCompile(
		`(?:github|gitlab)\.com/([A-Za-z0-9._-]+/[A-Za-z0-9._-]+)/(?:pull|merge_requests)/(\d+)`)

	// The session component of the metadata a client may send. Claude Code puts
	// an opaque per-session identifier here, which is a better grouping key
	// than anything inferred - when it is present, which is a question of
	// client version rather than of configuration.
	clientSessionPattern = regexp.MustCompile(`session_([A-Za-z0-9][A-Za-z0-9_-]{7,63})`)
)

// DefaultTicketPattern is the ticket-key shape most issue trackers use.
//
// It is configurable per deployment because it is the one extraction rule whose
// right answer is a fact about the customer rather than about the client. An
// operator who names their projects - (PAY|BILL)-\d+ - turns the lowest rung of
// the ladder from a heuristic into an allowlist, which is the recommended
// posture for anyone who cares about precision.
const DefaultTicketPattern = `\b[A-Z][A-Z0-9]{1,9}-\d{1,6}\b`

// notTicketPrefixes are identifiers shaped exactly like ticket keys that are
// not ticket keys. UTF-8, SHA-1, CVE-2024, RFC-2119, AES-256.
//
// This is a denylist, which is not this codebase's usual instinct - but it is
// guarding a heuristic on the lowest rung of the ladder rather than a trust
// boundary, and the allowlist alternative already exists: configure
// attribution.ticket_pattern with the project keys and none of this applies.
// Getting it wrong costs one wrong attribution, which is visible and fixable,
// and the guard is what stops a conversation about character encodings billing
// to a ticket called UTF-8.
var notTicketPrefixes = map[string]struct{}{
	"AES": {}, "ANSI": {}, "ARM": {}, "ASCII": {}, "BASE": {}, "BIP": {},
	"CSS": {}, "CVE": {}, "CWE": {}, "DES": {}, "EC": {}, "ECMA": {},
	"EIP": {}, "ERC": {}, "ES": {}, "FIPS": {}, "GMT": {}, "GPT": {},
	"HMAC": {}, "HTML": {}, "HTTP": {}, "HTTPS": {}, "IEEE": {}, "IP": {},
	"IPV": {}, "ISO": {}, "JDK": {}, "JEP": {}, "JSON": {}, "JSR": {},
	"LTS": {}, "MD": {}, "NIST": {}, "PBKDF": {}, "PEP": {}, "RFC": {},
	"RSA": {}, "SHA": {}, "SP": {}, "SQL": {}, "SSL": {}, "TLS": {},
	"UTC": {}, "UTF": {}, "X": {}, "XML": {},
}

// Extractor reads work signals out of requests.
//
// It holds the compiled ticket pattern, which is the only part of extraction a
// deployment configures. Everything else is a property of the clients, not of
// the customer.
type Extractor struct {
	ticket *regexp.Regexp
}

// NewExtractor compiles an extractor. An empty pattern uses the default.
func NewExtractor(ticketPattern string) (*Extractor, error) {
	if strings.TrimSpace(ticketPattern) == "" {
		ticketPattern = DefaultTicketPattern
	}
	re, err := regexp.Compile(ticketPattern)
	if err != nil {
		return nil, err
	}
	return &Extractor{ticket: re}, nil
}

// Shape is where in a request body the conversation lives, which differs by the
// API the request arrived in.
//
// Only the opening of the conversation depends on it. Everything structural -
// an environment block, git output, a remote, a pull request URL - is text
// inside the body whatever the shape, and is matched the same way.
type Shape int

const (
	// ShapeMessages is the Anthropic Messages body: a messages array whose
	// system prompt travels beside it, so the first message is the user's.
	ShapeMessages Shape = iota
	// ShapeChat is the OpenAI Chat Completions body: a messages array whose
	// first element is usually the system prompt, identical for every session
	// a tool opens.
	ShapeChat
	// ShapeResponses is the OpenAI Responses body: an input array, into which
	// Codex writes its environment and instructions ahead of the prompt.
	ShapeResponses
)

// maxOpeningMessages bounds how many messages are examined to find the first
// one a person wrote. A system prompt and a developer message come first; a
// conversation whose user message is not among the first few has no opening
// this can hash, and is grouped by the other methods or not at all.
const maxOpeningMessages = 8

// FingerprintShape is Fingerprint for a body of a given shape.
func (e *Extractor) FingerprintShape(shape Shape, userID string, body []byte) (fingerprint, method string) {
	if shape == ShapeMessages {
		return e.Fingerprint(userID, body)
	}

	for _, region := range scanRegions(body) {
		if m := firstMatch(region, "session_", clientSessionPattern, 128); len(m) > 1 {
			return hash(userID, "client-session", m[1]), "client-session-id"
		}
	}

	cwd := e.workingDirectory(body)
	var opening string
	switch shape {
	case ShapeChat:
		opening = openingOfArray(body, "messages")
	case ShapeResponses:
		opening = openingOfArray(body, "input")
	}
	if opening == "" {
		return "", ""
	}
	return hash(userID, cwd, opening), "conversation-prefix"
}

// StructuralShape is Structural for a body of a given shape. The same patterns
// apply to every shape today; the shape is taken so the call sites say what
// they are reading, and so a pattern that belongs to one API can be added
// without reaching into the other two.
func (e *Extractor) StructuralShape(_ Shape, body []byte) Signals { return e.Structural(body) }

// openingOfArray returns the first thing a person wrote, from an array of
// messages or input items under key.
//
// The first element is not it. A Chat Completions body opens with the tool's
// system prompt, which is the same for every session that tool ever opens - a
// fingerprint of it would merge every conversation in a repository into one.
// Codex's Responses body opens with its environment and the repository's
// instructions, which are the same for every session in a checkout. So this
// reads elements one at a time until it meets a user message, and inside that
// skips the blocks a tool writes rather than a person.
//
// A streaming decoder rather than a parse of the body: it stops at the first
// user message, so the cost is the system prompt in front of it and never the
// conversation behind it.
func openingOfArray(body []byte, key string) string {
	at := bytes.Index(body, []byte(`"`+key+`"`))
	if at < 0 {
		return ""
	}
	rest := body[at+len(key)+2:]
	colon := skipSpace(rest)
	if colon >= len(rest) || rest[colon] != ':' {
		return ""
	}
	rest = rest[colon+1:]
	open := skipSpace(rest)
	if open >= len(rest) {
		return ""
	}
	if rest[open] == '"' {
		// A Responses input can be a bare string, which is the prompt.
		var text string
		if err := json.NewDecoder(bytes.NewReader(rest[open:])).Decode(&text); err != nil {
			return ""
		}
		return cutOpening(text)
	}
	if rest[open] != '[' {
		return ""
	}

	dec := json.NewDecoder(bytes.NewReader(rest[open:]))
	if _, err := dec.Token(); err != nil {
		return ""
	}
	for i := 0; i < maxOpeningMessages && dec.More(); i++ {
		var item struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := dec.Decode(&item); err != nil {
			return ""
		}
		if item.Role != "user" {
			continue
		}
		if text := personText(item.Content); text != "" {
			return cutOpening(text)
		}
	}
	return ""
}

// personText reads the first text in a message's content that a person wrote:
// the content itself when it is a string, or the first text block that is not
// one a tool generated.
func personText(content json.RawMessage) string {
	var plain string
	if err := json.Unmarshal(content, &plain); err == nil {
		if generatedBlock(plain) {
			return ""
		}
		return plain
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return ""
	}
	for _, b := range blocks {
		if (b.Type == "text" || b.Type == "input_text") && b.Text != "" && !generatedBlock(b.Text) {
			return b.Text
		}
	}
	return ""
}

// generatedBlock reports text a coding tool wrote into a user message on its
// own account: Codex's environment and instruction blocks, which are the same
// in every session in a checkout.
func generatedBlock(text string) bool {
	trimmed := strings.TrimSpace(text)
	for _, prefix := range []string{"<environment_context>", "<user_instructions>", "# AGENTS.md instructions"} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

// cutOpening bounds an opening the way the Messages opening is bounded.
func cutOpening(text string) string {
	if len(text) > fingerprintText {
		return text[:fingerprintText]
	}
	return text
}

// Fingerprint groups a request with the other turns of its conversation.
//
// Two methods, in order of preference:
//
//   - The session identifier a client puts in its request metadata. Exact, and
//     unaffected by anything that happens to the conversation.
//   - A hash of the user, the working directory, and the opening of the
//     conversation. Coding agents re-send the whole conversation every turn, so
//     the opening is stable across the turns of one session and different
//     between sessions.
//
// The user is always part of the hash, so two people who happen to open
// identically are never merged. The working directory is part of it so that the
// same person running the same opening line in two repositories gets two
// sessions rather than one attributed to whichever repository spoke first.
//
// The fallback has one known limitation worth stating: a client that compacts
// its own history rewrites the opening, and the conversation continues under a
// new fingerprint. It reads as two sessions on the same work rather than as
// wrong attribution, and the metadata path is not affected.
func (e *Extractor) Fingerprint(userID string, body []byte) (fingerprint, method string) {
	for _, region := range scanRegions(body) {
		if m := firstMatch(region, "session_", clientSessionPattern, 128); len(m) > 1 {
			return hash(userID, "client-session", m[1]), "client-session-id"
		}
	}

	cwd := e.workingDirectory(body)
	opening := e.opening(body)
	if opening == "" {
		// Nothing stable to group by. A request without a conversation is not
		// forced into one: an invented session would inherit an attribution
		// that has nothing to do with it.
		return "", ""
	}

	return hash(userID, cwd, opening), "conversation-prefix"
}

// opening returns the first text block of the conversation.
//
// The value is cut at the end of that text and nowhere else. Taking a fixed
// number of bytes instead would be simpler and would be wrong: a short opening
// message would spill into the messages after it, and those grow with every
// turn - so the fingerprint would change on each request and every turn would
// become its own session.
func (e *Extractor) opening(body []byte) string {
	start := bytes.Index(body, []byte(`"messages"`))
	if start < 0 {
		return ""
	}

	region := body[start:min(start+8*fingerprintText, len(body))]

	// Both shapes the Messages API allows: content as an array of typed blocks,
	// and content as a bare string.
	return firstStringValue(region, "text", "content")
}

// firstStringValue reads the first string value belonging to one of the named
// keys.
//
// The key has to be matched as a key rather than as a substring, which is the
// whole reason this is not a two-line bytes.Index. A content block is
// {"type":"text","text":"…"}, so searching for the characters "text" finds the
// value of `type` first and returns the word "text" - identically, for every
// request from every conversation. Every session in the deployment then shares
// one fingerprint and one attribution. It looks like it is working right up
// until someone asks why all the spend is on one branch.
func firstStringValue(region []byte, keys ...string) string {
	bestAt := -1
	var best []byte

	for _, key := range keys {
		needle := []byte(`"` + key + `"`)
		offset := 0

		for offset < len(region) {
			i := bytes.Index(region[offset:], needle)
			if i < 0 {
				break
			}
			at := offset + i
			offset = at + len(needle)

			value, ok := stringValueAt(region[at+len(needle):])
			if !ok {
				continue
			}
			if bestAt < 0 || at < bestAt {
				bestAt, best = at, value
			}
			break
		}
	}

	if len(best) > fingerprintText {
		best = best[:fingerprintText]
	}
	return string(best)
}

// stringValueAt reads a JSON string value that begins after a key, allowing the
// whitespace a formatter might have left around the colon.
func stringValueAt(after []byte) ([]byte, bool) {
	i := skipSpace(after)
	if i >= len(after) || after[i] != ':' {
		return nil, false
	}

	rest := after[i+1:]
	j := skipSpace(rest)
	if j >= len(rest) || rest[j] != '"' {
		// An object or an array: this key is not the one holding the text.
		return nil, false
	}

	value := rest[j+1:]
	end := jsonStringEnd(value)
	if end < 0 {
		end = len(value)
	}
	return value[:end], true
}

func skipSpace(b []byte) int {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// jsonStringEnd finds the closing quote of a JSON string value, honouring
// escapes so that a quote inside the text is not mistaken for the end of it.
func jsonStringEnd(b []byte) int {
	escaped := false
	for i, c := range b {
		switch {
		case escaped:
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			return i
		}
	}
	return -1
}

// Structural reads the request body's structural ground truth.
//
// Regex and structure matching only, which is what makes this safe to run
// inline. Nothing here parses the conversation, and nothing here keeps any part
// of it: every value that survives is a repository, a branch, a number, or a
// ticket key.
func (e *Extractor) Structural(body []byte) Signals {
	var s Signals

	for _, region := range scanRegions(body) {
		// A branch, from the environment block a coding agent writes into its
		// own prompt, or from git status output in a tool result.
		if !s.Branch.Found() {
			if m := firstMatch(region, "Current branch:", branchPattern, 256); len(m) > 1 {
				s.Branch = Observation{Value: clean(m[1]), Rung: RungStructural, Evidence: "environment block"}
			}
		}
		if !s.Branch.Found() {
			if m := firstMatch(region, "On branch ", onBranchPattern, 256); len(m) > 1 {
				s.Branch = Observation{Value: clean(m[1]), Rung: RungStructural, Evidence: "git status output"}
			}
		}

		// A pull request, which also names its repository - the most complete
		// single thing gh output gives us.
		if !s.PullRequest.Found() {
			if m := firstMatch(region, "/pull/", pullRequestPattern, 256); len(m) > 2 {
				s.PullRequest = Observation{Value: clean(m[2]), Rung: RungStructural, Evidence: "pull request URL"}
				if !s.Repo.Found() {
					s.Repo = Observation{Value: clean(m[1]), Rung: RungStructural, Evidence: "pull request URL"}
				}
			}
		}

		// A remote, from git remote -v or any URL naming the repository.
		if !s.Repo.Found() {
			for _, anchor := range []string{"github.com", "gitlab.com"} {
				if m := firstMatch(region, anchor, remotePattern, 256); len(m) > 1 {
					s.Repo = Observation{Value: clean(m[1]), Rung: RungStructural, Evidence: "git remote"}
					break
				}
			}
		}
	}

	if dir := e.workingDirectory(body); dir != "" {
		s.Directory = Observation{Value: dir, Rung: RungStructural, Evidence: "working directory"}
	}

	e.findTicket(&s, body)
	return s
}

// workingDirectory reads the last element of the working directory path.
//
// The whole path is not kept. It is the one extracted value that would say
// something about a person rather than about their work - /Users/their-name is
// in every one of them - and the directory's own name is all the attribution
// needs.
func (e *Extractor) workingDirectory(body []byte) string {
	for _, region := range scanRegions(body) {
		for _, probe := range []struct {
			anchor  string
			pattern *regexp.Regexp
		}{
			{"orking directory:", cwdPattern},
			{"<cwd>", codexCwdPattern},
		} {
			m := firstMatch(region, probe.anchor, probe.pattern, 512)
			if len(m) < 2 {
				continue
			}
			dir := lastPathElement(clean(m[1]))
			if dir != "" && dir != "." && dir != "/" {
				return dir
			}
		}
	}
	return ""
}

// lastPathElement names the checkout at the end of a path, spelled either way.
//
// Separators are "/" on Unix and an escaped backslash on Windows, because the
// body is JSON and a Windows separator is written twice. Both are folded to "/"
// before the last element is taken, so the same checkout is named the same
// whichever machine an engineer is on - which is the point, since the name is
// what groups their work.
func lastPathElement(raw string) string {
	normalized := strings.ReplaceAll(raw, `\\`, "/")
	normalized = strings.TrimRight(normalized, "/")
	if normalized == "" {
		return ""
	}
	// A bare drive is not a checkout, and reporting "C:" as one would group
	// every Windows engineer's work under a drive letter.
	if strings.HasSuffix(normalized, ":") {
		return ""
	}
	return clean(path.Base(normalized))
}

// findTicket looks for a ticket key, structurally first and in conversation
// text second.
//
// The order matters more than it looks. A key in a branch name is a fact about
// what the person is doing; the same key in a sentence is a mention, and
// engineers discuss each other's tickets constantly. Attributing the second as
// though it were the first is how a cost report starts billing one team's work
// to another team's project.
func (e *Extractor) findTicket(s *Signals, body []byte) {
	if s.Branch.Found() {
		if key := e.matchTicket([]byte(s.Branch.Value)); key != "" {
			s.Ticket = Observation{Value: key, Rung: RungStructural, Evidence: "branch name"}
			return
		}
	}

	for _, region := range scanRegions(body) {
		if key := e.matchTicket(region); key != "" {
			s.Ticket = Observation{Value: key, Rung: RungContent, Evidence: "mentioned in conversation"}
			return
		}
	}
}

// matchTicket finds the first plausible ticket key in a region.
func (e *Extractor) matchTicket(region []byte) string {
	for _, m := range e.ticket.FindAll(region, maxAnchorTries) {
		key := clean(string(m))
		if key == "" {
			continue
		}
		prefix, _, found := strings.Cut(key, "-")
		if !found {
			continue
		}
		if _, excluded := notTicketPrefixes[strings.ToUpper(prefix)]; excluded {
			continue
		}
		return key
	}
	return ""
}

// Stamp reads the explicit attribution a client was configured to send.
//
// This is the precision tier of the ladder and the only rung that requires
// anything of the client: a per-user configuration that injects these headers,
// which Panorama generates. Values are treated as hostile - bounded, printable,
// and rejected outright rather than truncated - because a header is whatever
// the caller decided to put in it.
func Stamp(header func(string) string) Signals {
	var s Signals

	for _, field := range []struct {
		name string
		dst  *Observation
	}{
		{HeaderRepo, &s.Repo},
		{HeaderBranch, &s.Branch},
		{HeaderTicket, &s.Ticket},
		{HeaderPullRequest, &s.PullRequest},
	} {
		if v := clean(header(field.name)); v != "" {
			*field.dst = Observation{Value: v, Rung: RungStamp, Evidence: "client header"}
		}
	}

	return s
}

// The headers a client may stamp a request with. An allowlist: no other
// x-panorama- header is read, and none is forwarded to a provider.
const (
	HeaderRepo        = "x-panorama-repo"
	HeaderBranch      = "x-panorama-branch"
	HeaderTicket      = "x-panorama-ticket"
	HeaderPullRequest = "x-panorama-pr"
	// HeaderSession lets a client group its own turns, which is exact where
	// inference is a very good guess.
	HeaderSession = "x-panorama-session"
)

// The headers opencode sends on every request to a provider it does not own,
// with nobody configuring it to. They are not stamps: they say which of
// opencode's conversations a request belongs to and nothing about the work, so
// they group turns and never place a session against a work item.
const (
	HeaderOpenCodeSession = "x-session-id"
	// HeaderOpenCodeParentSession is set on a subagent's requests and names
	// the conversation that started it.
	HeaderOpenCodeParentSession = "x-parent-session-id"
)

// HeaderClaudeCodeSession is the conversation id Claude Code sends on every
// request, unasked: the main model's turns, the fast model's background calls
// (titles, summaries, command checks) and its subagents all carry the same one.
// Like opencode's, it groups turns and says nothing about the work.
const HeaderClaudeCodeSession = "x-claude-code-session-id"

// ClaudeCodeSession returns the Claude Code conversation a request belongs to,
// or "" when it names none.
//
// Without it a conversation is recognised by its working directory and opening
// message, and the fast model's calls carry neither: they open with their own
// short prompt and no environment block, so each became a session of its own,
// with no repository, and a row of its own on the audit trail.
func ClaudeCodeSession(header func(string) string) string {
	return clean(header(HeaderClaudeCodeSession))
}

// OpenCodeSession returns the opencode conversation a request belongs to, or
// "" when it names none.
//
// A subagent's requests are grouped under the conversation that started it,
// because the subagent is doing that conversation's work: left on its own
// session it would open unattributed and stay there, since a subagent's prompt
// is a task description rather than the conversation that found the ticket.
// Only the immediate parent is named, so a subagent started by a subagent
// groups under its parent rather than the root.
func OpenCodeSession(header func(string) string) string {
	if parent := clean(header(HeaderOpenCodeParentSession)); parent != "" {
		return parent
	}
	return clean(header(HeaderOpenCodeSession))
}

// CleanHeader normalises a client-supplied header value, rejecting rather than
// truncating anything out of bounds. Exported because the data plane reads the
// session header directly, and it must be held to the same rules as the rest.
func CleanHeader(v string) string { return clean(v) }

// StampedFingerprint builds a session fingerprint from an identifier the client
// supplied.
//
// The user is mixed in exactly as the inferred fingerprints do it, so a client
// that picks a guessable session id cannot join another person's session by
// naming it.
func StampedFingerprint(userID, session string) string {
	return hash(userID, "client-header", session)
}

// Combine merges what the headers said with what the body showed, keeping the
// higher rung for each field.
//
// Not "the stamp wins outright": a client that stamps a ticket but not a branch
// should still get its branch read out of the body, and a stamped request whose
// body reveals a pull request is better described by both.
func Combine(stamp, structural Signals) Signals {
	out := structural
	out.merge(stamp)
	return out
}

// scanRegions returns the parts of a body worth examining.
func scanRegions(body []byte) [][]byte {
	if len(body) <= 2*maxScan {
		return [][]byte{body}
	}
	return [][]byte{body[:maxScan], body[len(body)-maxScan:]}
}

// anchorLead is how far before an anchor the match window begins.
//
// An anchor is a literal that must be present for a pattern to have any chance,
// and the cheapest such literal is often in the middle of what the pattern
// matches - "/pull/" inside a pull request URL, "orking directory:" inside both
// spellings of the environment block. Starting the window at the anchor would
// put the beginning of the pattern's own match outside it, and the pattern would
// never fire. This is the bug that makes an extractor look like it works and
// silently find nothing.
const anchorLead = 128

// firstMatch locates an anchor cheaply and then runs a pattern over a small
// window around it.
//
// The anchor search is a literal scan, which is an order of magnitude faster
// than the regular expression it guards and is what keeps this affordable on
// the request path: a body with no environment block pays one memchr rather
// than a full pattern match.
func firstMatch(region []byte, anchor string, pattern *regexp.Regexp, window int) []string {
	needle := []byte(anchor)
	offset := 0

	for range maxAnchorTries {
		i := bytes.Index(region[offset:], needle)
		if i < 0 {
			return nil
		}

		start := offset + i
		from := max(0, start-anchorLead)
		end := min(start+window, len(region))

		if m := pattern.FindSubmatch(region[from:end]); m != nil {
			out := make([]string, len(m))
			for j, group := range m {
				out[j] = string(group)
			}
			return out
		}

		offset = start + len(needle)
		if offset >= len(region) {
			return nil
		}
	}
	return nil
}

// hash builds a fingerprint from its parts.
//
// The parts are separated by a byte that cannot appear in any of them, so that
// two different splits cannot produce the same input - the classic way a
// concatenated key collides.
func hash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
