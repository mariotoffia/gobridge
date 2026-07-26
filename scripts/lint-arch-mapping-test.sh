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
#
# In addition to the (component, package) MAPPING assertions, this
# script enforces NEGATIVE fixtures (M-24): a set of forbidden
# dependency EDGES (domain->ports, ports->HTTP/DB/SDK, adapter->sibling
# adapter, runtime-leaf->forbidden parent/sibling, adapter->config) that
# fail the test if they ever appear in production code -- a
# belt-and-suspenders mirror of the .go-arch-lint.yml `deps` and
# .golangci.yml `depguard` rules. See the "Negative fixtures" section
# at the bottom.

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
expect adapter_store_native_memoryrollout /adapters/native/store/memoryrollout
expect adapter_store_native_sqliteoutbox  /adapters/native/store/sqliteoutbox
expect adapter_store_native_sqlitedlq     /adapters/native/store/sqlitedlq
expect adapter_store_native_sqlitemanagedsubscriptions /adapters/native/store/sqlitemanagedsubscriptions
expect adapter_store_aws_dynamodblease    /adapters/aws/store/dynamodblease
expect adapter_store_aws_dynamodbrollout  /adapters/aws/store/dynamodbrollout
expect adapter_store_aws_dynamodboutbox   /adapters/aws/store/dynamodboutbox
expect adapter_store_aws_dynamodbdlq      /adapters/aws/store/dynamodbdlq
expect adapter_store_aws_dynamodbmanagedsubscriptions /adapters/aws/store/dynamodbmanagedsubscriptions

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
expect runtime             /runtime
expect runtime_dlq         /runtime/dlq
expect runtime_credentials /runtime/credentials
expect runtime_cluster     /runtime/cluster
expect runtime_session     /runtime/session
expect runtime_outbox      /runtime/outbox
expect runtime_route       /runtime/route
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

