#!/usr/bin/env bash
# Regression test for the .go-arch-lint.yml component map.
#
# This script asserts that every adapter package falls into the
# component the architecture intends. If a future edit accidentally
# broadens a component's `in:` glob (for example by reintroducing a
# blanket `adapters/**` pattern), the affected packages will get
# absorbed into the wrong component and this test will fail. A new
# adapter added without a typed config and a registration call also
# fails this regression because there is no component to receive it.
#
# It runs `go-arch-lint mapping` and greps the grouped output for
# specific (component, package) pairs. The script lists every adapter
# package, every processor role, every cross-cutting utility, and the
# inner-ring sentinels (domain bounded contexts, ports, config) so the
# YAML and the package layout cannot drift silently. If a component is
# added to .go-arch-lint.yml without a matching `expect` line here, the
# mapping regression will not catch a future broadening.

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

mapping="$(go-arch-lint mapping --project-path . --scheme grouped --output-color=false)"

fail=0
expect() {
    local component="$1"
    local pkg="$2"
    if ! awk -v c="${component}:" -v p="$pkg" '
        BEGIN { in_block = 0 }
        $0 ~ "^   " c "$" { in_block = 1; next }
        in_block && /^   [a-zA-Z]/ { in_block = 0 }
        in_block && index($0, p) > 0 { found = 1 }
        END { exit !found }
    ' <<< "$mapping"; then
        echo "FAIL: package '$pkg' is not mapped to component '$component'"
        fail=1
    fi
}

# Transport adapters: each must live in its own role-named component.
expect adapter_transport_mqtt_paho     /adapters/mqtt/transport/paho
expect adapter_transport_sqs           /adapters/aws/transport/sqs
expect adapter_transport_servicebus    /adapters/azure/transport/servicebus
expect adapter_transport_amqp091       /adapters/amqp/transport/amqp091
expect adapter_transport_amqp10        /adapters/amqp/transport/amqp10
expect adapter_transport_http          /adapters/http/transport

# Store implementation adapters.
expect adapter_store_native_memorylease   /adapters/native/store/memorylease
expect adapter_store_native_memoryoutbox  /adapters/native/store/memoryoutbox
expect adapter_store_native_memorydlq     /adapters/native/store/memorydlq
expect adapter_store_native_sqliteoutbox  /adapters/native/store/sqliteoutbox
expect adapter_store_native_sqlitedlq     /adapters/native/store/sqlitedlq
expect adapter_store_aws_dynamodblease    /adapters/aws/store/dynamodblease
expect adapter_store_aws_dynamodboutbox   /adapters/aws/store/dynamodboutbox
expect adapter_store_aws_dynamodbdlq      /adapters/aws/store/dynamodbdlq

# Store factory aggregators.
expect adapter_store_native_factory  /adapters/native/store
expect adapter_store_aws_factory     /adapters/aws/store

# Config source adapters — the only adapter category allowed to import
# the core config package.
expect adapter_config_native_file    /adapters/native/config/file
expect adapter_config_aws_dynamodb   /adapters/aws/config/dynamodb

# Credential, observability, cluster.
expect adapter_credentials_native_file /adapters/native/credentials/file
expect adapter_credentials_aws_ssm   /adapters/aws/credentials/ssm
expect adapter_metrics_cloudwatch    /adapters/aws/metrics/cloudwatch
expect adapter_metrics_otel          /adapters/otel/metrics
expect adapter_tracing_otel          /adapters/otel/tracing
expect adapter_cluster_native        /adapters/native/cluster
expect adapter_cluster_aws           /adapters/aws/cluster/ecs

# Inner ring sanity. The domain layer is decomposed by bounded context
# (FIX-004 Phase 5); each context has its own sentinel so the YAML
# cannot quietly re-absorb a sub-package into a catch-all.
expect domain_shared       /domain/shared
expect domain_messaging    /domain/messaging
expect domain_persistence  /domain/persistence
expect domain_routing      /domain/routing
expect domain_connectivity /domain/connectivity
expect domain_events       /domain/events
expect domain_clock        /domain/clock

# Layer 2 — ports, application services, shared kernel.
expect ports         /ports
expect config        /config
expect config_parser /config/parser
expect runtime       /runtime
expect runtime_dlq   /runtime/dlq
expect bridge        /bridge
expect validate      /validate
expect httpapi       /httpapi

# Cross-cutting utilities (stdlib-only inner ring).
expect logging         /logging
expect observability   /observability
expect circuitbreaker  /circuitbreaker

# In-process processor chain — one sentinel per role so siblings cannot
# silently re-merge under a future `processors` umbrella component.
expect processor_filter         /processors/filter
expect processor_tenant         /processors/tenant
expect processor_transform      /processors/transform
expect processor_circuitbreaker /processors/circuitbreaker

# Composition root.
expect cmd         /cmd
expect deployment  /deployment

if [[ $fail -eq 0 ]]; then
    echo "Architecture mapping test passed: all sentinel packages map to expected components."
else
    echo
    echo "Architecture mapping test FAILED. Either .go-arch-lint.yml is out of date,"
    echo "or a package was renamed/moved. Update either this script or the YAML"
    echo "policy so the two stay in sync."
    exit 1
fi
