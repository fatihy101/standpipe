<!--
This guide is executed by internal/cli/setup_guide_test.go.

Only fenced code blocks whose info string carries a `setup-test:<role>`
attribute are read by the test, for example:

    ```yaml setup-test:config
    ```sh setup-test:commands

Prose is never parsed. The roles are:

  config         the standpipe.yaml example (exactly one); loaded by the real
                 config loader after filling the key path, database path and
                 token_hash placeholders
  mint           the token-then-migrate commands (exactly one); executed in order
  commands       other command blocks; every standpipe/standpiped line is checked
                 against the binaries' own usage text
  token-example  an example token (exactly one); must match the published
                 secret-scanning regex in full
  trigger        the curl call (exactly one); its /t/{owner}/{repo}/{workflow}
                 target must be allowed by the config block
  response       an example 200 body (exactly one); every key must be in the
                 body the real server returns
  trace          expected `standpipe trace` lines (exactly one); each line must
                 appear in the real trace output

Blocks of the roles mint, commands and trigger are all usage-checked. Untagged
blocks are ignored. Put flags BEFORE positional arguments: the CLI uses Go's
flag package, which stops at the first positional argument.
-->

# Setup: a first real caller through the gateway

This guide takes you from no GitHub App to a real caller triggering a workflow through
Standpipe. It takes about **15 minutes**, including creating the GitHub App.

## Prerequisites

