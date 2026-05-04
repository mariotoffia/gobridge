#!/usr/bin/env bash
# Regression test for the .go-arch-lint.yml component map.
#
# This script asserts that every key adapter package falls into the
# component the architecture intends. If a future edit accidentally
# broadens a component's `in:` glob (for example by reintroducing a
# blanket `adapters/**` pattern), the affected packages will get
# absorbed into the wrong component and this test will fail.
#
# It runs `go-arch-lint mapping` and greps the grouped output for
# specific (component, package) pairs. The script intentionally lists
# only a few sentinel packages per component category — enough to
# catch broad mistakes without becoming a copy of the YAML itself.

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
expect adapter_store_native_sqliteoutbox  /adapters/native/store/sqliteoutbox
expect adapter_store_aws_dynamodboutbox   /adapters/aws/store/dynamodboutbox

# Store factory aggregators.
expect adapter_store_native_factory  /adapters/native/store
expect adapter_store_aws_factory     /adapters/aws/store

# Config source adapters — the only adapter category allowed to import
# the core config package.
expect adapter_config_native_file    /adapters/native/config/file
expect adapter_config_aws_dynamodb   /adapters/aws/config/dynamodb

# Credential, observability, cluster.
expect adapter_credentials_aws_ssm   /adapters/aws/credentials/ssm
expect adapter_metrics_otel          /adapters/otel/metrics
expect adapter_tracing_otel          /adapters/otel/tracing
expect adapter_cluster_aws           /adapters/aws/cluster/ecs

# Inner ring sanity.
expect domain  /domain
expect ports   /ports
expect config  /config

if [[ $fail -eq 0 ]]; then
    echo "Architecture mapping test passed: all sentinel packages map to expected components."
else
    echo
    echo "Architecture mapping test FAILED. Either .go-arch-lint.yml is out of date,"
    echo "or a package was renamed/moved. Update either this script or the YAML"
    echo "policy so the two stay in sync."
    exit 1
fi
