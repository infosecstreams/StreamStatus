# Pushing to the site as a GitHub App

The bot commits to `infosecstreams/infosecstreams.github.io`. Instead of a
personal access token tied to a person, it can authenticate as a GitHub App:
it signs a JWT with the App's private key, asks GitHub for an installation
token limited to that one repository with `contents: write`, pushes with it
(username `x-access-token`), and replaces it 15 minutes before its one-hour
expiry. Code: `src/githubauth.go`.

Pushes made with an App installation token trigger the site's workflows like
the PAT's pushes did.

## 1. Create the App (owner, in the browser)

1. Go to <https://github.com/organizations/infosecstreams/settings/apps/new>.
2. **GitHub App name:** something unique, e.g. `infosecstreams-streamstatus`.
   **Homepage URL:** `https://infosecstreams.com`.
3. **Webhook:** untick *Active* (the bot doesn't need App webhooks).
4. **Permissions → Repository permissions:** *Contents → Read and write*.
   Leave everything else at *No access* (*Metadata: Read-only* is added
   automatically and can't be removed). No organization or account permissions.
5. **Where can this GitHub App be installed?** *Only on this account*.
6. *Create GitHub App*. On the next page note the **App ID** (a number; the
   Client ID `Iv…` also works).
7. *Private keys → Generate a private key*. A `.pem` downloads. This is the
   only copy; treat it like a password.
8. Left sidebar *Install App* → *Install* next to `infosecstreams` →
   **Only select repositories** → `infosecstreams.github.io` → *Install*.
   The page you land on is
   `https://github.com/organizations/infosecstreams/settings/installations/<INSTALLATION_ID>`;
   note that number.

If `infosecstreams.github.io` has branch protection or a ruleset on `main`
that the PAT's user bypassed, add the App to that bypass list (Settings →
Rules → Rulesets → *Bypass list* → *Add bypass* → Apps).

## 2. Put the key on the host (coordinator)

```bash
scp ~/Downloads/<app-name>.<date>.private-key.pem iss:/home/ec2-user/hosting/secrets/streamstatus-github-app.pem
```

```bash
ssh iss 'cd /home/ec2-user/hosting && chmod 700 secrets && chmod 400 secrets/streamstatus-github-app.pem'
```

Then delete the downloaded `.pem` locally (or keep it only in a password
manager). The compose service mounts `./secrets` read-only at
`/run/secrets` (see `docker-compose.streamstatus.yml`).

Add to `/home/ec2-user/hosting/streamstatus.env`:

```
SS_GH_APP_ID=<App ID>
SS_GH_APP_INSTALLATION_ID=<installation ID>
SS_GH_APP_PRIVATE_KEY_PATH=/run/secrets/streamstatus-github-app.pem
```

Leave `SS_USERNAME`/`SS_TOKEN` in place for now; with the App variables set
they are ignored. Set all three App variables or none: a partial set is a
startup error, not a silent fallback to the PAT.

## 3. Switch over

1. `docker-compose up -d --no-deps streamstatus` (or let the next release
   deploy it).
2. `docker-compose logs --tail 50 streamstatus` should show
   `pushing as GitHub App <id> (installation <id>)` and
   `next GitHub App token refresh in 44m…`. A bad key, ID or installation
   fails at startup with GitHub's error message, before anything else runs,
   so the deploy script rolls back.
3. Wait for the next status change (or a reconcile that fixes one) and check
   the push landed in the site repo.
4. Revoke the old PAT (<https://github.com/settings/tokens>) and remove
   `SS_USERNAME`/`SS_TOKEN` from `streamstatus.env`.

## Rotating the key

Generate a second key in the App settings, replace
`secrets/streamstatus-github-app.pem`, restart the service, confirm a token
refresh in the logs, then delete the old key in the App settings.
