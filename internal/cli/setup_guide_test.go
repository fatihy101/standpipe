package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/fatihy101/standpipe/internal/config"
	"github.com/fatihy101/standpipe/internal/httpapi"
	"github.com/fatihy101/standpipe/internal/testutil"
)

// ---------------------------------------------------------------------------
// docs/setup.md, EXECUTED.
//
// The setup guide walks an operator from "no GitHub App" to "a real caller
// went through the gateway". The corrections such a guide needs are SHAPE and
// ORDER facts (a field name, a map versus a list, which command must run
// first), and those are exactly what a test can hold.
//
// THE CONVENTION. This test reads ONLY fenced code blocks whose info string
// carries a `setup-test:<role>` attribute, for example
//
//	```yaml setup-test:config
//	```sh setup-test:mint
//
// Prose is never parsed. The roles (see setupRoles) are:
//
//	config         the standpipe.yaml example                 exactly one
//	mint           token create, then migrate                 exactly one
//	commands       other command blocks                       any number
//	token-example  an example token                           exactly one
//	trigger        the curl call                              exactly one
//	response       an example 200 body (JSON)                 exactly one
//	trace          lines `standpipe trace` must print         exactly one
//
// Every standpipe/standpiped line in a mint, commands or trigger block is
// checked against the binaries' own usage text. Untagged blocks are ignored,
// and an unknown role is a failure. The same convention is documented in an
// HTML comment at the top of docs/setup.md.
//
// WHAT THIS FILE CANNOT DO. It executes the credential-free half: config
// validation, the migrate ordering, the token round-trip through the
// published regex, the spelling of every command line, and the 200 body and
// trace output of the real server against internal/testutil's fake GitHub.
// Steps that need a real GitHub App, its private key and an installation are
// not reachable from a test.
// ---------------------------------------------------------------------------

const setupGuidePath = "../../docs/setup.md"

// setupRoles maps each role to whether the guide must carry exactly one block
// of it (true) or any number (false).
var setupRoles = map[string]bool{
	"config":        true,
	"mint":          true,
	"commands":      false,
	"token-example": true,
	"trigger":       true,
	"response":      true,
	"trace":         true,
}

// commandRoles are the roles whose blocks hold shell command lines.
var commandRoles = []string{"mint", "commands", "trigger"}

type taggedBlock struct {
	lang string
	role string
	body string
	line int // 1-based line of the opening fence
}

const setupTag = "setup-test:"

// setupBlocks returns every tagged fenced block of docs/setup.md, in order.
func setupBlocks(t *testing.T) []taggedBlock {
	t.Helper()
	raw, err := os.ReadFile(setupGuidePath)
	if err != nil {
		t.Fatalf("reading %s: %v", setupGuidePath, err)
	}
	var out []taggedBlock
	var cur *taggedBlock
	var body []string
	inside := false
	for i, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "```") {
			if inside && cur != nil {
				body = append(body, line)
			}
			continue
		}
		if inside {
			if cur != nil {
				cur.body = strings.Join(body, "\n")
				out = append(out, *cur)
			}
			inside, cur, body = false, nil, nil
			continue
		}
		inside = true
		fields := strings.Fields(strings.TrimPrefix(line, "```"))
		for j, f := range fields {
			if !strings.HasPrefix(f, setupTag) {
				continue
			}
			role := strings.TrimPrefix(f, setupTag)
			if _, ok := setupRoles[role]; !ok {
				t.Fatalf("%s:%d tags a block with unknown role %q; known roles: %v",
					setupGuidePath, i+1, role, sortedRoleNames())
			}
			lang := ""
			if j > 0 {
				lang = fields[0]
			}
			cur = &taggedBlock{lang: lang, role: role, line: i + 1}
		}
	}
	if inside {
		t.Fatalf("%s has an unclosed code fence", setupGuidePath)
	}
	return out
}

