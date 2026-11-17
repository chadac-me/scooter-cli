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

## `scooter acp`

`scooter acp --server https://scooter.example.org` is an
[ACP](https://agentclientprotocol.com) agent on stdin/stdout. A new ACP session
creates a Scooter conversation; `--conversation <id>` attaches the first one to
an existing conversation instead. Auth headers come from
`--headers-command` (or `--headers-file`), as a JSON object such as
`{"Authorization": "Bearer …"}` or `{"Cookie": "_oauth2_proxy=…"}`. The command
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
