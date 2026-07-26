# GoBridge v0.3.0 — first release

This is the first release of GoBridge that you can actually install. Every piece
of it is published, versioned together, and consumable with `go get`.

---

## What GoBridge is, in plain English

Software systems constantly send each other small pieces of information — "a
sensor just read 21.4 °C", "an order was placed", "this door was unlocked".
These are called **messages**.

The problem is that systems send messages in completely different ways, rather
like different countries running incompatible postal services. A fleet of
sensors speaks one language. Amazon's cloud expects another. Microsoft's cloud
expects a third. An older system in your own building expects a fourth. None of
them understand each other.

**GoBridge is the post office between them.** You describe, in a configuration
file, "messages arriving *here* should be delivered *there*", and GoBridge
handles collection, translation, and delivery — instead of you writing and
maintaining custom glue code for every combination.

---

## What it can connect to

Each of these is a different way of moving messages around. GoBridge speaks all
of them, so it can translate between any two:

| In everyday terms | What the industry calls it |
|---|---|
| The lightweight language small devices use — sensors, meters, vehicles, building equipment. Designed for weak and intermittent connections. | **MQTT** |
| Amazon's cloud mailroom, where messages queue up until something collects them. | **AWS SQS** |
| Microsoft's equivalent cloud mailroom. | **Azure Service Bus** |
| The sorting office widely used inside companies, with rules for fanning one message out to many recipients. | **RabbitMQ / AMQP** |
| Ordinary web requests — the same mechanism your browser uses. | **HTTP** |

A practical example: temperature sensors that only know how to talk to devices
can end up feeding business software that only knows how to read from Amazon's
queues. Neither side has to change, or even know the other exists.

---

## Not losing messages

Moving messages is easy. *Not losing them when something breaks halfway* is the
hard part, and it is most of what GoBridge does.

You choose how careful to be:

- **Hand it over, then confirm.** Fast and simple. Fine when the occasional
  retry is acceptable.
- **Write it down first, then confirm, then deliver.** GoBridge records the
  message in durable storage *before* telling the sender "got it". If the power
  fails mid-delivery, the message is still written down and delivery resumes on
  restart. This is a postbox that survives a power cut.

Where GoBridge writes things down is your choice too: in memory (fast, for
development), in a local file, or in a cloud database (for real deployments).

### When a message simply cannot be delivered

Some messages are broken beyond saving — malformed, or permanently rejected by
the destination. Left alone, one poisonous message can jam the entire line
behind it.

GoBridge moves repeat offenders into a **dead-letter queue**: a clearly labelled
"problem pile", separate from the main flow. Nothing is silently discarded, the
queue keeps moving, and you can inspect the pile later and re-submit anything
that was merely unlucky.

---

## Doing things to messages on the way through

Messages can pass through a chain of steps in transit:

- **Filter** — drop messages you do not care about, so you never pay to move or
  store them.
- **Transform** — reshape a message so the receiving system understands it.
- **Circuit breaker** — when a destination has fallen over, stop hammering it
  and let it recover, instead of piling on. The same idea as a household fuse.
- **Tenant isolation** — if you serve several customers through one bridge, keep
  each customer's messages strictly separated.

---

## Running more than one copy (clustering)

For anything that matters you run several copies, so one crashing does not stop
the flow. That creates a new problem: if three copies all read the same source,
every message gets delivered three times.

GoBridge coordinates the copies so that **exactly one** is responsible for a
given stream of work. Think of a baton being passed between runners: only the
holder is working, and if the holder collapses, another picks the baton up
automatically and carries on. You get spare capacity without duplicate
deliveries.

It can also **change configuration across all the copies together**, in a
coordinated way, so they move to the new settings as a group rather than
drifting into disagreement with each other.

---

## Seeing what is going on

GoBridge reports what it is doing — how many messages moved, what is stuck, how
long things took — in the standard formats that monitoring tools already
understand. It also offers a small administrative web interface for inspecting a
running bridge, adding routes, and managing the dead-letter pile, without
restarting anything.

---

## Installing

**One version for everything.** GoBridge is published as 31 separate pieces so
you install only what you need — and every piece carries the *same* version
number. There is no compatibility table to check and no way to accidentally mix
mismatched parts.

```bash
# The core. No external dependencies at all.
go get github.com/mariotoffia/gobridge@v0.3.0

# Then only the connectors you actually use:
go get github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho@v0.3.0
go get github.com/mariotoffia/gobridge/adapters/aws/transport/sqs@v0.3.0
go get github.com/mariotoffia/gobridge/adapters/azure/transport/servicebus@v0.3.0
go get github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091@v0.3.0
```

If you only bridge sensor traffic into Amazon's queues, nothing related to
Microsoft or RabbitMQ comes along for the ride.

A runnable demo:

```bash
go install github.com/mariotoffia/gobridge/cmd/gobridge@v0.3.0
```

---

## Please read this before deploying

- **`cmd/gobridge` is a demo, not a production binary.** It deliberately
  includes only MQTT and local storage, and refuses to start if handed a
  configuration using anything else. It exists to show how the wiring works.
  For a real deployment, build your own composition root — see the
  [Deployment Guide](docs/deployment-guide.md) and the
  [AWS file-based profile](deployment/aws-filebased-config/README.md).
- **The published container image is a release candidate, not a production
  approval.** It is built, scanned, and recorded by digest, but the production
  sign-off described in [RELEASE.md](RELEASE.md) is a separate step that has not
  been performed.
- **This is a `v0.x` release.** The public API may still change between minor
  versions.
- **Two adapters ship without integration-test coverage in CI.** The AWS SSM
  credential adapter and the CloudWatch metrics adapter depend on LocalStack,
  which now requires a licence token that this project does not have configured.
  Their tests skip rather than run. Everything else — MQTT, SQS, Azure Service
  Bus, RabbitMQ, AMQP 1.0, HTTP, the stores, and the clustering failover proof —
  runs against real brokers and databases in CI on every change.

---

## Where to start

| If you want to… | Read |
|---|---|
| See it work, end to end | [Scenario 1: MQTT-to-MQTT Bridge](docs/scenarios/01-mqtt-to-mqtt.md) |
| See everything it can be configured to do | [Configuration Overview](docs/configuration-overview.md) |
| Understand how it is built | [ARCHITECTURE.md](ARCHITECTURE.md) |
| Teach it to speak something new | [PLUGIN.md](PLUGIN.md) |