func sortedRoleNames() []string {
	var out []string
	for r := range setupRoles {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// blocksOf returns the blocks of one role, enforcing the "exactly one" rule.
func blocksOf(t *testing.T, role string) []taggedBlock {
	t.Helper()
	var out []taggedBlock
	for _, b := range setupBlocks(t) {
		if b.role == role {
			out = append(out, b)
		}
	}
	if setupRoles[role] && len(out) != 1 {
		t.Fatalf("%s has %d blocks tagged %s%s, want exactly 1", setupGuidePath, len(out), setupTag, role)
	}
	if len(out) == 0 {
		t.Fatalf("%s has no block tagged %s%s", setupGuidePath, setupTag, role)
	}
	return out
}

func oneBlock(t *testing.T, role string) taggedBlock { return blocksOf(t, role)[0] }

// TestSetupGuideTagsAreWellFormed pins the convention itself: every required
// role is present exactly once, and every tagged block names a language.
func TestSetupGuideTagsAreWellFormed(t *testing.T) {
	for _, role := range sortedRoleNames() {
		for _, b := range blocksOf(t, role) {
			if strings.TrimSpace(b.body) == "" {
				t.Errorf("%s:%d: the %s block is empty", setupGuidePath, b.line, role)
			}
			if b.lang == "" {
				t.Errorf("%s:%d: the %s block has no language before its tag", setupGuidePath, b.line, role)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Step 2: the config example.
// ---------------------------------------------------------------------------

// setupConfig returns the config block with the placeholders the guide itself
// tells the operator to fill: the App key path, the token_hash minted in step
// 3, and a writable database path. Nothing else is changed; the bytes that
// reach config.Load are the bytes an operator would paste.
func setupConfig(t *testing.T, dir, keyPath, tokenHash string) string {
	t.Helper()
	yaml := oneBlock(t, "config").body

	replacements := [][2]string{
		{"private_key_path: /etc/standpipe/github-app.pem", "private_key_path: " + keyPath},
		{"sqlite_path: ./standpipe.db", "sqlite_path: " + filepath.Join(dir, "standpipe.db")},
	}
	if tokenHash != "" {
		replacements = append(replacements, [2]string{`token_hash: ""`, `token_hash: "` + tokenHash + `"`})
	}
	for _, r := range replacements {
		if !strings.Contains(yaml, r[0]) {
			t.Fatalf("the setup guide's config block no longer contains %q, so this test cannot fill the "+
				"placeholder the guide tells the operator to fill", r[0])
		}
		yaml = strings.Replace(yaml, r[0], r[1], 1)
	}
	return yaml
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// fakeAppKey writes a PEM the daemon's own loader accepts, standing in for the
// .pem step 2 tells the operator to copy into place.
func fakeAppKey(t *testing.T, dir string) string {
	t.Helper()
	path, err := testutil.WriteAppKey(dir)
	if err != nil {
		t.Fatalf("WriteAppKey: %v", err)
	}
	return path
}

// mintForTest returns a token_hash the loader accepts.
func mintForTest(t *testing.T) string {
	t.Helper()
	_, hash, err := httpapi.MintToken("tok_a1")
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	return hash
}

func TestSetupGuideConfigLoadsThroughTheRealLoader(t *testing.T) {
	// The loader is the one `standpiped` and `standpipe migrate` run, and it
	// is STRICT about unknown fields, so a misspelled key is a hard failure
	// rather than a silently ignored line. That strictness is what lets this
	// test hold a field NAME at all.
	dir := t.TempDir()
	yaml := setupConfig(t, dir, fakeAppKey(t, dir), mintForTest(t))

	cfg, err := config.Load(writeFile(t, filepath.Join(dir, "setup.yaml"), yaml))
	if err != nil {
		t.Fatalf("the setup guide's config example does not load: %v\n\n%s\n\n"+
			"An operator who copies it verbatim is stopped before they reach GitHub.", err, yaml)
	}
	if cfg.Storage.SQLitePath == "" {
		t.Error("storage.sqlite_path came back empty; the example names the wrong key")
	}
	// callers is a MAP keyed by caller name.
	if len(cfg.Callers) != 1 {
		t.Fatalf("callers = %d entries, want 1", len(cfg.Callers))
	}
	caller, ok := cfg.Callers["release-bot"]
	if !ok {
		t.Fatalf("callers has no key \"release-bot\"; keys are %v; `callers` is a map keyed by "+
			"the caller's name", callerNames(cfg))
	}
	if caller.TokenID != "tok_a1" {
		t.Errorf("token_id = %q, want tok_a1, the value step 3's `token create --token-id` uses", caller.TokenID)
	}
	// The caller's allow entry uses singular workflow:/ref:, a DIFFERENT shape
	// from allowed_targets' plural lists.
	if len(caller.Allow) != 1 || caller.Allow[0].Workflow == "" || caller.Allow[0].Ref == "" {
		t.Errorf("callers.release-bot.allow = %+v; the example must produce one entry carrying "+
			"both a workflow and a ref", caller.Allow)
	}
	if len(cfg.AllowedTargets) != 1 {
		t.Errorf("allowed_targets = %d, want 1", len(cfg.AllowedTargets))
	}
}

func callerNames(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Callers))
	for k := range cfg.Callers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Step 3: token first, then migrate.
// ---------------------------------------------------------------------------

func TestSetupGuideMintsBeforeMigrating(t *testing.T) {
	dir := t.TempDir()
	key := fakeAppKey(t, dir)

	t.Run("migrate refuses a config whose token_hash is still empty", func(t *testing.T) {
		// This is the fact step 3's ordering rests on. It is a property of
		// config validation, not of the migrator, so no schema state can mask
		// it.
		path := writeFile(t, filepath.Join(dir, "no-hash.yaml"), setupConfig(t, dir, key, ""))
		out, err := capture(t, func() error { return runMigrate([]string{"--config", path}) })
		if err == nil {
			t.Fatalf("standpipe migrate accepted an empty token_hash. The guide's ordering rule exists "+
				"only because it does not; if that changed, the guide should say so.\n%s", out)
		}
		if !strings.Contains(err.Error(), "token_hash") {
			t.Errorf("migrate failed with %v, which does not name token_hash; the guide tells the "+
				"operator which field to fill, and that promise rests on this message", err)
		}
		// `config validate` runs this same loader (and exits the process on
		// failure, so it is not called in-process here).
		if _, err := config.Load(path); err == nil {
			t.Errorf("config.Load accepted an empty token_hash; the guide validates only after minting " +
				"because it does not")
		}
	})

	t.Run("token create, then paste the hash, then migrate", func(t *testing.T) {
		argv := commandLines(oneBlock(t, "mint"))
		tokenAt, validateAt, migrateAt := -1, -1, -1
		for i, a := range argv {
			switch {
			case len(a) > 2 && a[0] == "standpipe" && a[1] == "token" && a[2] == "create" && tokenAt < 0:
				tokenAt = i
			case len(a) > 2 && a[0] == "standpipe" && a[1] == "config" && a[2] == "validate" && validateAt < 0:
				validateAt = i
			case len(a) > 1 && a[0] == "standpipe" && a[1] == "migrate" && migrateAt < 0:
				migrateAt = i
			}
		}
		if tokenAt < 0 || validateAt < 0 || migrateAt < 0 {
			t.Fatalf("the mint block must contain `standpipe token create`, `standpipe config validate` "+
				"and `standpipe migrate`; it has %v", argv)
		}
		if tokenAt > validateAt || tokenAt > migrateAt {
			t.Fatalf("the mint block validates or migrates before it mints the token: %v", argv)
		}

		out, err := capture(t, func() error { return runToken(argv[tokenAt][2:]) })
		if err != nil {
			t.Fatalf("`%s` failed: %v\n%s", strings.Join(argv[tokenAt], " "), err, out)
		}
		token, hash := parseTokenOutput(out)
		if token == "" || hash == "" {
			t.Fatalf("token create printed no token and/or no token_hash line:\n%s", out)
		}

		// The printed hash, pasted in, is one the loader accepts; then the
		// guide's own migrate line runs against that file.
		path := writeFile(t, filepath.Join(dir, "after-token.yaml"), setupConfig(t, dir, key, hash))
		validateArgs := replaceFlagValue(t, argv[validateAt][3:], "--file", path)
		validateOut, err := capture(t, func() error { return runConfigValidate(validateArgs) })
		if err != nil || !strings.Contains(validateOut, "valid: true") {
			t.Fatalf("standpipe config validate did not accept the config after the hash was pasted in: %v\n%s",
				err, validateOut)
		}

		migrateArgs := replaceFlagValue(t, argv[migrateAt][2:], "--config", path)
		migrateOut, err := capture(t, func() error { return runMigrate(migrateArgs) })
		if err != nil {
			t.Fatalf("standpipe migrate refused the config after the hash was pasted in: %v\n%s", err, migrateOut)
		}
		if !strings.Contains(migrateOut, "applied:") {
			t.Errorf("migrate printed no applied migrations:\n%s", migrateOut)
		}
	})
}

// replaceFlagValue swaps the value of flag in args for value, failing if the
// flag is not there: the guide's own spelling is what runs.
func replaceFlagValue(t *testing.T, args []string, flag, value string) []string {
	t.Helper()
	out := append([]string(nil), args...)
	for i, a := range out {
		if a == flag && i+1 < len(out) {
			out[i+1] = value
			return out
		}
		if strings.HasPrefix(a, flag+"=") {
			out[i] = flag + "=" + value
			return out
		}
	}
	t.Fatalf("%v carries no %s flag", args, flag)
	return nil
}

// parseTokenOutput reads `standpipe token create`'s screen the way an operator
// does: the token from the "shown once" block, the hash from the YAML snippet.
func parseTokenOutput(out string) (token, hash string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, httpapi.TokenPrefix()+"_"):
			token = line
		case strings.HasPrefix(line, "token_hash:"):
			hash = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "token_hash:")), `"`)
		}
	}
	return token, hash
}