# ============================================================================
# Negative fixtures: forbidden dependency EDGES (M-24)
#
# The `expect` assertions above prove package -> component MAPPING. They
# do NOT prove that forbidden EDGES stay absent: a future edit could
# broaden a `mayDependOn` list in .go-arch-lint.yml (or a depguard rule
# in .golangci.yml) and the mapping would still look correct. The checks
# below are the belt-and-suspenders complement -- they fail if a core
# forbidden edge ever appears in PRODUCTION code (test files excluded,
# matching the arch-lint excludeFiles rule). They parse the actual
# import statements, so they hold even if both lint configs are wrong.
#
# list_imports parses import specs with an awk state machine (single-line
# `import "x"` and grouped `import ( … )` blocks, comment lines skipped),
# emitting "FILE<TAB>IMPORTPATH". Because it only reads real import
# specs, a path that merely appears inside a string literal or a comment
# (e.g. an OpenTelemetry tracer name) is never a false positive.
# (Uses `find`, not bash globstar, so it runs on the macOS system
# bash 3.2 the project targets.)
list_imports() {
    local f
    find "$@" -type f -name '*.go' ! -name '*_test.go' 2>/dev/null | while IFS= read -r f; do
        awk -v file="$f" '
            /^[[:space:]]*\/\// { next }
            /^[[:space:]]*import[[:space:]]*\(/ { blk = 1; next }
            blk && /^[[:space:]]*\)/ { blk = 0; next }
            {
                if ($0 ~ /^[[:space:]]*import[[:space:]]+/ && index($0, "\"") > 0) {
                    s = $0; sub(/^[^"]*"/, "", s); sub(/".*/, "", s); print file "\t" s; next
                }
                if (blk && index($0, "\"") > 0) {
                    s = $0; sub(/^[^"]*"/, "", s); sub(/".*/, "", s); print file "\t" s
                }
            }
        ' "$f"
    done
}

# deny_path <label> <edges> <path-ERE>: fail if any edge's import path
# matches <path-ERE>.
deny_path() {
    local label="$1" edges="$2" re="$3" hits
    hits="$(printf '%s\n' "$edges" | awk -F'\t' -v re="$re" 'NF==2 && $2 ~ re { print $1 "  ->  " $2 }' || true)"
    if [[ -n "$hits" ]]; then
        echo "FAIL: forbidden edge [${label}]:"
        printf '%s\n' "$hits" | sed 's/^/    /'
        fail=1
    fi
}

report_bad() {
    local label="$1" hits="$2"
    if [[ -n "$hits" ]]; then
        echo "FAIL: forbidden edge [${label}]:"
        printf '%s\n' "$hits" | sed 's/^/    /'
        fail=1
    fi
}

dom_edges="$(list_imports domain)"
ports_edges="$(list_imports ports)"
adapter_edges="$(list_imports adapters)"
leaf_edges="$(list_imports runtime/dlq runtime/cluster runtime/session runtime/outbox runtime/route runtime/credentials)"

# 1. Domain is the innermost ring: it must not import ports or any outer
#    application / composition package.
deny_path "domain -> ports"   "$dom_edges" '^github\.com/mariotoffia/gobridge/ports($|/)'
deny_path "domain -> config"  "$dom_edges" '^github\.com/mariotoffia/gobridge/config($|/)'
deny_path "domain -> runtime" "$dom_edges" '^github\.com/mariotoffia/gobridge/runtime($|/)'
deny_path "domain -> bridge"  "$dom_edges" '^github\.com/mariotoffia/gobridge/bridge($|/)'

# 2. Ports must not leak transport / storage technology: no stdlib HTTP
#    or SQL types and no vendor SDK imports (ports is stdlib + domain).
deny_path "ports -> net/http"     "$ports_edges" '^net/http$'
deny_path "ports -> database/sql" "$ports_edges" '^database/sql$'
deny_path "ports -> AWS SDK"      "$ports_edges" '^github\.com/aws/'
deny_path "ports -> Azure SDK"    "$ports_edges" '^github\.com/Azure/'
deny_path "ports -> yaml"         "$ports_edges" '^gopkg\.in/yaml'
deny_path "ports -> mapstructure" "$ports_edges" '^github\.com/go-viper/mapstructure'

# 3. Adapters must not import the config shared kernel -- except the
#    config-source adapters under adapters/*/config/*, whose reason to
#    exist is producing a *ports.BridgeConfig.
cfg_bad="$(printf '%s\n' "$adapter_edges" | awk -F'\t' '
    NF==2 && $2 ~ /^github\.com\/mariotoffia\/gobridge\/config($|\/)/ &&
    $1 !~ /^adapters\/[^\/]+\/config\// { print $1 "  ->  " $2 }' || true)"
report_bad "adapter -> config (non-config adapter)" "$cfg_bad"

# 4. Adapter -> sibling adapter. The ONLY permitted intra-`adapters/`
#    imports are the two store-factory aggregators importing their own
#    store-impl leaves (the documented aggregator -> implementation
#    inversion: adapters/<cloud>/store/<file>.go -> adapters/<cloud>/
#    store/<leaf>). Every other adapter-to-adapter import
#    (transport->transport, store-leaf->sibling-leaf, credential->
#    transport, cross-cloud, …) is a forbidden sibling edge.
adapter_bad="$(printf '%s\n' "$adapter_edges" | awk -F'\t' '
    NF==2 && $2 ~ /^github\.com\/mariotoffia\/gobridge\/adapters\// {
        f=$1; p=$2;
        if (f ~ /^adapters\/native\/store\/[^\/]+\.go$/ && p ~ /^github\.com\/mariotoffia\/gobridge\/adapters\/native\/store\//) next;
        if (f ~ /^adapters\/aws\/store\/[^\/]+\.go$/    && p ~ /^github\.com\/mariotoffia\/gobridge\/adapters\/aws\/store\//) next;
        print f "  ->  " p;
    }' || true)"
report_bad "adapter -> sibling adapter" "$adapter_bad"

# 5. Runtime leaves never import the parent `runtime` engine, and the
#    only permitted leaf -> leaf edges are runtime/outbox -> runtime/dlq
#    and runtime/route -> {runtime/dlq, runtime/outbox}. Any other
#    leaf-to-leaf or leaf-to-parent import is forbidden (the dependency
#    direction is parent -> leaf only).
leaf_bad="$(printf '%s\n' "$leaf_edges" | awk -F'\t' '
    NF!=2 { next }
    $2 == "github.com/mariotoffia/gobridge/runtime" { print $1 "  ->  " $2; next }
    $2 ~ /^github\.com\/mariotoffia\/gobridge\/runtime\// {
        f=$1; p=$2;
        if (f ~ /^runtime\/outbox\// && p == "github.com/mariotoffia/gobridge/runtime/dlq") next;
        if (f ~ /^runtime\/route\//  && (p == "github.com/mariotoffia/gobridge/runtime/dlq" || p == "github.com/mariotoffia/gobridge/runtime/outbox")) next;
        print f "  ->  " p;
    }' || true)"
report_bad "runtime leaf -> forbidden parent/sibling" "$leaf_bad"

if [[ $fail -eq 0 ]]; then
    echo "Architecture mapping test passed: all sentinel packages map to expected components."
else
    echo
    echo "Architecture mapping test FAILED. Either .go-arch-lint.yml is out of date,"
    echo "or a package was renamed/moved. Update either this script or the YAML"
    echo "policy so the two stay in sync."
    exit 1
fi
