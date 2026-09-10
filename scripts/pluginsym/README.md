# pluginsym

CI gate for decoder/factory symmetry in a build-tagged composition package.
It reads **every non-test `.go` file** in the directory, independently of the
host platform and active build tags. Each file is checked separately:

1. **R1 — symmetry:** kinds registered by adapter `Register(reg)` calls must
   equal the factory kinds wired in that file, after alias collapse.
2. **R2 — one owner:** an adapter import path may register in only one file.
3. **R3 — empty stubs:** files negating a family tag must contain no adapter
   registration or factory wiring.
4. **R4 — additive families:** the positive constraint must be exactly
   `gobridge_<family> || gobridge_all`; the inverse stub must be exactly
   `!gobridge_<family> && !gobridge_all`. Extra conditions, all-only tags,
   mixed constraints and implicit GOOS/GOARCH filename suffixes are rejected.
5. **R5 — blank root:** files without family tags must contain no adapter
   registration or factory wiring. Untagged seed paths are not an exception.

The gate resolves adapter calls through each file's own import table and invokes
the corresponding entry in `adapterRegistrars` on a fresh `ports.Registry`.
Factory calls are collected receiver-agnostically:

- `RegisterTransport("kind", ...)`
- `RegisterTransportFactory("kind", ...)`
- `RegisterStoreFactory("kind", ...)`

Supervisor and Builder calls for the same kind deduplicate. All kind arguments
must be string literals; dynamic wiring fails rather than hiding from the gate.
Taking a registrar or wiring method as a function value is also rejected.
Unrelated registrations, such as the file credential resolver, are not plugin
decoder registrations.

Aliases collapse to the same canonical kind: `mqtt.paho` → `mqtt`,
`aws.sqs` → `sqs`, `azure.servicebus` → `servicebus`,
`amqp.amqp091` → `amqp091`, and `amqp.amqp10` → `amqp10`.
Wiring at least one alias satisfies its group.

Since a build includes or excludes a whole file, per-file symmetry composes
across family selections without enumerating every tag combination.

## Usage

```sh
# From the repository root
go run ./scripts/pluginsym -dir cmd/gobridge
go run ./scripts/pluginsym -dir cmd/gobridge -v
make lint

go -C scripts/pluginsym test ./...
```

`-dir` defaults to `cmd/gobridge`, relative to the working directory. `-v` prints
each file's registered and wired kinds. Violations include the file and rule,
and exit with status 1; directory/read/Go syntax errors exit with status 2.

## Adding an adapter

1. Put its `Register(reg)` call and literal factory wiring in the same
   `plugins_<family>.go` file with the additive family constraint.
2. Keep any Builder seed-store wiring in that same file.
3. Add the inverse stub with no registration/wiring calls.
4. Add the adapter import path and `Register` function to `adapterRegistrars`
   in this tool, plus its module requirement.
5. Extend `aliasMap` if the adapter exposes aliases.

The gate rejects registration drift until both sides agree.