// ---------------------------------------------------------------------------
// Step 3: the example token, against the published scanning regex.
// ---------------------------------------------------------------------------

func TestSetupGuideExampleTokenMatchesTheScanRegex(t *testing.T) {
	// The example is the shape an operator compares their own output against.
	// It is checked against the regex the minter enforces (and
	// docs/reference/tokens.md publishes), so a prefix or width change that
	// leaves the guide behind fails here.
	re := regexp.MustCompile(httpapi.TokenScanPattern())
	example := strings.TrimSpace(oneBlock(t, "token-example").body)
	if loc := re.FindString(example); loc != example {
		t.Errorf("the setup guide's example token does not fully match the published scanning regex.\n"+
			"  example: %s\n  regex:   %s\n  matched: %q", example, re, loc)
	}

	out, err := capture(t, func() error {
		return runToken([]string{"create", "--caller", "release-bot", "--token-id", "tok_a1"})
	})
	if err != nil {
		t.Fatalf("token create: %v\n%s", err, out)
	}
	minted, _ := parseTokenOutput(out)
	if loc := re.FindString(minted); loc != minted {
		t.Errorf("a freshly minted token does not fully match the published regex: %q vs %q", minted, loc)
	}
	if a, b := prefixOf(example), prefixOf(minted); a != b {
		t.Errorf("the guide's example carries prefix %q while `token create` mints %q", a, b)
	}
}