1. **A GitHub App.** It can be created under your personal account or an organization.
2. **A test repository** with at least one workflow that has a `workflow_dispatch` trigger.
3. The `standpipe` and `standpiped` binaries (see *Install* in [index.html](index.html#install)).

Why an App and not a personal access token: a PAT belongs to one person, breaks everywhere
when it is revoked, and with the `repo` scope reaches every repository. An App installation
token lives for one hour and can be limited to a set of repositories.

## 1. Create the GitHub App

**Settings → Developer settings → GitHub Apps → New GitHub App**

| Field | Value |
|---|---|
| Name | anything, e.g. `standpipe-dev` |
| Homepage URL | anything |
| Webhook | **off**: Standpipe does not use webhooks |

**Repository permissions**: these three, and no others:

| Permission | Level | Used for |
|---|---|---|
| Actions | **Read and write** | dispatching workflows |
| Metadata | Read-only | listing repositories and workflows |
| Contents | Read-only | reading workflow files to validate `inputs` |

> `actions: write` lets the App run arbitrary code in that repository's CI. Use a test
> repository.

After creating it:

- Note the **App ID** (top of the page).
- **Generate a private key** and keep the downloaded `.pem` file.
- **Install App** on the test repository only, not *All repositories*.

## 2. Write the configuration

```sh setup-test:commands
mkdir -p /etc/standpipe          # or a local directory
cp ~/Downloads/*.private-key.pem /etc/standpipe/github-app.pem
chmod 600 /etc/standpipe/github-app.pem
```

`standpipe.yaml`:

```yaml setup-test:config
github:
  app_id: 123456                                   # your App ID
  private_key_path: /etc/standpipe/github-app.pem

storage:
  sqlite_path: ./standpipe.db

allowed_targets:
  - repo: you/your-test-repo                      # your repository
    workflows: [probe.yml]                        # your workflow file
    refs: [refs/heads/main]

# callers is a MAP keyed by the caller's name, not a list
callers:
  release-bot:
    token_id: tok_a1
    token_hash: ""            # filled in step 3; migrate refuses an empty hash
    role: caller
    allow:
      - repo: you/your-test-repo
        workflow: probe.yml        # SINGULAR here
        ref: refs/heads/main       # SINGULAR here
```

> **Two naming shapes meet here.** `allowed_targets` defines a *set* of targets, so its
> `workflows` and `refs` are **lists**. A caller's `allow` is a list of
> `{repo, workflow, ref}` **triples** with **singular** fields. Copy the plural into `allow`
> and the loader stops with `unknown field(s): callers.release-bot.allow[0].workflows`.

> **The repository and workflow appear in three places**: here (in `allowed_targets` *and*
> in the caller's `allow`), in step 4 (`onboard`) and in step 6 (the `curl` path). **All
> three must match.** Change them together when you substitute your own repository; if you
> miss one, the error appears only at the last step, as `403 forbidden`. Use the workflow's
> **file name** (`probe.yml`), not its `name:`. Refs must be written in full
> (`refs/heads/main`): a short name like `main` is rejected because it could be a branch or
> a tag.

## 3. Mint a token first, then create the schema

**The order matters.** The configuration is not valid while `token_hash` is empty: both
`config validate` and `migrate` refuse it. So mint the token first, paste its hash, then
validate and create the schema.

```sh setup-test:mint
standpipe token create --caller release-bot --token-id tok_a1
# paste the printed token_hash value into standpipe.yaml, then:
standpipe config validate --file standpipe.yaml
standpipe migrate --config standpipe.yaml
```

The token is shown **once**. It looks like this (placeholder value):

```text setup-test:token-example
spd_tok_a1_EXAMPLE000000000000000000000000000000000000_example
```

Store it as the caller's secret. The command also prints a `standpipe.yaml` entry for the
caller; copy its `token_hash` value into your configuration. Before printing a token, `token create` checks it against the gateway's own
parser and against the published secret-scanning regex, and refuses to print it if either
rejects it. The format and the regex are in [reference/tokens.md](reference/tokens.md).

`config validate` prints `valid: true` and a short summary. A **missing** App key is only a
warning here (it is fatal when `standpiped` starts); a key that is present is actually parsed
by the daemon's own loader, so a truncated PEM, a corrupt DER body, an EC key or plain garbage
fails with `valid: false`.

If you set `observability.watch_ips`, a non-canonical IP spelling (`0:0:0:0:0:0:0:1`,
`2001:DB8::1`, `::ffff:192.0.2.7`) is canonicalised at load time and `validate` prints a
warning naming both forms. Without that, the entry would silently never match.

## 4. Onboard the repository

```sh setup-test:commands
standpipe onboard --repo you/your-test-repo
```

Without `--workflows` it discovers the repository's `workflow_dispatch` workflows and asks for
confirmation. It also checks that the App is actually installed, so a missing installation
fails here rather than at the first trigger.

## 5. Start the gateway

```sh setup-test:commands
standpiped --config standpipe.yaml --listen :8443 --admin-listen 127.0.0.1:9090
```

In another terminal:

```sh setup-test:commands
standpipe status --config standpipe.yaml
```

## 6. The real caller

```sh setup-test:trigger
curl -sS -X POST http://localhost:8443/t/you/your-test-repo/probe.yml \
  -H "Authorization: Bearer spd_tok_a1_..." \
  -H "Idempotency-Key: $(uuidgen)" \
  -H "Content-Type: application/json" \
  -d '{"ref":"refs/heads/main","inputs":{}}'
```

Expect **200**. The body carries `trigger_event_id` and, when tier 1 correlation worked,
`run_id`:

```json setup-test:response
{
  "trigger_event_id": "evt_…",
  "run_id": 1234567890
}
```

The field is `trigger_event_id`. Looking for `.event_id` returns `null`, and nothing tells you
why.

Then trace it. Flags go **before** the event id:

```sh setup-test:commands
standpipe trace --config standpipe.yaml <trigger_event_id>
```

The trace should include:

```text setup-test:trace
resolution: exact
source: dispatch_response (tier 1)
status: forwarded
integrity: content_sha256 OK
```

At this point a real caller has gone through the gateway, and setup is complete.

### Optional: two observations worth recording

These are not troubleshooting steps. They are measurements only someone with a real App can
take, and both answers are useful.

**Is the run listable the moment the dispatch returns?** Tier 1 correlation reads `run_id`
from the dispatch response; the resolver's other tiers find runs by *listing* a workflow's
runs. Whether a run already appears in that listing when the dispatch returns has not been
measured. Right after the `curl` above returns 200, note the `run_id` and immediately run
the resolver's own query (your own `gh` login is enough; identity is not the point, listing
is):

```sh
gh api "/repos/you/your-test-repo/actions/workflows/probe.yml/runs?event=workflow_dispatch&per_page=100" \
  --jq '.workflow_runs[0:5][] | "\(.id) \(.status) \(.created_at)"'
```

Either the `run_id` is in the list, or it is not; if not, repeat once a second and note on
which attempt it appears. Neither answer means your setup is broken: step 6 already succeeded
from the response body alone. Do not re-dispatch to "fix" it; a second dispatch is a second
sample.

**Does the quota series appear?** `gateway_github_rate_remaining{bucket=…}` exists only after
a **successful** GitHub call, because failed App JWT calls carry no `X-RateLimit-*` headers.
Step 6 is the first moment it can appear:

```sh
curl -sS http://127.0.0.1:9090/metrics | grep gateway_github_rate
```

Record which `bucket=` labels appear, with `remaining` and `reset_timestamp` per bucket. Then
trigger again and check whether a bucket's `reset_timestamp` ever moves **backwards**.

### Try a tag as well

GitHub accepts a full tag ref (`refs/tags/v0.1`) for dispatch. To check that the gateway's ref
validation does too, first add the ref in **both** places in step 2: the `allowed_targets`
`refs` list and the caller's `allow` (as a second triple, or with `ref: "*"`). Otherwise you
get `403` and are testing your own allowlist instead.

## What is tested, and what is not

`internal/cli/setup_guide_test.go` executes the tagged code blocks of this guide against the
real code: the configuration example through the daemon's own strict loader, the
token-then-migrate order, the example token against the published scanning regex, every
`standpipe`/`standpiped` command line against the binaries' own usage text, the `curl` target
against the configuration's allowlist, and the 200 body and trace lines against the output of
the real server running against a fake GitHub.

What it cannot test is the half that needs credentials: a GitHub App, its private key and an
installation. Steps 2 and 3 work end to end without an App (`config validate` treats a missing
`.pem` as a warning, and `migrate` never reads the key). The first command that needs the App
is `onboard` in step 4, and steps 5 and 6 depend on the same identity. A test also binds only
what this guide **says**; a missing step is invisible to it.

## Troubleshooting

If the gateway itself is unreachable, see the [runbook](runbook.md). The most likely errors on
a first setup:

| Symptom | Cause |
|---|---|
| `503 app_not_found` | **`app_id` names no App.** Installing does not fix this; correct `app_id`. GitHub answers `404 "Integration not found"` for an unknown App. |
| `503 installation_not_found` | An installation was looked up and not found: the App is not installed on that repository, the repository does not exist, or the App cannot see it. `standpipe onboard --repo <owner/repo>` tells the three apart. |
| `503 credential_expired` | The `.pem` path is wrong or unreadable. |
| `403 forbidden` | The target is not in `allowed_targets`, or the caller's `allow` does not cover it. Repository and workflow must match in all three places. |
| `unknown field(s): …allow[0].workflows` | A plural field inside a caller's `allow`; use `workflow` / `ref`. |
| `422 upstream_rejected` | GitHub rejected the request; the message is in `detail`, most likely about the ref form. |
