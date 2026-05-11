# pluginsym

CI gate that enforces plugin-registry symmetry between the per-process
`*ports.Registry` (config-decoder side) and the canonical composition
root (`cmd/gobridge/main.go`) that wires concrete factories into the
`*bridge.Supervisor` / `*bridge.Builder` (factory side).

Asymmetry is the AP-008 anti-pattern: an adapter registers a
`PluginConfig` decoder for kind `"foo"` but no factory is ever wired
(or vice-versa). The runtime then fails at composition time with an
unhelpful error and the asymmetry is invisible to CI.

For every adapter the canonical composition root invokes `Register`
for, this tool asserts:

- Every kind that has a registered `ConfigDecoder` has a corresponding
  wired factory in `cmd/gobridge/main.go`. Aliases (e.g. `aws.sqs`
  → `sqs`, `mqtt.paho` → `mqtt`) collapse via `aliasMap` onto a
  canonical kind, and the canonical-kind group is satisfied if **at
  least one alias is wired**. This matches the supervisor's transport
  / store maps, which are keyed by a single name per backing
  technology, and matches the existing `cmd/gobridge/main.go`
  wiring (`"mqtt"` is wired, the long form `"mqtt.paho"` is not).
- Every wired factory in `cmd/gobridge/main.go` corresponds to a
  registered decoder (no orphan factories). A wired alias is accepted
  if its canonical form (or any other alias of that canonical) is
  registered.

The factory-side wiring is discovered statically by parsing
`cmd/gobridge/main.go` with `go/ast` and collecting every call to:

- `Supervisor.RegisterTransport(name, …)`
- `Supervisor.RegisterStoreFactory(name, …)`
- `Builder.RegisterTransportFactory(name, …)`
- `Builder.RegisterStoreFactory(name, …)`

Only string-literal first arguments are extracted; dynamic wiring is
out of scope and is silently ignored (the analyzer enforces what it
can statically observe).

## Usage

```sh
# From the repository root
go run ./scripts/pluginsym               # exit 1 on any asymmetry
go run ./scripts/pluginsym -v            # also print discovered registered/wired kinds

# Or via the Makefile (used by CI)
make lint-pluginsym
```

Flags:

- `-main <path>` — override the canonical composition root main.go
  (default: `cmd/gobridge/main.go` resolved from the cwd).
- `-v` — verbose; print the registered and wired kind sets to stderr.

## Adding a new adapter

1. Add the adapter's `Register(reg *ports.Registry) error` call to
   `cmd/gobridge/main.go`.
2. Wire the factory via `sup.RegisterTransport(name, …)` /
   `sup.RegisterStoreFactory(name, …)` (or the `Builder` equivalents)
   in the same file, using one of the kinds the adapter registered.
3. Add the same `Register` call to `buildRegisteredKinds` in
   `main.go` here so the analyzer sees the new decoders.
4. If the adapter exposes alias kinds (a short and a long form),
   extend `aliasMap` so the analyzer canonicalizes them onto the
   wired name.

The tool will fail the gate until the decoder side and the factory
side are symmetric.
