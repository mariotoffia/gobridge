# Runbook: External Config Writers Must Write Atomically

**Applies to:** any process OTHER than gobridge that writes the config file
gobridge watches — a deploy script, a config templating tool (Helm, Jsonnet,
`envsubst`), a person editing the file in an editor, or a job that renders the
file from a database.
**Audience:** operators and anyone whose tooling writes the watched config file.
**Risk:** high — an external in-place write can make gobridge silently stop
forwarding messages while the process stays healthy and probes keep passing.

## Overview

gobridge watches its config file and reloads it while running (hot reload). If an
external tool writes that file **in place** — opening the existing file and
overwriting its contents where they sit — the watcher can read the file while the
write is only partly done. A partly-written file can, by bad luck, parse as a
valid config with no routes, and gobridge will switch the live bridge over to that
empty config and stop forwarding messages. No error is logged, because from
gobridge's point of view it loaded a valid config.

The fix is a one-line change in how the external tool writes the file: write to a
temporary file in the same directory, then rename it over the target. gobridge's
own writer already does this, so this runbook is only about EXTERNAL writers.

gobridge also carries an in-process safety net — a content-stability gate that
holds a change back until it reads the same new bytes twice across the settle
window — which closes the *common* torn-read window. It narrows the risk but does
not remove it, so atomic writing remains required. See
[In-process mitigation: the content-stability gate](#in-process-mitigation-the-content-stability-gate)
below.

## What the file watcher is doing

While gobridge runs, a background watcher keeps an eye on the config file. When the
file changes, the watcher reads it, parses it, validates it, and — if it is valid
— swaps the running bridge over to the new config without a restart. This is the
hot-reload feature that lets you change routes, senders, and receivers on a live
bridge.

The watcher reacts to filesystem change events (and, as a backstop, a periodic
content check). It cannot tell *why* the file changed or whether the tool that
changed it has finished writing. It sees "the file changed" and reads whatever is
on disk at that moment.

## How an in-place write can be read half-finished

"In place" means the writer opens the existing file and overwrites its bytes where
they sit — the same file path, the same underlying file, growing or shrinking as
the writer goes. Tools that do this include shell redirection (`> config.yaml`),
`sed -i`, most editors' default save, and any code that opens the path for writing
and writes in chunks.

Writing a file is not instant. The writer puts down the first part of the new
content, then the next part, and so on. Between those steps the file on disk holds
a mix: some new content at the front, and either leftover old content or nothing
after it. A file caught in that state is a **partial write** — a valid-looking
front section followed by whatever had not been written yet.

If the watcher's read lands in that window, it reads the partial file. The watcher
has no way to know the writer was not done.

## How a partial file can look like a valid empty config

gobridge requires exactly one field for a config to be valid: `bridge.id`. It does
NOT require any routes, receivers, or senders. A config with a bridge id and nothing else is accepted.

Picture an external tool rewriting the file in place, top to bottom. Suppose the
full, intended file is:

```yaml
bridge:
  id: prod

sessions:
  - id: mqtt-conn
    transport: mqtt
    options:
      session:
        broker_url: tcp://mqtt.example.com:1883
        client_id: prod-ingest-01

receivers:
  - id: mqtt-in
    session_id: mqtt-conn
    topics:
      - topic: "sensors/#"
        qos: 1
senders:
  - id: sqs-out
    transport: sqs
    options:
      queue_url: https://sqs.us-west-1.amazonaws.com/123456789/sensor-events
bindings:
  - id: to-sqs
    sender_id: sqs-out
    address: sensor-events
routes:
  - id: sensor-ingest
    receiver_id: mqtt-in
    bindings: [to-sqs]
```

If the watcher reads the file after the writer has put down only the first two
lines, the file on disk is:

```yaml
bridge:
  id: prod
```

That is a complete, syntactically valid YAML document. It parses. It has
`bridge.id`, so it passes validation. It has no `routes`, so it describes a bridge
that forwards nothing. gobridge treats it as a deliberate new config and swaps to
it.

## Why this silently breaks message forwarding

A bridge with zero routes has nothing to subscribe to and nothing to forward. The
receivers, senders, and bindings from the previous config are gone. Depending on
the transport, gobridge either tears down the source subscriptions or stops
dispatching what arrives. Messages the bridge would have forwarded are dropped or
never picked up.

Nothing looks wrong from the outside:

- The config was valid, so no parse or validation error is logged.
- The process is fine, so liveness and readiness probes stay green.
- Traffic stops flowing.

When the external tool finishes its write a fraction of a second later, the file
on disk is the full, correct config again. The bridge recovers on the next
filesystem event or the watcher's periodic content check — but only after a
routing gap, and the gap recurs on every deploy that writes in place.

## What an external writer must do instead

Write the new content to a temporary file in the **same directory** as the target,
then rename the temporary file over the target. A rename within one directory on
one filesystem is atomic: at every instant the target path points at either the
old file or the fully-written new file, never a mix. The watcher can only ever read
one of those two complete files.

Two rules make this work:

1. **Same directory, same filesystem.** Rename is only atomic within a single
   filesystem. A temp file in `/tmp` renamed onto `/etc/gobridge/config.yaml` is
   usually a cross-filesystem move, which copies then deletes — not atomic. Put the
   temp file next to the target.
2. **Write the whole file to the temp path, then rename.** Never touch the target
   path until the content is complete.

Shell example:

```bash
set -euo pipefail

target=/etc/gobridge/config.yaml
dir=$(dirname "$target")

# Temp file in the SAME directory as the target.
tmp=$(mktemp "$dir/.config.yaml.XXXXXX")

# Render or copy the full config into the temp file.
render_config > "$tmp"

# Optional: reject bad content before publishing.
# your_yaml_linter "$tmp"

# Atomic publish: the watcher sees the old file or the new file, never a mix.
mv -f "$tmp" "$target"
```

`mv` within one directory uses the `rename(2)` system call, which is the atomic
swap. The same pattern applies in any language: create a temp file beside the
target, write it in full, then rename it into place.

Templating and deploy tools:

- Kubernetes ConfigMap volume mounts already publish atomically (the projected
  `..data` symlink is swapped), so a ConfigMap-mounted config is safe.
- `envsubst`, `sed`, and shell redirection are NOT atomic. Redirect into a temp
  file and `mv` it into place, as above.
- Editors: use "save via rename" / atomic save, or edit a copy and `mv` it in. A
  plain in-place save is a partial-write risk.

## In-process mitigation: the content-stability gate

As defense in depth, the watcher does not apply a change the instant it first
reads new content. It requires the **same new bytes to be read twice across the
settle window** before swapping the runtime. The first read records the content
as a candidate; a confirming read one `config_watch.debounce` window later (a
dedicated confirm timer in `poll` mode) must return byte-identical content before
the file is parsed and applied. Content that is still changing between the two
reads — the normal signature of an in-progress write — is held back, not applied.

This closes the **common** torn-read window. An in-place write that completes
within one settle window is never observed as "settled" on a partial file,
because the confirming read sees either the finished file or still-changing
bytes. A larger `config_watch.debounce` widens this protection, at the cost of
proportionally slower reloads.

It does **not** replace atomic writes. A writer that stalls mid-write for longer
than the settle window — leaving a valid partial file untouched across both reads
— still looks "stable" and can be applied. Slow or chunked renderers, network
filesystems, and paused editors can all produce that state. Atomic
temp-file-plus-rename remains the required discipline for every external writer;
the stability gate only narrows the window, it does not remove the requirement.

## Why gobridge itself does not need this

gobridge's own config writer is already atomic. It writes the new config to a
temporary file in the target's directory, fsyncs it, then renames it over the
target and fsyncs the directory. So when gobridge
rewrites its own config — through the admin config transactions API, for example —
the watcher never sees a partial file. This runbook exists only because external
tools do not automatically follow the same discipline.

## Verification

- Confirm the deploy pipeline writes the config with temp-file-plus-rename, not
  in-place redirection or `sed -i`.
- After a deploy, check that route count and delivery metrics match the intended
  config. A `MessagesReceived` series that flatlines while upstream traffic
  continues is the signal that the bridge swapped to a route-less config.
- Related: [Config rollback](config-rollback.md) covers recovering a running
  bridge after a bad config swap.
