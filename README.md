# scooter-cli

`scooter` connects local tools to a [Scooter](https://github.com/chadac-me/scooter)
server. [scooter-mode](https://github.com/chadac-me/scooter-mode) uses it to
chat from Emacs and to open a conversation's sandbox over Tramp.

Work in progress.

## Install

Prebuilt static binaries for Linux and macOS (amd64, arm64) are attached to
each [GitHub release](https://github.com/chadac-me/scooter-cli/releases), with
`SHA256SUMS`; `scooter version` prints the release. Or use Nix:

```sh
nix profile install github:chadac-me/scooter-cli
```

## Commands for scripts and agents

Each prints JSON and exits; flags may come anywhere.

```sh
scooter login --server home --url https://scooter.example.org --api-key  # paste a key from Settings → API keys
scooter login --server home --url https://scooter.example.org --cookie   # or paste _oauth2_proxy
scooter login --server work --url https://scooter.work.example \
  --issuer https://idp.work.example --client-id scooter                  # browser, OIDC + PKCE
scooter servers                          # configured servers, and which are signed in
scooter ls [--active] [--all]            # your conversations, newest first
scooter new [--project] [--wait] "Fix the flaky login test"
scooter send [--wait] ID "Also update the changelog"
scooter wait ID [--timeout 10m]          # until the turn ends or needs you
scooter show ID [--last 5]               # state, pending requests, recent messages
scooter cancel ID | scooter suspend ID | scooter url ID
```

`wait` (and `--wait`) report `state`: `idle` when the turn ended, `needs_user`
when the agent waits on an approval or question (listed in `pending`; answer
it at `url`), `error` when the run failed, or `running` on timeout.
`--project` asks the agent to clone the working directory's git remote and
branch, so push your work first.

Servers live in `~/.config/scooter/config.json` and logins in
`credentials.json` next to it (mode 0600). `--server` takes a name or URL,
then `$SCOOTER_SERVER`, then the default (the first server you log in to, or
`login --default`). OIDC ID tokens refresh on their own; when a login can't
be refreshed, commands exit 3 and ask you to run `scooter login` again.

An API key (Scooter's `apikeys` contrib) needs no auth proxy and doesn't expire
with a browser session. It reaches only your own conversations. It can answer
an agent's questions but not approvals, which stay in the web UI. A revoked or
expired key exits 3 like any other refused login. `scooter tunnel` needs a
cookie or OIDC login.

## `scooter acp`

`scooter acp --server https://scooter.example.org` is an
[ACP](https://agentclientprotocol.com) agent on stdin/stdout. A new ACP session
creates a Scooter conversation; `--conversation <id>` attaches the first one to
an existing conversation instead. Auth headers come from
`--headers-command` (or `--headers-file`), as a JSON object such as
`{"Authorization": "Bearer …"}` or `{"Cookie": "_oauth2_proxy=…"}`. With an API
key, point `--server` at `<url>/webhooks/agent-api/v1` and send it as the
Bearer. The command
is rerun once when the server's auth proxy rejects a request, with `{retry}`
in it replaced by `t` (otherwise `nil`).

```sh
nix run github:chadac-me/scooter-cli -- acp --server https://scooter.example.org \
  --headers-command 'pass show scooter/headers'
```

## `scooter tunnel`

`scooter tunnel --server URL --conversation ID --headers-command CMD` pipes
stdin/stdout to a conversation's sshd, for use as an ssh `ProxyCommand`.
`--spec-file FILE` reads the server, conversation and headers from one JSON
file instead: `{"url": …, "conversation": …, "headers": {…}}`.

## Development

`nix develop -c just ci` runs what CI runs: flake check, `gofmt`, `go vet`,
golangci-lint and the unit tests.
