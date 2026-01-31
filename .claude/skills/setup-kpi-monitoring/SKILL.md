---
name: setup-kpi-monitoring
description: "Step-by-step guide for implementing business-aligned KPI monitoring with actionable thresholds, ownership, and dashboards using cloud-agnostic patterns."
tools: Read, Grep, Glob, Write, Edit, grafana
context: kpi, metrics, monitoring, business-metrics, dashboards, alerting
---

# Skill: Setup KPI Monitoring

This skill teaches you how to implement KPI (Key Performance Indicator) monitoring for your  services. You'll define business-aligned KPIs, implement collectors, create dashboards, set up alerting, and establish ownership for each metric.

KPIs bridge technical metrics and business outcomes. Observability tells you what broke; KPIs tell you if you're succeeding. Without KPIs, you have data but no insight. You can tell the system is up but not whether it's delivering value.

The difference between a metric and a KPI: metrics are what you measure, KPIs are what you manage. A KPI has an owner, a threshold, and a response plan. If a KPI drops and nobody investigates, it's just a vanity metric.

## Prerequisites

- Grafana instance accessible (self-hosted or Grafana Cloud)
- Existing microservice with observability setup (see setup-observability skill)
- Prometheus or CloudWatch as metrics backend
- Understanding of your bounded context's business goals

## Overview

In this skill, you will:
1. Define a KPI catalog aligned to business goals
2. Implement KPI collectors in your services
3. Create Grafana dashboards for KPI visualization
4. Set up alerting with actionable thresholds
5. Assign ownership and response plans
6. Create KPI reporting for stakeholders

## MCP Tools Available

When working with KPI monitoring, you can use:
- **grafana**: Create and manage dashboards, configure alerts, query metrics

## Step 1: Define KPI Catalog

A KPI catalog documents what you measure, why it matters, and who owns it. Start with 5-10 KPIs that matter most—if everything is a KPI, nothing is.

### KPI Definition Structure

```pseudocode
TYPE Category
    RELIABILITY = "reliability"
    PERFORMANCE = "performance"
    BUSINESS = "business"
    OPERATIONAL = "operational"
END TYPE

TYPE IndicatorType
    LEADING = "leading"
    LAGGING = "lagging"
END TYPE

TYPE Threshold
    target: Float64       // Goal value (e.g., 99.0 for 99% success rate)
    warning: Float64      // Warning level (page during business hours)
    critical: Float64     // Critical level (page immediately)
    comparison: String    // "gte" (>= target is good) or "lte" (<= target is good)
END TYPE

TYPE KPIDefinition
    id: String                    // Unique identifier (e.g., "optimization_success_rate")
    name: String                  // Human-readable name for dashboards
    description: String           // What this KPI measures and why it matters
    category: Category            // Groups this KPI with related metrics
    indicatorType: IndicatorType  // Leading (predicts) or lagging (reports)
    unit: String                  // How value is measured (percent, milliseconds, count, kWh)
    threshold: Threshold          // Target value (SLO-style)
    owner: String                 // Team responsible for this KPI
    responsePlan: String          // What to do when threshold is breached
    boundedContext: String        // Links to a specific domain context
    leadingIndicator: String (optional)  // References a leading indicator that predicts this KPI
END TYPE

TYPE KPICatalog
    version: String
    updatedAt: DateTime
    definitions: List<KPIDefinition>
END TYPE
```

###  KPI Catalog Example

