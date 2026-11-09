# Runbook: direct calls while the gateway is unreachable

What to do when the Standpipe gateway cannot be reached. Read it in order and apply it in order.

**Prerequisites. Prepare these today; they cannot be prepared during an outage:**

- Every caller machine has a **local clone** of the repository that holds `routing.json`. Fetching `routing.json` from GitHub raw during an outage is **forbidden**: the thing you need to read may be in the same place as the thing you cannot reach.
- The `GITHUB_PAT_FALLBACK` environment variable is set. The gateway's GitHub App identity **cannot** be used on this path.

The `standpipe` commands below read `standpipe.yaml` from the current directory by default; pass `--config PATH` otherwise. Flags go **before** the event id: `standpipe trace --json <trigger_event_id>`, not `standpipe trace <trigger_event_id> --json`.

---

## 1. Decision: switch to a direct call?

Look at the response you got from the gateway. **A situation with no row in this table does not justify a direct call.**

| What you see | Meaning | Action |
|---|---|---|
| TCP connection cannot be established (5 s or more) | The gateway is not running | **Go to step 2** |
| No HTTP response (read of 10 s or more) | The gateway is not answering | **Go to step 2**, after step 1a |
| `503` + `"error":"backend_unreachable"` | The gateway is up but cannot reach GitHub | **Go to step 2.** You may not be able to reach it either |
| `503` + `"detail":"credential_expired"` | The gateway's credential is dead | **Go to step 2.** Retrying does not help |
| `503` + `"detail":"installation_not_found"` | **No installation found** for the target repository: the App is not installed, **or** the repository does not exist, **or** the App cannot see it. The response does not say which | **Go to step 2.** Tell the operator: `standpipe onboard --repo <owner/repo>` tells the three apart |
| `503` + `"detail":"app_not_found"` | `github.app_id` names an App that does not exist. **Installing the App does not fix this** | **Go to step 2.** Tell the operator: the gateway's configuration needs fixing, not the target repository |
| `429` | Rate limit | **WAIT.** Wait for `Retry-After`, then retry |
| `502` | GitHub upstream error | **WAIT.** Retry |
| `500` + `"error":"internal_error"` | The gateway is up but **cannot keep records** | **STOP.** Do not switch to a direct call. Alert the operator, wait for `Retry-After` |
| `503` + `"error":"storage_degraded"` | Free disk is below the threshold | **STOP.** Alert the operator, wait for `Retry-After` |
| `4xx` (400/401/403/409/413/415/422) | The request is invalid or unauthorized | **STOP.** A direct call would also be rejected, or would make an unauthorized dispatch |

**Why `500` means STOP:** an unrecorded dispatch is worse than a recorded one.

### 1a. If you got a timeout: the dispatch may have gone out

A timeout does **not** mean the dispatch did not happen. Answer this first.

**If you have access to the gateway machine** (operator): the SQLite file is in place and readable even if the process is dead.

```sh
standpipe trace <trigger_event_id>
standpipe trace --json <trigger_event_id>
```

If you do not have the `trigger_event_id`:

```sh
standpipe status
```

Look at the `unresolved` count in the `TRIGGERS` block.

Read the `trace` output like this:

| `trace` output | Meaning | Action |
|---|---|---|
| `OUTCOME ... status forwarded` | The dispatch **went out** | **Do NOT make a direct call.** The job is running |
| `RUN LINK` line carries a run id | The dispatch went out and the run is linked | **Do NOT make a direct call** |
| `OUTCOME — (none recorded ...)` | The dispatch result could not be written; uncertain | **Step 1b** |
| Event not found | No record; the dispatch most likely did not go out | **Go to step 2** |
| `integrity ... MISMATCH` | The record is corrupted | **STOP.** Operator intervention |
| `integrity ... UNVERIFIABLE (unknown_schema_version)` | The record is **not** corrupted: the row was written with another schema version that this build cannot verify | Ignore integrity; decide from the `OUTCOME` / `RUN LINK` lines |
| `integrity ... UNVERIFIABLE (row_malformed)` | The row has the wrong shape: a writer bug | **STOP.** Treat it like `MISMATCH`, operator intervention |

**Why `UNVERIFIABLE` is not `MISMATCH`:** integrity has three values. `UNVERIFIABLE` does not mean "the record is corrupted"; it means "this build cannot verify this row", and it is never printed without a reason. **Read** the reason: the schema-version reason is an expected state and does not affect the direct-call decision. The line that makes that decision is `OUTCOME`.

**If you do not have access** (a caller application): you cannot run `standpipe trace`. Go to step 1b.

### 1b. Under uncertainty: is a double dispatch acceptable?

You must have made this decision **in advance, for each pipeline**.

- **A double dispatch is acceptable** (idempotent work, for example a test run): go to step 2.
- **It is not acceptable** (deploy, release, tagging): **STOP.** Wait for the operator. The cost of waiting is smaller than the cost of deploying twice.

---

## 2. Determine the backend

1. **Update the local clone** if you can. If the network is available:
   ```sh
   git -C /path/to/repo pull --ff-only
   ```
   If this fails, **carry on** with the clone you have. Do **not** try to fetch the file with `curl`, `raw.githubusercontent.com` or the GitHub API.