func prefixOf(token string) string {
	if i := strings.Index(token, "_"); i >= 0 {
		return token[:i+1]
	}
	return token
}

// ---------------------------------------------------------------------------
// Every command line the guide tells an operator to type.
// ---------------------------------------------------------------------------

// commandLines returns the standpipe/standpiped lines of a block, tokenised,
// with trailing shell comments removed. Comment lines and other programs are
// skipped.
func commandLines(b taggedBlock) [][]string {
	var out [][]string
	for _, line := range strings.Split(b.body, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, " #"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if !strings.HasPrefix(line, "standpipe ") && !strings.HasPrefix(line, "standpiped ") {
			continue
		}
		out = append(out, strings.Fields(line))
	}
	return out
}

func TestSetupGuideCommandLinesMatchTheCLIsOwnUsage(t *testing.T) {
	// The right-hand side is the binaries' OWN usage text, so a rename that
	// updates the help output and forgets the setup guide fails here.
	var commands [][]string
	for _, role := range commandRoles {
		for _, b := range blocksOf(t, role) {
			commands = append(commands, commandLines(b)...)
		}
	}
	if len(commands) < 6 {
		t.Fatalf("found only %d standpipe command lines in the tagged blocks; the guide walks through "+
			"more than that, so either a tag was lost or the extractor is broken", len(commands))
	}

	for _, argv := range commands {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			usage := operatorUsage
			if argv[0] == "standpiped" {
				usage = daemonUsage
			}
			line := usageLineFor(t, usage, argv)

			// Every long flag the guide types must exist in the usage, as a whole
			// flag: --admin must not pass because --admin-listen exists.
			for _, tok := range argv[1:] {
				if !strings.HasPrefix(tok, "--") {
					continue
				}
				flag := strings.SplitN(tok, "=", 2)[0]
				if !regexp.MustCompile(regexp.QuoteMeta(flag) + `([^A-Za-z0-9-]|$)`).MatchString(line) {
					t.Errorf("the guide types %s, which the binary's own usage line does not offer:\n"+
						"  guide: %s\n  usage: %s", flag, strings.Join(argv, " "), strings.TrimSpace(line))
				}
			}
			// And the sub-verb, where there is one (`config validate`,
			// `token create`).
			if len(argv) > 2 && isWord(argv[2]) && !strings.Contains(line, " "+argv[2]) {
				t.Errorf("the guide types the subcommand %q, which the usage line does not name:\n"+
					"  usage: %s", argv[2], strings.TrimSpace(line))
			}
			// Go's flag package stops at the first positional argument, so a
			// flag written after a <placeholder> argument is not parsed as a
			// flag at all.
			seenPositional := false
			for _, tok := range argv[2:] {
				if strings.HasPrefix(tok, "<") {
					seenPositional = true
					continue
				}
				if seenPositional && strings.HasPrefix(tok, "-") {
					t.Errorf("the guide puts flag %s after a positional argument in %q; the CLI stops "+
						"parsing flags at the first positional argument", tok, strings.Join(argv, " "))
				}
			}
		})
	}
}