```pseudocode
CONSTRUCTOR NewCatalog() RETURNS KPICatalog
    RETURN KPICatalog{
        version: "1.0.0",
        updatedAt: CurrentTimeUTC(),
        definitions: [
            // Reliability KPIs
            KPIDefinition{
                id: "optimization_success_rate",
                name: "Optimization Success Rate",
                description: "Percentage of optimization requests completed successfully. Failed optimizations lose revenue.",
                category: Category.RELIABILITY,
                indicatorType: IndicatorType.LAGGING,
                unit: "percent",
                threshold: Threshold{target: 99.0, warning: 98.0, critical: 95.0, comparison: "gte"},
                owner: "Braiin Context",
                responsePlan: "Page on-call, check Braiin service health and downstream dependencies",
                boundedContext: "braiin",
                leadingIndicator: "optimization_queue_depth"
            },
            KPIDefinition{
                id: "event_processing_lag_p95",
                name: "Event Processing Lag (p95)",
                description: "95th percentile lag between event emission and processing. Stale data causes wrong decisions.",
                category: Category.RELIABILITY,
                indicatorType: IndicatorType.LEADING,
                unit: "milliseconds",
                threshold: Threshold{target: 500, warning: 1000, critical: 5000, comparison: "lte"},
                owner: "Platform Team",
                responsePlan: "Check event bus health, consumer lag, DLQ depth",
                boundedContext: "platform"
            },
            // Performance KPIs
            KPIDefinition{
                id: "schedule_calculation_latency_p95",
                name: "Schedule Calculation Latency (p95)",
                description: "Time to calculate an optimization schedule. Slow responses frustrate users.",
                category: Category.PERFORMANCE,
                indicatorType: IndicatorType.LAGGING,
                unit: "milliseconds",
                threshold: Threshold{target: 2000, warning: 3000, critical: 5000, comparison: "lte"},
                owner: "Braiin Context",
                responsePlan: "Profile slow queries, check algorithm efficiency, review cache hit rates",
                boundedContext: "braiin"
            },
            // Business KPIs
            KPIDefinition{
                id: "flexibility_capacity_committed_kwh",
                name: "Flexibility Capacity Committed (kWh)",
                description: "Total energy flexibility committed to grid. This is what we're selling.",
                category: Category.BUSINESS,
                indicatorType: IndicatorType.LAGGING,
                unit: "kWh",
                threshold: Threshold{target: 10000, warning: 8000, critical: 5000, comparison: "gte"},
                owner: "Grid Context",
                responsePlan: "Review facility enrollments, check optimization patterns, validate grid signals",
                boundedContext: "grid"
            },
            KPIDefinition{
                id: "peak_shaving_events_triggered",
                name: "Peak Shaving Events Triggered",
                description: "Number of peak shaving events successfully triggered. Proves the system delivers value.",
                category: Category.BUSINESS,
                indicatorType: IndicatorType.LAGGING,
                unit: "count",
                threshold: Threshold{target: 100, warning: 50, critical: 20, comparison: "gte"},
                owner: "Braiin Context",
                responsePlan: "Review grid signal reception, check event triggering logic, validate facility responses",
                boundedContext: "braiin"
            },
            // Operational KPIs
            KPIDefinition{
                id: "dlq_depth_trend",
                name: "DLQ Depth Trend",
                description: "Number of messages in dead-letter queues. Growing DLQ means silent failures.",
                category: Category.OPERATIONAL,
                indicatorType: IndicatorType.LEADING,
                unit: "count",
                threshold: Threshold{target: 0, warning: 10, critical: 100, comparison: "lte"},
                owner: "Platform Team",
                responsePlan: "Investigate failed messages, check consumer health, review error patterns",
                boundedContext: "platform"
            },
            KPIDefinition{
                id: "acl_translation_error_rate",
                name: "ACL Translation Error Rate",
                description: "Rate of failures in Anti-Corruption Layer translations. Failing ACLs break data flow.",
                category: Category.OPERATIONAL,
                indicatorType: IndicatorType.LEADING,
                unit: "percent",
                threshold: Threshold{target: 0, warning: 1.0, critical: 5.0, comparison: "lte"},
                owner: "Integration Team",
                responsePlan: "Check external API changes, review schema compatibility, update ACL mappings",
                boundedContext: "integration"
            }
        ]
    }
END CONSTRUCTOR

METHOD KPICatalog.GetByID(id: String) RETURNS (KPIDefinition, Boolean)
    FOR EACH def IN this.definitions DO
        IF def.id == id THEN
            RETURN (def, TRUE)
        END IF
    END FOR
    RETURN (KPIDefinition{}, FALSE)
END METHOD

METHOD KPICatalog.GetByCategory(category: Category) RETURNS List<KPIDefinition>
    result = NewList()
    FOR EACH def IN this.definitions DO
        IF def.category == category THEN
            result.Add(def)
        END IF
    END FOR
    RETURN result
END METHOD

METHOD KPICatalog.GetLeadingIndicators() RETURNS List<KPIDefinition>
    result = NewList()
    FOR EACH def IN this.definitions DO
        IF def.indicatorType == IndicatorType.LEADING THEN
            result.Add(def)
        END IF
    END FOR
    RETURN result
END METHOD
```