2. Read the file:
   ```sh
   cat /path/to/repo/routing.json
   ```
3. Take the `default_backend` value. **In Phase 0 it is always `"github"`.** If you see any other value: **STOP** and tell the operator.
4. `overrides` is empty in Phase 0. If it is not: **STOP** and tell the operator.

---

## 3. Call GitHub directly

```sh
curl -X POST \
  -H "Authorization: Bearer $GITHUB_PAT_FALLBACK" \
  -H "Accept: application/vnd.github+json" \
  "https://api.github.com/repos/<owner>/<repo>/actions/workflows/<workflow.yml>/dispatches" \
  -d '{"ref":"refs/heads/main","inputs":{"env":"staging"}}'
```

Rules:

- **Do not send `standby_trigger_id`.** The gateway generates that input, and there is no identity to generate on the direct path; sending one creates a false correlation record.
- `inputs` can carry at most **25** keys (GitHub's limit).
- The expected response is **`204 No Content`**. No run id is returned.
- `422`: the `ref` or `inputs` are wrong. Do not retry; fix the request.
- `404`: the workflow does not exist or the PAT lacks permission. Do not retry.

**Record the command you ran and when you ran it.** Step 4 depends on it.

---

## 4. Record it

A direct call does **not** appear in the gateway's log. This is expected and accepted; it contributes to the coverage gap.

For every direct call, write down the following (ticket, incident log, team channel; where it goes should be fixed for the team):

1. Timestamp (UTC, RFC 3339).
2. `owner/repo`, workflow file, `ref`.
3. The `inputs` keys sent, **not their values**.
4. The gateway symptom you saw in step 1 (which row).
5. The step 1b decision: was the double-dispatch risk accepted?
6. The **owner** of the PAT used, not the token itself.

**Do not write secrets.** Tokens, PATs and `inputs` values never go into any record.

---

## 5. Return to normal

1. Confirm the gateway is up:
   ```sh
   curl -sf http://<gateway-host>:<port>/healthz
   standpipe status
   ```
   If callers reach the gateway through a Cloudflare Tunnel that only exposes `/t/`, `/healthz` is not reachable through the tunnel; run the check from the gateway host or a network that reaches the listener directly.
2. Move the caller back to the normal path. **Do not leave it on the direct path behind a permanent flag.**
3. Check triggers that timed out during the outage and remained `unresolved`:
   ```sh
   standpipe status
   standpipe trace <trigger_event_id>
   ```
4. Add the step 4 records to the coverage-gap review.

---

## 6. Caller application template

A skeleton that carries the decisions of steps 1 and 1b in code. `ALLOWS_DUPLICATE_DISPATCH` is set deliberately **per pipeline**; its default is `false` and it should stay that way.

```python
import os, time, requests

GATEWAY_URL       = "https://gateway.acme.dev/t/acme/core/test.yml"
GITHUB_DIRECT_URL = "https://api.github.com/repos/acme/core/actions/workflows/test.yml/dispatches"
GATEWAY_TOKEN     = os.environ["STANDPIPE_TOKEN"]
GITHUB_PAT        = os.environ["GITHUB_PAT_FALLBACK"]

payload   = {"ref": "refs/heads/main", "inputs": {"env": "staging"}}
build_id  = os.environ.get("BUILD_ID", "unknown")
idem_key  = f"deploy-{build_id}"
headers   = {"Authorization": f"Bearer {GATEWAY_TOKEN}", "Idempotency-Key": idem_key}
allows_duplicate = os.environ.get("ALLOWS_DUPLICATE_DISPATCH", "false") == "true"

def direct():
    # Step 3. standby_trigger_id is NOT sent.
    r = requests.post(GITHUB_DIRECT_URL, json=payload,
                      headers={"Authorization": f"Bearer {GITHUB_PAT}",
                               "Accept": "application/vnd.github+json"},
                      timeout=15)
    r.raise_for_status()
    # Step 4: record this call.

try:
    r = requests.post(GATEWAY_URL, json=payload, headers=headers, timeout=10)

    # 409 in_progress: the earlier request is still running. Do NOT switch to a direct call (step 1).
    if r.status_code == 409 and r.json().get("error") == "idempotency_in_progress":
        time.sleep(r.json().get("retry_after_seconds", 5))
        r = requests.post(GATEWAY_URL, json=payload, headers=headers, timeout=10)

    # 500 / storage_degraded: an unrecorded dispatch is worse than a recorded one (step 1).
    if r.status_code == 500 or (r.status_code == 503 and r.json().get("error") == "storage_degraded"):
        raise RuntimeError("Gateway cannot keep records: no direct call, operator needed.")

    # 503 backend_unreachable / credential_expired / installation_not_found
    # / app_not_found: step 2.
    if r.status_code == 503:
        direct()
    else:
        r.raise_for_status()

except (requests.ConnectionError, requests.Timeout):
    # Step 1a: the dispatch MAY have gone out. We cannot run `standpipe trace` here.
    if allows_duplicate:
        direct()
    else:
        raise RuntimeError(
            f"Gateway timeout: double-dispatch risk. Check {idem_key} with `standpipe trace`. "
            f"Operator intervention needed.")
```
