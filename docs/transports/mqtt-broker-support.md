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
| Version | 2.1.2, pinned by image digest |
| Protocol | MQTT v5 and v3.1.1, on the same listener |
| Where it comes from | `testutil/mqttlocal`, started per test in Docker |

Mosquitto 2.1's WebSocket listener rejects an empty WebSocket frame and
disconnects the client as a malformed packet; GoBridge never sends one.
`TestMQTTWebsocketConn_EmptyWriteSendsNoFrame` checks the frames the session
puts on the wire. The WebSocket and Secure WebSocket rows below
(`TestIntegration_WebSocket_CarriesAuthenticatedTraffic`,
`TestIntegration_SecureWebSocket_ValidatesTheBrokerCertificate`) carry traffic
through this broker.

One product, one version, pinned. A floating tag would mean the evidence
described here silently became evidence about something else.

## Proved features

Every row names the test that fails if the behaviour regresses. All of them run
against that broker except the proxied rows, which are marked. Those run on
loopback, with a generated authority for TLS, because what they prove is the
route the client takes and the identity it validates on a socket it did not
dial itself.

| Feature | What is proved | Evidence |
|---|---|---|
| Plaintext MQTT (`tcp://`) | Connect, subscribe, publish, settle | `TestIntegration_SessionStartAndClose`, `TestIntegration_PubSubRoundTrip` |
| Direct TLS (`ssl://`) | Server certificate validated against a configured CA; traffic flows | `TestIntegration_DirectTLS_ValidatesTheBrokerAndCarriesTraffic` |
| TLS trust enforcement | A broker certificate no configured authority signed is refused | `TestIntegration_DirectTLS_RefusesAnUntrustedBrokerCertificate` |
| Mutual TLS | The session presents a client certificate; a listener that requires one refuses a session without it | `TestIntegration_MutualTLS_PresentsTheClientCertificate` |
| Proxied TLS *(loopback, not Mosquitto)* | A dial through a SOCKS5 proxy validates the broker identity derived from the broker URL, not from the socket | `TestDialMQTTTLS_ThroughProxyVerifiesBrokerIdentity` |
| Proxied WebSocket *(loopback, not Mosquitto)* | `ws://` and `wss://` reach the broker through the `ALL_PROXY` proxy and never around it; `wss://` validates the broker identity derived from the broker URL | `TestDialMQTTWebsocket_AllProxyCarriesTheDial`, `TestDialMQTTWebsocket_UnreachableProxyFailsClosed`, `TestDialMQTTWebsocket_SecureThroughProxyVerifiesBrokerIdentity` |
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

## Proved on MQTT 3.1.1

Every row runs against the same broker, with the bridge session set to
`protocol_version: v3.1.1` ([MQTT 3.1.1](mqtt-311.md)). A test that needs an
MQTT 5 publisher says so.

| Feature | What is proved | Evidence |
|---|---|---|
| Publish and subscribe | Connect, subscribe, publish and settle at QoS 0, 1 and 2; the delivered `mqtt.qos` is the published QoS | `TestIntegration_MQTT311_PubSubRoundTrip` |
| Durable session resumption | For QoS 1 and QoS 2, a message the broker queued while the session was offline survives a broker restart and is delivered when the session resumes with Clean Session 0; a reconnect of that session sees Session Present (no `MQTTSessionResumeLost`) | `TestIntegration_MQTT311_PersistentSessionRedeliversUnsettled` |
| No retained replay on a resumed session | A persistent session gets a retained message once when it subscribes. After its connection drops and resumes with Session Present 1, the reconcile sends no SUBSCRIBE: the retained message is not delivered again and a live publish still arrives. When the broker has lost the session (Session Present 0, `MQTTSessionResumeLost`), the session subscribes again and receives the retained message | `TestIntegration_MQTT311_ResumedSessionDoesNotReplayRetained` |
| Oversized publish | On a persistent session, a payload above `max_payload_bytes` is acked and dropped (`MQTTIngressPoisonDropped`) with no ingress reject, later traffic flows, and a fresh session with the same client ID never receives it again | `TestIntegration_MQTT311_OversizedPublishIsAckedAndDropped` |
| Username/password | A wrong password (CONNACK return code 4 or 5) surfaces as a classified `ErrNotAuthorized` | `TestIntegration_MQTT311_CredentialFailureSurfacesNotAuthorized` |
| Refused subscription | A filter the broker's ACL denies (SUBACK `0x80`) fails the reconcile; a permitted filter on the same session still delivers | `TestIntegration_MQTT311_RefusedSubscriptionFailsReconcile` |
| Last Will | Published when the connection dies ungracefully, and **not** published after a graceful DISCONNECT | `TestIntegration_MQTT311_LastWill` |
| Headers are not carried | A publish with a user property, a subject and an envelope ID from an MQTT 5 session arrives on a 3.1.1 session with only `mqtt.topic`, `mqtt.qos` and `mqtt.retained`, no subject, and a minted envelope ID (also in `mqtt.message-id`, marked `x-bridge.generated-id`) | `TestIntegration_MQTT311_HeadersAreNotCarried` |
| Unsubscribe | Removing a filter converges on the synthesized UNSUBACK, and the removed filter stops delivering | `TestIntegration_MQTT311_UnsubscribeConverges` |
| Shared subscriptions (`$share`) | Competing 3.1.1 consumers split a stream without duplication | `TestIntegration_MQTT311_SharedSubscription` |
| Broker window above `receive_maximum` | A broker in-flight limit (Mosquitto's default 20) above `receive_maximum` is refused as an ingress reject on every connection, the session redials instead of wedging and `Close` returns, and nothing is lost: with a larger `receive_maximum` every queued message arrives | `TestIntegration_MQTT311_BrokerWindowAboveReceiveMaximumIsRefusedNotWedged` |

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
| EMQX, HiveMQ, VerneMQ, NanoMQ, RabbitMQ's MQTT plugin | Untested, on either protocol version. MQTT v5 conformance differs between products, particularly around shared subscriptions, session expiry and server-side limits. |
| AWS IoT Core | Untested. It restricts MQTT v5 properties, caps QoS at 1, and imposes its own topic and throughput limits. |
| Broker-side high availability (clustered brokers, failover between broker nodes) | Untested. The multi-URL proof moves between *endpoints*, not between members of a broker cluster with shared session state. |
| MQTT 3.1 (protocol name `MQIsdp`, level 3) | Not supported. `protocol_version` accepts only `v5` and `v3.1.1`. |
| Broker-enforced authorization (ACLs on topics) | Only a refused SUBSCRIBE is proved (`TestIntegration_MQTT311_RefusedSubscriptionFailsReconcile`). Publish ACLs are untested, and which topics a broker permits is the broker's policy. |

If you need one of these supported, the shortest path is a fixture that starts
that broker and the same proofs pointed at it: the tests above are written
against the session API, not against Mosquitto.