Good KPIs are business-aligned, measurable with clear thresholds, actionable with owners, and include leading indicators that predict problems before customers notice.

## Step 2: Implement KPI Collectors

KPI collectors gather metrics from your services and publish them in a format dashboards can query.

### KPI Collector Interface

```pseudocode
TYPE KPIValue
    kpiId: String
    value: Float64
    timestamp: DateTime
    labels: Map<String, String> (optional)
    context: String

INTERFACE Collector
    METHOD Collect(ctx: Context) RETURNS Result<KPIValue, Error>
    METHOD KPIID() RETURNS String
    METHOD Interval() RETURNS Duration
END INTERFACE

INTERFACE Publisher
    METHOD Publish(ctx: Context, value: KPIValue) RETURNS Result<Void, Error>
    METHOD PublishBatch(ctx: Context, values: List<KPIValue>) RETURNS Result<Void, Error>
END INTERFACE

TYPE Registry
    collectors: Map<String, Collector>
    publisher: Publisher
    catalog: KPICatalog

CONSTRUCTOR NewRegistry(publisher: Publisher, catalog: KPICatalog) RETURNS Registry
    RETURN Registry{
        collectors: NewMap(),
        publisher: publisher,
        catalog: catalog
    }
END CONSTRUCTOR

METHOD Registry.Register(collector: Collector) RETURNS Result<Void, Error>
    kpiId = collector.KPIID()
    _, exists = this.catalog.GetByID(kpiId)
    IF NOT exists THEN
        RETURN Error("unknown KPI: " + kpiId)
    END IF
    this.collectors[kpiId] = collector
    RETURN Ok(Void)
END METHOD

METHOD Registry.CollectAll(ctx: Context) RETURNS Result<List<KPIValue>, Error>
    values = NewList()
    FOR EACH collector IN this.collectors.Values() DO
        result = collector.Collect(ctx)
        IF result.IsError() THEN
            // Log but continue collecting other KPIs
            CONTINUE
        END IF
        values.Add(result.Value())
    END FOR
    RETURN Ok(values)
END METHOD
```

### Prometheus Publisher

```pseudocode
TYPE PrometheusPublisher IMPLEMENTS Publisher
    gauges: Map<String, GaugeVec>
    mutex: Mutex

CONSTRUCTOR NewPrometheusPublisher() RETURNS PrometheusPublisher
    RETURN PrometheusPublisher{
        gauges: NewMap()
    }
END CONSTRUCTOR

METHOD PrometheusPublisher.RegisterKPI(def: KPIDefinition)
    this.mutex.Lock()
    DEFER this.mutex.Unlock()

    gauge = NewGaugeVec({
        namespace: "",
        subsystem: "kpi",
        name: def.id,
        help: def.description,
        labelNames: ["bounded_context", "category", "owner"]
    })
    RegisterMetric(gauge)
    this.gauges[def.id] = gauge
END METHOD

METHOD PrometheusPublisher.Publish(ctx: Context, value: KPIValue) RETURNS Result<Void, Error>
    this.mutex.RLock()
    gauge, exists = this.gauges[value.kpiId]
    this.mutex.RUnlock()

    IF NOT exists THEN
        RETURN Error("unknown KPI: " + value.kpiId)
    END IF

    gauge.WithLabels(
        value.context,
        value.labels["category"],
        value.labels["owner"]
    ).Set(value.value)

    RETURN Ok(Void)
END METHOD

METHOD PrometheusPublisher.PublishBatch(ctx: Context, values: List<KPIValue>) RETURNS Result<Void, Error>
    FOR EACH value IN values DO
        result = this.Publish(ctx, value)
        IF result.IsError() THEN
            RETURN result
        END IF
    END FOR
    RETURN Ok(Void)
END METHOD
```

