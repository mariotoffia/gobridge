# MQTT broker support: what is proved, and against what

> Part of the [MQTT transport reference](mqtt.md).

This page is the boundary between what this bridge is *tested* against and what
it is merely *expected* to work with. Read it before assuming a broker, a
transport or a security mode is supported.

The rule it follows: a claim appears here only when a named test exercises it
against a real broker. Everything else is listed as unproved — which is not the
same as broken, and is not something to build a production plan on without
running your own proof.

## The broker under test

| | |
|---|---|
| Product | Eclipse Mosquitto |
| Version | 2.0.22, pinned by image digest |
| Protocol | MQTT v5 only |
| Where it comes from | `testutil/mqttlocal`, started per test in Docker |

One product, one version, pinned. A floating tag would mean the evidence
described here silently became evidence about something else.

## Proved features

Every row names the test that fails if the behaviour regresses. All of them run
against that broker except the proxied-TLS row, which is marked, and which runs
on loopback against a generated authority because what it proves is the identity
the client validates on a socket it did not dial itself.

| Feature | What is proved | Evidence |
|---|---|---|
| Plaintext MQTT (`tcp://`) | Connect, subscribe, publish, settle | `TestIntegration_SessionStartAndClose`, `TestIntegration_PubSubRoundTrip` |
| Direct TLS (`ssl://`) | Server certificate validated against a configured CA; traffic flows | `TestIntegration_DirectTLS_ValidatesTheBrokerAndCarriesTraffic` |
| TLS trust enforcement | A broker certificate no configured authority signed is refused | `TestIntegration_DirectTLS_RefusesAnUntrustedBrokerCertificate` |
| Mutual TLS | The session presents a client certificate; a listener that requires one refuses a session without it | `TestIntegration_MutualTLS_PresentsTheClientCertificate` |
| Proxied TLS *(loopback, not Mosquitto)* | A dial through a SOCKS5 proxy validates the broker identity derived from the broker URL, not from the socket | `TestDialMQTTTLS_ThroughProxyVerifiesBrokerIdentity` |
| Username/password | Correct credentials connect; a wrong one surfaces as a classified `ErrNotAuthorized` | `TestIntegration_CredentialFailure_SurfacesNotAuthorized` |
| Credential rotation | A live session refused for a stale secret reaches the broker after the rotated one is pushed | `TestIntegration_CredentialRotation_ConnectsWithTheRotatedSecret` |
| WebSocket (`ws://`) | Upgrade, authentication and message flow | `TestIntegration_WebSocket_CarriesAuthenticatedTraffic` |
| Secure WebSocket (`wss://`) | The same, with the server certificate validated | `TestIntegration_SecureWebSocket_ValidatesTheBrokerCertificate` |
| Shared subscriptions (`$share`) | Competing consumers split a stream without duplication | `TestIntegration_SharedSubscription_CompetingConsumers` |
| Multi-URL failover | An endpoint that stops carrying sessions is left for a healthy one, and traffic resumes | `TestIntegration_MultiURLFailover_MovesToTheHealthyEndpoint` |
| Last Will | Registered at CONNECT and published when the connection dies ungracefully | `TestIntegration_LastWill_IsPublishedOnUngracefulDeath` |
| Last Will suppression | A graceful DISCONNECT does **not** trigger the will | `TestIntegration_LastWill_IsSuppressedByAGracefulDisconnect` |
| Server inflight quota | A broker quota far below the bridge's own loses nothing | `TestIntegration_ServerLimit_LowInflightQuotaLosesNothing` |
| Server message-size limit | An oversized publish fails and says so, rather than vanishing | `TestIntegration_ServerLimit_OversizedPublishIsRejectedNotLost` |
| Durable session resumption | A persistent session resumes its subscriptions and unsettled deliveries across a restart | `TestUC51_PersistentSessionRecovery`, `TestUC77_QoS2UnderBrokerRestart` |

## Network fault profile

Injected with `testutil/netfault`, a TCP proxy between the session and the
broker. Each fault is bounded and reversible, and each proof requires recovery
inside the bound the session's own reconnect policy declares — not eventually.

| Fault | What the client sees | Bound proved | Evidence |
|---|---|---|---|
| Partition | Every connection dies; new ones are reset | Reconnect and flow within 30 s of healing | `TestIntegration_NetworkFault_PartitionRecoversWithinItsBound` |
| Half-open connection | Socket established, writable, nothing delivered | Keep-alive detects it and the session rebuilds within 30 s | `TestIntegration_NetworkFault_HalfOpenConnectionRecovers` |
| Latency | Everything arrives, late | No loss at 40 ms injected per hop | `TestIntegration_NetworkFault_LatencySpikeDoesNotLoseMessages` |
| Endpoint withdrawal | New connections reset, live ones keep working | Failover to the alternate URL | `TestIntegration_MultiURLFailover_MovesToTheHealthyEndpoint` |

Per-segment packet loss is deliberately **not** modelled. A TCP proxy that
dropped application bytes would corrupt the stream rather than lose a segment,
which is a failure no real network produces. Half-open is the honest
application-visible form of loss once retransmission has given up.

## Release evidence: the numbers behind the claims

These are what the release gate exercises. Each is a *bounded* figure, chosen
from something real rather than rounded up to sound impressive.

| Claim | Exercised | Where |
|---|---|---|
| Message conservation | Receiving session window (192) x 25 refills = 4,800 messages, through a broker restart mid-stream, on the published lease profile | `TestGAP_ReleaseVolumeConservation` |
| Failover objective, published profile | 45 s lease TTL, real SIGKILL of the owner process, ceiling 90 s to `ServiceLevelFull` (46 s observed — a SIGKILLed owner leaves no release behind, so the successor waits out the whole TTL) | `TestUC3PublishedProfileFailover` |
| Failover objective, compressed profile | 5 s lease TTL, ceiling 25 s — proves the mechanism, not the deployed profile | `TestUC3SeparateProcessFailover` |
| Soak | 60 minutes at 100 msgs/sec (`make test-soak`); the ordinary suite runs a 5-minute smoke profile | `TestUC68_Soak` |
| Mutation fuzzing | 5 minutes per target by default, raised with `make fuzz FUZZTIME=…` | `make fuzz` |

Running them: `make test-release-gate` for the release subset, `make test-soak`
for the published soak profile, `make fuzz` for mutation. All three are
developer-machine runs — see
[Deployment, long-running and shell test suites](../internals/testing-slow-suites.md).

## Not proved

Everything below may well work. None of it is tested here, so none of it is a
supported claim.

| | Status |
|---|---|
| EMQX, HiveMQ, VerneMQ, NanoMQ, RabbitMQ's MQTT plugin | Untested. MQTT v5 conformance differs between products, particularly around shared subscriptions, session expiry and server-side limits. |
| AWS IoT Core | Untested. It restricts MQTT v5 properties, caps QoS at 1, and imposes its own topic and throughput limits. |
| Broker-side high availability (clustered brokers, failover between broker nodes) | Untested. The multi-URL proof moves between *endpoints*, not between members of a broker cluster with shared session state. |
| MQTT v3.1.1 | Not supported. The adapter requires MQTT v5 properties for identity, expiry and flow control. |
| Broker-enforced authorization (ACLs on topics) | Untested. The bridge surfaces a refused subscription; which topics a broker permits is the broker's policy. |

If you need one of these supported, the shortest path is a fixture that starts
that broker and the same proofs pointed at it: the tests above are written
against the session API, not against Mosquitto.