// usageLineFor finds the single usage line for argv's verb inside the
// "Usage:" block. The exit-codes block below it names some of the same verbs
// without their flags, and matching there would compare a command against
// prose.
func usageLineFor(t *testing.T, usage string, argv []string) string {
	t.Helper()
	prefix := argv[0]
	if prefix == "standpipe" && len(argv) > 1 {
		prefix += " " + argv[1]
	}
	var found []string
	inBlock := false
	for _, line := range strings.Split(usage, "\n") {
		if strings.HasPrefix(line, "Usage:") {
			inBlock = true
			continue
		}
		if inBlock && strings.TrimSpace(line) == "" {
			inBlock = false
			continue
		}
		if inBlock && strings.HasPrefix(strings.TrimSpace(line), prefix) {
			found = append(found, line)
		}
	}
	if len(found) == 0 {
		t.Fatalf("the guide tells the operator to run %q, and the binary's usage has no line for %q. "+
			"Either the verb was renamed or the guide names one that does not exist.",
			strings.Join(argv, " "), prefix)
	}
	if len(found) > 1 {
		t.Fatalf("%q matches %d usage lines; the matcher cannot pick one", prefix, len(found))
	}
	return found[0]
}

func isWord(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Step 6: the last step, and what about it is checkable without a GitHub App.
// ---------------------------------------------------------------------------

func TestSetupGuideTriggerTargetIsAllowedByTheConfig(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(writeFile(t, filepath.Join(dir, "s6.yaml"),
		setupConfig(t, dir, fakeAppKey(t, dir), mintForTest(t))))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	repo, workflow := setupTriggerTarget(t)
	allowedTarget, allowedCaller := false, false
	for _, target := range cfg.AllowedTargets {
		if target.Repo != repo {
			continue
		}
		for _, w := range target.Workflows {
			if w == workflow || w == "*" {
				allowedTarget = true
			}
		}
	}
	for _, a := range cfg.Callers["release-bot"].Allow {
		if a.Repo == repo && (a.Workflow == workflow || a.Workflow == "*") {
			allowedCaller = true
		}
	}
	if !allowedTarget || !allowedCaller {
		t.Fatalf("the trigger block's target %s/%s is not authorised by the config block "+
			"(allowed_targets: %v, caller allow: %v). An operator following the guide verbatim would "+
			"reach the LAST step and get 403 forbidden.", repo, workflow, allowedTarget, allowedCaller)
	}
}

// setupTriggerTarget parses the /t/{owner}/{repo}/{workflow} path out of the
// trigger block.
func setupTriggerTarget(t *testing.T) (repo, workflow string) {
	t.Helper()
	for _, tok := range strings.Fields(oneBlock(t, "trigger").body) {
		idx := strings.Index(tok, "/t/")
		if idx < 0 {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(tok[idx:], "/t/"), "/")
		if len(parts) != 3 {
			t.Fatalf("the trigger block's path %q is not /t/{owner}/{repo}/{workflow}", tok)
		}
		return parts[0] + "/" + parts[1], parts[2]
	}
	t.Fatalf("the trigger block posts to no /t/ path")
	return "", ""
}

func TestSetupGuideResponseFieldsAreInTheRealBody(t *testing.T) {
	var example map[string]json.RawMessage
	if err := json.Unmarshal([]byte(oneBlock(t, "response").body), &example); err != nil {
		t.Fatalf("the response block is not a JSON object: %v", err)
	}
	if _, ok := example["trigger_event_id"]; !ok {
		t.Errorf("the response block does not show trigger_event_id, the field step 6 tells the operator to read")
	}

	// The keys a real 200 carries, produced by the real server against the
	// fake GitHub, not a hand-written list.
	a := newAcceptance(t)
	a.bootstrap()
	_, body := a.trigger("test.yml", "setup-fields", `{"ref":"refs/heads/main","inputs":{}}`)

	for field := range example {
		if _, present := body[field]; !present {
			t.Errorf("the setup guide shows the 200 body carrying %q; it does not. Keys: %v",
				field, sortedKeys(body))
		}
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestSetupGuideTraceLinesAppearInRealTraceOutput(t *testing.T) {
	// The trace block is the operator's success criterion for the whole
	// setup. Each of its lines is looked for in what `standpipe trace` really
	// prints. GitHub here is internal/testutil's fake; the chain from the App
	// JWT to the dispatch response against real GitHub is what the operator's
	// first real call is for.
	var promised []string
	for _, line := range strings.Split(oneBlock(t, "trace").body, "\n") {
		if strings.TrimSpace(line) != "" {
			promised = append(promised, line)
		}
	}
	if len(promised) < 4 {
		t.Fatalf("the trace block states %d expectations, want at least four: %v", len(promised), promised)
	}

	a := newAcceptance(t)
	a.bootstrap()
	_, body := a.trigger("test.yml", "setup-trace", `{"ref":"refs/heads/main","inputs":{}}`)
	eventID, _ := body["trigger_event_id"].(string)
	if eventID == "" {
		t.Fatalf("no trigger_event_id in %v", body)
	}
	a.bootstrap()

	out, err := capture(t, func() error {
		return runTraceCmd([]string{"--config", a.cfgPath, eventID})
	})
	if err != nil {
		t.Fatalf("standpipe trace: %v\n%s", err, out)
	}
	got := normaliseTrace(out)
	for _, want := range promised {
		if !strings.Contains(got, normaliseTrace(want)) {
			t.Errorf("the setup guide promises the trace shows %q; it does not.\n---\n%s", want, out)
		}
	}
}

// normaliseTrace collapses the column padding `trace` prints with and drops
// the ':' that separates a label from its value, so the guide's lines can be
// compared with a screen written in columns. Punctuation is not the subject;
// the words the operator is told to look for are.
func normaliseTrace(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, ":", " ")), " ")
}