### Example Collectors

```pseudocode
INTERFACE MetricsQueryClient
    METHOD QueryRate(ctx: Context, metric: String, labels: String, window: Duration) RETURNS Result<Float64, Error>
END INTERFACE

TYPE OptimizationSuccessCollector IMPLEMENTS Collector
    metricsClient: MetricsQueryClient

CONSTRUCTOR NewOptimizationSuccessCollector(client: MetricsQueryClient) RETURNS OptimizationSuccessCollector
    RETURN OptimizationSuccessCollector{metricsClient: client}
END CONSTRUCTOR

METHOD OptimizationSuccessCollector.KPIID() RETURNS String
    RETURN "optimization_success_rate"
END METHOD

METHOD OptimizationSuccessCollector.Interval() RETURNS Duration
    RETURN 1 * Minute
END METHOD

METHOD OptimizationSuccessCollector.Collect(ctx: Context) RETURNS Result<KPIValue, Error>
    // Query successful optimizations
    successResult = this.metricsClient.QueryRate(
        ctx,
        "optimization_completed_total",
        `result="success"`,
        5 * Minute
    )
    IF successResult.IsError() THEN
        RETURN successResult.Error()
    END IF
    successRate = successResult.Value()

    // Query total optimizations
    totalResult = this.metricsClient.QueryRate(
        ctx,
        "optimization_completed_total",
        "",
        5 * Minute
    )
    IF totalResult.IsError() THEN
        RETURN totalResult.Error()
    END IF
    totalRate = totalResult.Value()

    // Calculate success rate percentage
    successPct = 0.0
    IF totalRate > 0 THEN
        successPct = (successRate / totalRate) * 100
    END IF

    RETURN Ok(KPIValue{
        kpiId: this.KPIID(),
        value: successPct,
        timestamp: CurrentTimeUTC(),
        labels: {
            "category": Category.RELIABILITY,
            "owner": "Braiin Context"
        },
        context: "braiin"
    })
END METHOD

INTERFACE QueueClient
    METHOD GetQueueDepth(ctx: Context, queueUrl: String) RETURNS Result<Int64, Error>
END INTERFACE

TYPE DLQDepthCollector IMPLEMENTS Collector
    queueClient: QueueClient
    queueUrls: List<String>

CONSTRUCTOR NewDLQDepthCollector(client: QueueClient, queueUrls: List<String>) RETURNS DLQDepthCollector
    RETURN DLQDepthCollector{queueClient: client, queueUrls: queueUrls}
END CONSTRUCTOR

METHOD DLQDepthCollector.KPIID() RETURNS String
    RETURN "dlq_depth_trend"
END METHOD

METHOD DLQDepthCollector.Interval() RETURNS Duration
    RETURN 30 * Second
END METHOD

METHOD DLQDepthCollector.Collect(ctx: Context) RETURNS Result<KPIValue, Error>
    totalDepth = 0
    FOR EACH url IN this.queueUrls DO
        result = this.queueClient.GetQueueDepth(ctx, url)
        IF result.IsError() THEN
            CONTINUE  // Log and continue
        END IF
        totalDepth = totalDepth + result.Value()
    END FOR

    RETURN Ok(KPIValue{
        kpiId: this.KPIID(),
        value: Float64(totalDepth),
        timestamp: CurrentTimeUTC(),
        labels: {
            "category": Category.OPERATIONAL,
            "owner": "Platform Team"
        },
        context: "platform"
    })
END METHOD
```

