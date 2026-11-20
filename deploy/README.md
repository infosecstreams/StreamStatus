# Deploying StreamStatus

Releases deploy themselves. Pushing a `vX.Y.Z` tag runs
`.github/workflows/build-and-push.yml`, which:

1. runs the test workflow (`go vet`, `go test -race`, `govulncheck`);
2. builds the image and pushes `ghcr.io/infosecstreams/streamstatus:vX.Y.Z` and `:latest`;
3. signs the pushed digest with keyless cosign (GitHub OIDC; no keys to manage),
   checks that signature exactly as the host will, and attaches a GitHub build
   provenance attestation;
4. SSHes to the host with a deploy key that can only run `deploy-streamstatus`,
   passing nothing but `deploy sha256:<digest>`.

On the host, [`deploy-streamstatus`](deploy-streamstatus) verifies the
signature (signed by this repo's `build-and-push.yml` on a `v*` tag), pulls the
image by digest, tags the running image `streamstatus-rollback:previous`, pins
`STREAMSTATUS_IMAGE=ghcr.io/infosecstreams/streamstatus@sha256:<digest>` in
`/home/ec2-user/hosting/.env`, recreates only the `streamstatus` service
(`--no-deps`), and waits for `server starting on` in the logs followed by 30 s
without a restart. If that doesn't happen it rolls back to the previous image
and the workflow fails.

Pre-release tags (`v1.4.0-rc.1`) build, push and sign but never deploy or move
`:latest`. A `workflow_dispatch` from a branch builds a signed `:sha-<commit>`
image and never deploys.

## Why push over SSH, and not Watchtower or SSM

- **Watchtower** (or any registry poller) would deploy whatever `:latest` points
  at without checking a signature, on its own schedule, with no rollback and no
  result in the Actions UI. It also needs the Docker socket in a long-running
  container. A pull model is attractive in that GitHub would hold no
  credentials, but the forced-command key below holds very little: it can only
  ask the host to deploy a digest that is already signed by our release
  workflow, i.e. at worst re-deploy an older release.
- **AWS SSM Run Command with GitHub OIDC** is the other clean option (no SSH
  key at all, no inbound port). It needs an IAM OIDC provider, a role trusted
  only for `repo:infosecstreams/StreamStatus:ref:refs/tags/v*`, an SSM
  document and the SSM agent/instance profile. Worth it if port 22 is ever
  closed to the internet; the host script stays the same, only the transport
  in the `deploy` job changes.
- **Push over SSH with a forced command** reuses what exists (port 22, the
  `ec2-user` account), gives the workflow a pass/fail and the deploy log, and
  the key is useless for anything else (`restrict` = no shell, PTY or
  forwarding; the command is fixed; the input is one strictly validated
  digest).

Defence in depth on the GitHub side: the key lives in a `production`
environment whose secrets are only released to `v*` tag refs, and a tag
ruleset limits who can create `v*` tags.

## One-time host setup (coordinator, on `ssh iss`)

All paths are in `/home/ec2-user/hosting`. Take a backup first:

```bash
cd /home/ec2-user/hosting && cp -a docker-compose.yml docker-compose.yml.bak.$(date +%F) && cp -a nginx nginx.bak.$(date +%F) 2>/dev/null; ls -la .env 2>/dev/null; cat .env 2>/dev/null
```

1. **cosign.** Copy `deploy/install-cosign.sh` over and run it (pinned v3.1.4,
   checksum-verified, installs `/usr/local/bin/cosign`).
2. **Deploy script.**
   `install -m 0755 deploy-streamstatus /home/ec2-user/hosting/deploy-streamstatus`.
   It must not be writable by anyone but `ec2-user`.
3. **Compose.** Replace the `streamstatus` service in `docker-compose.yml` with
   the one in [`docker-compose.streamstatus.yml`](docker-compose.streamstatus.yml)
   and add the top-level `volumes: streamstatus-data:`. Changes against today:
   image from `${STREAMSTATUS_IMAGE:-…:latest}`, a named volume at `/data` for
   the SQLite DB, `./secrets` mounted read-only at `/run/secrets`, a `/tmp`
   tmpfs, port 8080 published on loopback only, rotated logs. Then
   `mkdir -m 700 -p secrets` and `docker-compose config -q`.
   If the existing file declares `version: "2"` (not 2.1+), the `${VAR:-default}`
   syntax is unsupported; use `3.x`. If `.env` already exists, the script only
   ever rewrites its `STREAMSTATUS_IMAGE=` line.
   Check nothing else relies on port 8080 being public (Twitch reaches the bot
   through nginx) before switching to `127.0.0.1:8080:8080`; close 8080 in the
   security group too if it is open.
4. **Proxy.** The host now runs Caddy (see `deploy/edge/caddy/Caddyfile`). It
   routes `/webhook/callbacks` and `/streamstatus/*` to the bot and everything
   else to Plausible, which owns `/api/*` and `/live/*`. Never route `/api/` or
   `/live/` to the bot: that breaks analytics.
5. **Recreate once with the new compose file**, still on the old image:
   `docker-compose up -d --no-deps streamstatus`, then
   `./deploy-streamstatus status` (shows no pin yet). Images built before this
   workflow are unsigned, so the script will refuse them; the first pin is
   written by the first release made with the new workflow, and from then on
   every deploy has a signed rollback target.
6. **Deploy key.** Generate it on your own machine, never on CI:

   ```bash
   ssh-keygen -t ed25519 -N '' -C streamstatus-deploy@github-actions -f streamstatus-deploy
   ```

   Append one line to `/home/ec2-user/.ssh/authorized_keys` on the host:

   ```
   restrict,command="/home/ec2-user/hosting/deploy-streamstatus" ssh-ed25519 AAAA…(streamstatus-deploy.pub)… streamstatus-deploy@github-actions
   ```

   Record the host key for CI: `ssh-keyscan -t ed25519 <host> > known_hosts`
   and compare its fingerprint (`ssh-keygen -lf known_hosts`) with the host's
   own (`ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` on the host).

## One-time GitHub setup (owner)

In `infosecstreams/StreamStatus` → Settings:

1. **Environments → New environment `production`.**
   - Deployment branches and tags → *Selected branches and tags* → add a **tag**
     rule `v*` (no branches). Runs from any other ref can't read its secrets.
   - Environment secrets: `DEPLOY_SSH_KEY` = contents of `streamstatus-deploy`
     (private key), `DEPLOY_SSH_KNOWN_HOSTS` = contents of `known_hosts`.
   - Environment variables: `DEPLOY_HOST` = the host's public DNS name or IP;
     optionally `DEPLOY_USER` (default `ec2-user`).
   - Leave "Required reviewers" off for fully automatic deploys, or add
     yourself to approve each one.
   Then delete the local private key file.
2. **Rules → Rulesets → New tag ruleset** targeting `v*`: restrict creations,
   updates and deletions to admins (bypass list = you). Only people who can
   create release tags can trigger deploys.
3. **Actions → General → Workflow permissions:** "Read repository contents"
   (the workflows request what they need per job).
4. Make sure the `streamstatus` package on ghcr.io stays public (the host pulls
   and verifies anonymously).

## Testing a deploy safely

1. Tag a pre-release, e.g. `v1.4.0-rc.1`. It builds, pushes, signs and attests
   but does not deploy. Check the run's summary for the digest.
2. Actions → *Build, sign and deploy* → *Run workflow*, **Use workflow from:
   Tags → v1.4.0-rc.1**, deploy mode **verify**. The deploy job connects with
   the real key and the host verifies the signature and pulls the image,
   changing nothing. This proves the key, the environment rule, known_hosts,
   cosign and the identity check end to end. (It also rebuilds and re-signs
   the rc image; harmless.)
3. On the host, `./deploy-streamstatus verify sha256:<digest>` and
   `./deploy-streamstatus status` give the same answers by hand.
4. Release for real with `v1.4.0`. Watch the deploy job's log; it ends with
   `DEPLOYED ghcr.io/infosecstreams/streamstatus@sha256:…`.

Negative checks worth doing once: a `workflow_dispatch` from `main` must skip
the deploy job; `ssh -i streamstatus-deploy ec2-user@<host>` must print the
usage line and exit, as must `ssh … 'deploy sha256:abc; id'`.

## Day-2

Run the script by hand **on the host** as
`env -u SSH_CONNECTION ./deploy-streamstatus status` (or `rollback`). Inside
any SSH session without the forced command it refuses with the usage line,
because it can't tell an admin shell from someone trying to get a shell
with the deploy key.


- **Status:** `./deploy-streamstatus status` (pin, running image, last deploys).
  The full log is `/home/ec2-user/hosting/.deploy-streamstatus/deploy.log`.
- **Roll back:** automatic on a failed deploy. By hand, on the host:
  `./deploy-streamstatus rollback` (back to the image before the last deploy).
  Or from GitHub: open the older release's workflow run and *Re-run* its
  `deploy` job, which redeploys that run's signed digest.
- **Manual deploy of a specific signed digest:** `./deploy-streamstatus deploy sha256:<digest>`.
- **Health URL:** once `/streamstatus/status.json` ships, set
  `HEALTH_URL='http://127.0.0.1:8080/streamstatus/status.json'` in the script's
  configuration block so a deploy also requires an HTTP 200.
- **Rotating the deploy key:** add the new `authorized_keys` line, update
  `DEPLOY_SSH_KEY`, remove the old line.
- **Tests:** `deploy/test/run.sh` exercises the script against fake
  docker/compose/cosign; CI runs it with shellcheck.

## Verifying images yourself

```bash
cosign verify ghcr.io/infosecstreams/streamstatus@sha256:<digest> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/infosecstreams/StreamStatus/\.github/workflows/build-and-push\.yml@refs/tags/v'
gh attestation verify oci://ghcr.io/infosecstreams/streamstatus@sha256:<digest> -R infosecstreams/StreamStatus
```