Collectors translate raw metrics into business KPIs. Each collector knows how to calculate its specific KPI from the underlying data sources.

## Step 3: Create Grafana Dashboards

Grafana dashboards visualize KPIs with context, thresholds, and trends.

### Dashboard Structure

```pseudocode
TYPE GrafanaDashboard
    title: String
    uid: String
    tags: List<String>
    panels: List<Panel>
    refresh: String
    time: TimeRange
END TYPE

TYPE Panel
    id: Int
    title: String
    type: String
    gridPos: GridPos
    targets: List<Target>
    thresholds: List<Threshold> (optional)
END TYPE

TYPE GridPos
    h: Int
    w: Int
    x: Int
    y: Int
END TYPE

TYPE Target
    expr: String
    legend: String
END TYPE

TYPE TimeRange
    from: String
    to: String
END TYPE

TYPE DashboardBuilder
    catalog: KPICatalog

CONSTRUCTOR NewDashboardBuilder(catalog: KPICatalog) RETURNS DashboardBuilder
    RETURN DashboardBuilder{catalog: catalog}
END CONSTRUCTOR

METHOD DashboardBuilder.BuildOverviewDashboard() RETURNS GrafanaDashboard
    dashboard = GrafanaDashboard{
        title: " KPI Overview",
        uid: "-kpi-overview",
        tags: ["kpi", "", "business"],
        refresh: "1m",
        time: TimeRange{from: "now-24h", to: "now"},
        panels: NewList()
    }

    panelId = 1
    yPos = 0

    // Add category sections
    FOR EACH category IN [Category.RELIABILITY, Category.PERFORMANCE, Category.BUSINESS, Category.OPERATIONAL] DO
        kpis = this.catalog.GetByCategory(category)
        IF kpis.IsEmpty() THEN
            CONTINUE
        END IF

        // Category header
        dashboard.panels.Add(Panel{
            id: panelId,
            title: category + " KPIs",
            type: "text",
            gridPos: GridPos{h: 2, w: 24, x: 0, y: yPos}
        })
        panelId = panelId + 1
        yPos = yPos + 2

        // KPI panels (4 per row)
        xPos = 0
        FOR EACH def IN kpis DO
            panel = this.buildKPIPanel(panelId, def, xPos, yPos)
            dashboard.panels.Add(panel)
            panelId = panelId + 1
            xPos = xPos + 6
            IF xPos >= 24 THEN
                xPos = 0
                yPos = yPos + 6
            END IF
        END FOR
        IF xPos > 0 THEN
            yPos = yPos + 6
        END IF
    END FOR

    RETURN dashboard
END METHOD

METHOD DashboardBuilder.buildKPIPanel(id: Int, def: KPIDefinition, x: Int, y: Int) RETURNS Panel
    // Build PromQL query for this KPI
    query = "_kpi_" + def.id + `{bounded_context="` + def.boundedContext + `"}`

    thresholds = [
        Threshold{color: "green", value: 0}
    ]

    // Add warning and critical thresholds
    thresholds.Add(Threshold{color: "yellow", value: def.threshold.warning})
    thresholds.Add(Threshold{color: "red", value: def.threshold.critical})

    RETURN Panel{
        id: id,
        title: def.name,
        type: "stat",
        gridPos: GridPos{h: 6, w: 6, x: x, y: y},
        targets: [
            Target{expr: query, legend: def.name}
        ],
        thresholds: thresholds
    }
END METHOD

METHOD GrafanaDashboard.ToJSON() RETURNS Result<String, Error>
    RETURN JsonSerialize(this)
END METHOD
```

The dashboard should show: current value as a large number, trend sparkline, threshold indicators, and owner/context labels.

## Step 4: Set Up Alerting

Alerts turn KPIs into actionable notifications. Every alert needs a threshold, an owner, and a response plan.

### Alert Configuration

```pseudocode
TYPE AlertSeverity
    WARNING = "warning"
    CRITICAL = "critical"
END TYPE

TYPE AlertConfig
    kpiId: String
    name: String
    severity: AlertSeverity
    threshold: Float64
    comparison: String
    evaluationPeriod: Duration
    evaluationCount: Int
    notificationGroup: String
    runbook: String
    owner: String
END TYPE

TYPE AlertManager
    catalog: KPICatalog

CONSTRUCTOR NewAlertManager(catalog: KPICatalog) RETURNS AlertManager
    RETURN AlertManager{catalog: catalog}
END CONSTRUCTOR

METHOD AlertManager.GenerateAlerts() RETURNS List<AlertConfig>
    alerts = NewList()

    FOR EACH def IN this.catalog.definitions DO
        // Warning alert
        alerts.Add(AlertConfig{
            kpiId: def.id,
            name: def.name + " - Warning",
            severity: AlertSeverity.WARNING,
            threshold: def.threshold.warning,
            comparison: def.threshold.comparison,
            evaluationPeriod: 5 * Minute,
            evaluationCount: 2,
            notificationGroup: def.owner + "-slack",
            runbook: "https://runbooks..io/kpi/" + def.id,
            owner: def.owner
        })

        // Critical alert
        alerts.Add(AlertConfig{
            kpiId: def.id,
            name: def.name + " - Critical",
            severity: AlertSeverity.CRITICAL,
            threshold: def.threshold.critical,
            comparison: def.threshold.comparison,
            evaluationPeriod: 5 * Minute,
            evaluationCount: 1,
            notificationGroup: def.owner + "-pagerduty",
            runbook: "https://runbooks..io/kpi/" + def.id,
            owner: def.owner
        })
    END FOR

    RETURN alerts
END METHOD

METHOD AlertConfig.ToGrafanaAlertRule() RETURNS Map<String, Any>
    condition = "lt"
    IF this.comparison == "gte" THEN
        condition = "gt"
    END IF

    RETURN {
        "alert": this.name,
        "for": this.evaluationPeriod.String(),
        "severity": this.severity,
        "annotations": {
            "summary": this.name + " threshold breached",
            "runbook_url": this.runbook,
            "owner": this.owner
        },
        "labels": {
            "kpi_id": this.kpiId,
            "severity": this.severity,
            "owner": this.owner
        },
        "conditions": [{
            "evaluator": {
                "type": condition,
                "params": [this.threshold]
            }
        }]
    }
END METHOD
```

Alerts must be actionable. If an alert fires and nobody knows what to do, it becomes noise. Every alert has a runbook.

## Step 5: Assign Ownership

Every KPI needs an owner who is accountable for its health.

### Ownership Registry

```pseudocode
TYPE Owner
    name: String
    slackChannel: String
    pagerDuty: String
    email: String
    onCallUrl: String
    kpis: List<String>
END TYPE

TYPE OwnershipRegistry
    owners: Map<String, Owner>

CONSTRUCTOR NewOwnershipRegistry() RETURNS OwnershipRegistry
    RETURN OwnershipRegistry{
        owners: {
            "Braiin Context": Owner{
                name: "Braiin Context",
                slackChannel: "#braiin-alerts",
                pagerDuty: "braiin-context-service",
                email: "braiin-team@.io",
                onCallUrl: "https://oncall..io/braiin",
                kpis: ["optimization_success_rate", "schedule_calculation_latency_p95", "peak_shaving_events_triggered"]
            },
            "Platform Team": Owner{
                name: "Platform Team",
                slackChannel: "#platform-alerts",
                pagerDuty: "platform-team-service",
                email: "platform-team@.io",
                onCallUrl: "https://oncall..io/platform",
                kpis: ["event_processing_lag_p95", "dlq_depth_trend"]
            },
            "Grid Context": Owner{
                name: "Grid Context",
                slackChannel: "#grid-alerts",
                pagerDuty: "grid-context-service",
                email: "grid-team@.io",
                onCallUrl: "https://oncall..io/grid",
                kpis: ["flexibility_capacity_committed_kwh"]
            },
            "Integration Team": Owner{
                name: "Integration Team",
                slackChannel: "#integration-alerts",
                pagerDuty: "integration-team-service",
                email: "integration-team@.io",
                onCallUrl: "https://oncall..io/integration",
                kpis: ["acl_translation_error_rate"]
            }
        }
    }
END CONSTRUCTOR

METHOD OwnershipRegistry.GetOwner(kpiId: String) RETURNS (Owner, Boolean)
    FOR EACH owner IN this.owners.Values() DO
        FOR EACH id IN owner.kpis DO
            IF id == kpiId THEN
                RETURN (owner, TRUE)
            END IF
        END FOR
    END FOR
    RETURN (Owner{}, FALSE)
END METHOD

METHOD OwnershipRegistry.ValidateOwnership(catalog: KPICatalog) RETURNS List<String>
    unowned = NewList()
    FOR EACH def IN catalog.definitions DO
        _, found = this.GetOwner(def.id)
        IF NOT found THEN
            unowned.Add(def.id)
        END IF
    END FOR
    RETURN unowned
END METHOD
```

A KPI without an owner is nobody's problem. Ownership must be explicit and include escalation paths.

## Step 6: Create KPI Reporting

Regular KPI reports communicate system health to stakeholders.

### Report Generator

```pseudocode
TYPE Report
    title: String
    period: String
    generatedAt: DateTime
    summary: Summary
    kpis: List<KPIStatus>
    incidents: List<Incident>
END TYPE

TYPE Summary
    totalKPIs: Int
    healthy: Int
    warning: Int
    critical: Int
    healthScore: Float64
END TYPE

TYPE KPIStatus
    definition: KPIDefinition
    current: Float64
    previous: Float64
    trend: String
    status: String
END TYPE

TYPE Incident
    kpiId: String
    severity: String
    startTime: DateTime
    endTime: DateTime
    duration: Duration
    owner: String
    resolution: String
END TYPE

INTERFACE MetricsClient
    METHOD QueryKPIValue(ctx: Context, kpiId: String, at: DateTime) RETURNS Result<Float64, Error>
    METHOD QueryKPIRange(ctx: Context, kpiId: String, from: DateTime, to: DateTime) RETURNS Result<List<Float64>, Error>
END INTERFACE

TYPE ReportGenerator
    catalog: KPICatalog
    metricsClient: MetricsClient
    reportTemplate: Template

CONSTRUCTOR NewReportGenerator(catalog: KPICatalog, client: MetricsClient) RETURNS Result<ReportGenerator, Error>
    template = ParseTemplate("report", reportTemplateString)
    IF template.IsError() THEN
        RETURN template.Error()
    END IF
    RETURN Ok(ReportGenerator{
        catalog: catalog,
        metricsClient: client,
        reportTemplate: template.Value()
    })
END CONSTRUCTOR

METHOD ReportGenerator.GenerateWeeklyReport(ctx: Context) RETURNS Result<Report, Error>
    now = CurrentTimeUTC()
    weekAgo = now.AddDays(-7)

    report = Report{
        title: "Weekly KPI Report",
        period: weekAgo.Format("2006-01-02") + " to " + now.Format("2006-01-02"),
        generatedAt: now,
        kpis: NewList()
    }

    healthy = 0
    warning = 0
    critical = 0

    FOR EACH def IN this.catalog.definitions DO
        currentResult = this.metricsClient.QueryKPIValue(ctx, def.id, now)
        IF currentResult.IsError() THEN
            CONTINUE
        END IF
        current = currentResult.Value()

        previousResult = this.metricsClient.QueryKPIValue(ctx, def.id, weekAgo)
        previous = 0.0
        IF previousResult.IsOk() THEN
            previous = previousResult.Value()
        END IF

        status = this.evaluateStatus(def, current)
        trend = this.calculateTrend(current, previous)

        SWITCH status
            CASE "healthy":
                healthy = healthy + 1
            CASE "warning":
                warning = warning + 1
            CASE "critical":
                critical = critical + 1
        END SWITCH

        report.kpis.Add(KPIStatus{
            definition: def,
            current: current,
            previous: previous,
            trend: trend,
            status: status
        })
    END FOR

    total = this.catalog.definitions.Length()
    report.summary = Summary{
        totalKPIs: total,
        healthy: healthy,
        warning: warning,
        critical: critical,
        healthScore: (Float64(healthy) / Float64(total)) * 100
    }

    RETURN Ok(report)
END METHOD

METHOD ReportGenerator.evaluateStatus(def: KPIDefinition, value: Float64) RETURNS String
    IF def.threshold.comparison == "gte" THEN
        IF value >= def.threshold.target THEN
            RETURN "healthy"
        ELSE IF value >= def.threshold.warning THEN
            RETURN "warning"
        END IF
        RETURN "critical"
    END IF
    // lte comparison
    IF value <= def.threshold.target THEN
        RETURN "healthy"
    ELSE IF value <= def.threshold.warning THEN
        RETURN "warning"
    END IF
    RETURN "critical"
END METHOD

METHOD ReportGenerator.calculateTrend(current: Float64, previous: Float64) RETURNS String
    IF previous == 0 THEN
        RETURN "new"
    END IF
    change = ((current - previous) / previous) * 100
    IF change > 5 THEN
        RETURN "improving"
    ELSE IF change < -5 THEN
        RETURN "degrading"
    END IF
    RETURN "stable"
END METHOD

METHOD ReportGenerator.RenderMarkdown(report: Report) RETURNS Result<String, Error>
    RETURN this.reportTemplate.Execute(report)
END METHOD
```

Reports communicate KPI health to stakeholders. Automate weekly reports to ensure consistent visibility.

## Wiring It All Together

### Entry Point with KPI Collection

```pseudocode
GLOBAL registry: Registry

FUNCTION Initialize()
    catalog = NewCatalog()
    publisher = NewPrometheusPublisher()

    // Register KPIs with Prometheus
    FOR EACH def IN catalog.definitions DO
        publisher.RegisterKPI(def)
    END FOR

    registry = NewRegistry(publisher, catalog)

    // Register collectors
    metricsClient = NewMetricsClient()
    queueClient = NewQueueClient()

    registry.Register(NewOptimizationSuccessCollector(metricsClient))
    registry.Register(NewDLQDepthCollector(queueClient, [GetEnvironmentVariable("DLQ_URL")]))
END FUNCTION

FUNCTION Handler(ctx: Context) RETURNS Result<Void, Error>
    result = registry.CollectAll(ctx)
    IF result.IsError() THEN
        RETURN result
    END IF

    values = result.Value()
    Log("Collected " + values.Length() + " KPI values")
    RETURN Ok(Void)
END FUNCTION

FUNCTION Main()
    StartHandler(Handler)
END FUNCTION
```

## Verification Checklist

After setting up KPI monitoring, verify:

- [ ] KPI catalog documents all 5-10 critical KPIs
- [ ] Each KPI has business alignment (answers "are we delivering value?")
- [ ] Each KPI has a clear threshold (target, warning, critical)
- [ ] Each KPI has an owner with contact information
- [ ] Each KPI has a response plan documented
- [ ] Leading indicators are included (predict problems before customers notice)
- [ ] Collectors are implemented for each KPI
- [ ] Grafana dashboard shows all KPIs with thresholds
- [ ] Warning and critical alerts are configured
- [ ] Alert notifications route to correct owners
- [ ] Runbooks exist for each KPI alert
- [ ] Weekly reports are automated
- [ ] No vanity metrics (every KPI drives action when breached)
