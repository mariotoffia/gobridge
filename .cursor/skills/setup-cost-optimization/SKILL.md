---
name: setup-cost-optimization
description: "Step-by-step guide for implementing cost optimization strategies for serverless architectures including compute right-sizing, data transfer optimization, storage policies, and cost visibility."
---

# Skill: Setup Cost Optimization

This skill teaches you how to implement comprehensive cost optimization for serverless architectures following  patterns. You'll learn to audit costs, right-size compute functions, minimize data transfer, configure storage policies, and set up cost visibility with anomaly detection.

Cost optimization is not a one-time exercise. It's a continuous practice that must be built into your architecture from day one. Serverless "pay-per-use" pricing makes cost directly tied to code efficiency—inefficient code directly translates to higher bills.

Without proper cost governance, serverless costs can spiral quickly. A single misconfigured function with excessive memory or a database table without TTL can silently drain your budget.

## Prerequisites

- Cloud CLI configured with appropriate credentials
- Understanding of serverless compute, NoSQL databases, and object storage
- Existing microservices following  patterns
- Cost Explorer and monitoring service access
- IAM permissions for cost management APIs

## Overview

In this skill, you will:
1. Audit current costs to establish baselines
2. Right-size compute memory using power tuning
3. Optimize data transfer patterns
4. Configure storage policies (database TTL, object storage lifecycle)
5. Implement comprehensive cost tagging
6. Create cost dashboards and reports
7. Set up anomaly detection and alerts

## Step 1: Audit Current Costs

Before optimizing, you need visibility into current spending. This establishes baselines and identifies high-impact optimization targets.

### Cost Analyzer

```pseudocode
TYPE ServiceCost
    serviceName: String
    cost: Float64
    currency: String
    period: String
END TYPE

TYPE CostAnomaly
    resourceId: String
    expectedCost: Float64
    actualCost: Float64
    percentChange: Float64
    detectedAt: DateTime
END TYPE

INTERFACE CostExplorerClient
    METHOD GetCostAndUsage(ctx: Context, input: CostUsageInput) RETURNS Result<CostUsageOutput, Error>
END INTERFACE

TYPE CostAnalyzer
    client: CostExplorerClient

CONSTRUCTOR NewCostAnalyzer(config: CloudConfig) RETURNS CostAnalyzer
    RETURN CostAnalyzer{
        client: NewCostExplorerClient(config)
    }
END CONSTRUCTOR

METHOD CostAnalyzer.GetServiceCosts(ctx: Context, startDate: DateTime, endDate: DateTime) RETURNS Result<List<ServiceCost>, Error>
    input = CostUsageInput{
        timePeriod: DateInterval{
            start: startDate.Format("2006-01-02"),
            end: endDate.Format("2006-01-02")
        },
        granularity: "MONTHLY",
        metrics: ["BlendedCost"],
        groupBy: [GroupDefinition{type: "DIMENSION", key: "SERVICE"}]
    }

    result = this.client.GetCostAndUsage(ctx, input)
    IF result.IsError() THEN
        RETURN Error("failed to get cost data: " + result.Error().Message())
    END IF

    costs = NewList()
    FOR EACH group IN result.Value().ResultsByTime DO
        FOR EACH g IN group.Groups DO
            amount = ParseFloat(g.Metrics["BlendedCost"].Amount)
            currency = g.Metrics["BlendedCost"].Unit
            costs.Add(ServiceCost{
                serviceName: g.Keys[0],
                cost: amount,
                currency: currency,
                period: group.TimePeriod.Start
            })
        END FOR
    END FOR

    RETURN Ok(costs)
END METHOD

METHOD CostAnalyzer.DetectAnomalies(ctx: Context, threshold: Float64) RETURNS Result<List<CostAnomaly>, Error>
    // Get last 7 days vs previous 7 days
    now = CurrentTimeUTC()
    currentEnd = now
    currentStart = now.AddDays(-7)
    previousEnd = currentStart
    previousStart = previousEnd.AddDays(-7)

    currentResult = this.GetServiceCosts(ctx, currentStart, currentEnd)
    IF currentResult.IsError() THEN
        RETURN currentResult.Error()
    END IF

    previousResult = this.GetServiceCosts(ctx, previousStart, previousEnd)
    IF previousResult.IsError() THEN
        RETURN previousResult.Error()
    END IF

    // Build lookup for previous period
    prevCosts = NewMap()
    FOR EACH c IN previousResult.Value() DO
        prevCosts[c.serviceName] = c.cost
    END FOR

    anomalies = NewList()
    FOR EACH c IN currentResult.Value() DO
        prev = prevCosts[c.serviceName]
        IF prev > 0 THEN
            change = ((c.cost - prev) / prev) * 100
            IF change > threshold THEN
                anomalies.Add(CostAnomaly{
                    resourceId: c.serviceName,
                    expectedCost: prev,
                    actualCost: c.cost,
                    percentChange: change,
                    detectedAt: now
                })
            END IF
        END IF
    END FOR

    RETURN Ok(anomalies)
END METHOD
```

## Step 2: Right-Size Compute Memory

Serverless compute pricing is based on memory x duration. More memory means faster execution but higher per-ms cost.

### Compute Optimizer

```pseudocode
TYPE MemoryRecommendation
    functionName: String
    currentMemory: Int32
    recommendedMemory: Int32
    currentCostPerReq: Float64
    optimalCostPerReq: Float64
    savingsPercent: Float64
    reason: String
END TYPE

INTERFACE ComputeClient
    METHOD GetFunction(ctx: Context, functionName: String) RETURNS Result<FunctionConfig, Error>
    METHOD ListFunctions(ctx: Context, marker: String) RETURNS Result<FunctionListOutput, Error>
END INTERFACE

TYPE ComputeOptimizer
    computeClient: ComputeClient
    metricsClient: MetricsClient

CONSTRUCTOR NewComputeOptimizer(config: CloudConfig) RETURNS ComputeOptimizer
    RETURN ComputeOptimizer{
        computeClient: NewComputeClient(config),
        metricsClient: NewMetricsClient(config)
    }
END CONSTRUCTOR

METHOD ComputeOptimizer.AnalyzeFunction(ctx: Context, functionName: String) RETURNS Result<MemoryRecommendation, Error>
    configResult = this.computeClient.GetFunction(ctx, functionName)
    IF configResult.IsError() THEN
        RETURN Error("failed to get function config: " + configResult.Error().Message())
    END IF

    currentMemory = configResult.Value().MemorySize
    avgDuration = this.getAverageDuration(ctx, functionName)
    maxMemoryUsed = this.getMaxMemoryUsed(ctx, functionName)

    rec = MemoryRecommendation{
        functionName: functionName,
        currentMemory: currentMemory
    }

    memoryUtilization = Float64(maxMemoryUsed) / Float64(currentMemory) * 100

    IF memoryUtilization < 50 THEN
        rec.recommendedMemory = Int32(Ceil(Float64(maxMemoryUsed) * 1.5 / 64) * 64)
        rec.reason = Format("Memory utilization only %.1f%%, function is over-provisioned", memoryUtilization)
    ELSE IF memoryUtilization > 85 THEN
        rec.recommendedMemory = Int32(Ceil(Float64(currentMemory) * 1.5 / 64) * 64)
        rec.reason = Format("Memory utilization %.1f%%, increase for headroom", memoryUtilization)
    ELSE
        rec.recommendedMemory = currentMemory
        rec.reason = Format("Memory utilization %.1f%%, current setting is optimal", memoryUtilization)
    END IF

    // Calculate cost impact
    pricePerGBSecond = 0.0000166667
    currentGBSeconds = Float64(currentMemory) / 1024 * avgDuration / 1000
    optimalGBSeconds = Float64(rec.recommendedMemory) / 1024 * avgDuration / 1000

    rec.currentCostPerReq = currentGBSeconds * pricePerGBSecond
    rec.optimalCostPerReq = optimalGBSeconds * pricePerGBSecond
    rec.savingsPercent = (rec.currentCostPerReq - rec.optimalCostPerReq) / rec.currentCostPerReq * 100

    RETURN Ok(rec)
END METHOD
```

## Step 3: Optimize Data Transfer

Data transfer costs accumulate silently. Cross-zone traffic, NAT Gateway usage, and public internet egress all add up.

### Data Transfer Optimizer

```pseudocode
TYPE VPCEndpointRecommendation
    serviceName: String
    endpointType: String
    estimatedSavings: Float64
    endpointCost: Float64
    reason: String
END TYPE

TYPE DataTransferOptimizer
    networkClient: NetworkClient

METHOD DataTransferOptimizer.AnalyzeVPCEndpoints(ctx: Context, vpcId: String) RETURNS Result<List<VPCEndpointRecommendation>, Error>
    existingResult = this.networkClient.DescribeVpcEndpoints(ctx, vpcId)
    IF existingResult.IsError() THEN
        RETURN Error("failed to describe VPC endpoints: " + existingResult.Error().Message())
    END IF

    existingServices = NewMap()
    FOR EACH ep IN existingResult.Value().VpcEndpoints DO
        existingServices[ep.ServiceName] = TRUE
    END FOR

    recommendedServices = [
        {service: "nosql-database", endpointType: "Gateway", description: "Free gateway endpoint for NoSQL database"},
        {service: "object-storage", endpointType: "Gateway", description: "Free gateway endpoint for object storage"},
        {service: "message-queue", endpointType: "Interface", description: "Interface endpoint for message queue"}
    ]

    recommendations = NewList()
    FOR EACH svc IN recommendedServices DO
        IF NOT existingServices[svc.service] THEN
            rec = VPCEndpointRecommendation{
                serviceName: svc.service,
                endpointType: svc.endpointType,
                reason: svc.description
            }
            IF svc.endpointType == "Gateway" THEN
                rec.endpointCost = 0
                rec.estimatedSavings = 50
            ELSE
                rec.endpointCost = 7.20 * 24 * 30 / 1000
                rec.estimatedSavings = 20
            END IF
            recommendations.Add(rec)
        END IF
    END FOR

    RETURN Ok(recommendations)
END METHOD
```

## Step 4: Configure Storage Policies

Storage costs grow silently over time. Without lifecycle policies and TTL, old data accumulates indefinitely.

### Database TTL Configuration

```pseudocode
TYPE TTLRecommendation
    tableName: String
    hasTTL: Boolean
    ttlAttribute: String
    itemCount: Int64
    recommendation: String
END TYPE

TYPE DatabaseOptimizer
    client: DatabaseClient

METHOD DatabaseOptimizer.AnalyzeTTL(ctx: Context) RETURNS Result<List<TTLRecommendation>, Error>
    recommendations = NewList()
    tablesResult = this.client.ListTables(ctx)

    FOR EACH tableName IN tablesResult.Value().TableNames DO
        descResult = this.client.DescribeTable(ctx, tableName)
        ttlResult = this.client.DescribeTimeToLive(ctx, tableName)

        rec = TTLRecommendation{
            tableName: tableName,
            itemCount: descResult.Value().ItemCount
        }

        IF ttlResult.Value().Status == "ENABLED" THEN
            rec.hasTTL = TRUE
            rec.ttlAttribute = ttlResult.Value().AttributeName
            rec.recommendation = "TTL is configured correctly"
        ELSE
            rec.hasTTL = FALSE
            rec.recommendation = "Consider enabling TTL to automatically delete old items"
        END IF

        recommendations.Add(rec)
    END FOR

    RETURN Ok(recommendations)
END METHOD

METHOD DatabaseOptimizer.EnableTTL(ctx: Context, tableName: String, attributeName: String) RETURNS Result<Void, Error>
    result = this.client.UpdateTimeToLive(ctx, tableName, attributeName, TRUE)
    IF result.IsError() THEN
        RETURN Error("failed to enable TTL: " + result.Error().Message())
    END IF
    RETURN Ok(Void)
END METHOD
```

### Object Storage Lifecycle Policies

```pseudocode
TYPE LifecycleRule
    id: String
    status: String
    prefix: String
    transitions: List<Transition>
END TYPE

TYPE ObjectStorageOptimizer
    client: ObjectStorageClient

METHOD ObjectStorageOptimizer.CreateStandardLifecycleRules(ctx: Context, bucketName: String) RETURNS Result<Void, Error>
    rules = [
        LifecycleRule{
            id: "transition-to-ia-30-days",
            status: "Enabled",
            prefix: "",
            transitions: [Transition{days: 30, storageClass: "STANDARD_IA"}]
        },
        LifecycleRule{
            id: "transition-to-archive-90-days",
            status: "Enabled",
            prefix: "archive/",
            transitions: [Transition{days: 90, storageClass: "ARCHIVE"}]
        }
    ]

    result = this.client.PutBucketLifecycleConfiguration(ctx, bucketName, rules)
    IF result.IsError() THEN
        RETURN Error("failed to create lifecycle rules: " + result.Error().Message())
    END IF

    RETURN Ok(Void)
END METHOD
```

## Step 5: Implement Cost Tagging

Tags enable cost allocation and accountability.

### Tagging Standards

```pseudocode
TYPE StandardTags
    service: String
    environment: String
    team: String
    costCenter: String
    project: String
END TYPE

FUNCTION RequiredTags() RETURNS List<String>
    RETURN ["service", "environment", "team", "cost_center", "project"]
END FUNCTION

FUNCTION ValidateResourceTags(tags: Map<String, String>) RETURNS List<String>
    missing = NewList()
    FOR EACH required IN RequiredTags() DO
        IF NOT tags.ContainsKey(required) THEN
            missing.Add(required)
        END IF
    END FOR
    RETURN missing
END FUNCTION

FUNCTION ApplyStandardTags(existingTags: Map<String, String>, standard: StandardTags) RETURNS Map<String, String>
    existingTags["service"] = standard.service
    existingTags["environment"] = standard.environment
    existingTags["team"] = standard.team
    existingTags["cost_center"] = standard.costCenter
    existingTags["project"] = standard.project
    RETURN existingTags
END FUNCTION
```

## Step 6: Create Cost Dashboards

Visibility drives accountability. Create dashboards that show cost trends and anomalies.

### Cost Dashboard Builder

```pseudocode
TYPE CostDashboardBuilder
    client: DashboardClient

METHOD CostDashboardBuilder.CreateServiceCostDashboard(ctx: Context, serviceName: String) RETURNS Result<Void, Error>
    widgets = [
        DashboardWidget{
            type: "text",
            properties: {"markdown": Format("# %s Cost Dashboard", serviceName)}
        },
        DashboardWidget{
            type: "metric",
            properties: {
                "title": "Function Invocations",
                "metrics": [["Serverless/Compute", "Invocations", "FunctionName", serviceName + "-api"]],
                "period": 3600
            }
        },
        DashboardWidget{
            type: "metric",
            properties: {
                "title": "Database Consumed Capacity",
                "metrics": [
                    ["NoSQL/Database", "ConsumedReadCapacityUnits", "TableName", serviceName + "-table"],
                    ["NoSQL/Database", "ConsumedWriteCapacityUnits", "TableName", serviceName + "-table"]
                ],
                "period": 3600
            }
        }
    ]

    body = JsonSerialize({"widgets": widgets})
    RETURN this.client.PutDashboard(ctx, Format("%s-cost-dashboard", serviceName), body)
END METHOD
```

## Step 7: Implement Anomaly Alerts

Detect cost spikes before they become expensive surprises.

### Cost Anomaly Alerting

```pseudocode
TYPE CostAlertManager
    client: AlertClient

METHOD CostAlertManager.CreateComputeCostAlert(ctx: Context, functionName: String, notificationArn: String) RETURNS Result<Void, Error>
    result = this.client.PutMetricAlarm(ctx, MetricAlarmInput{
        alarmName: Format("%s-invocation-spike", functionName),
        alarmDescription: "Compute invocations significantly above normal - potential cost impact",
        metricName: "Invocations",
        namespace: "Serverless/Compute",
        statistic: "Sum",
        dimensions: [Dimension{name: "FunctionName", value: functionName}],
        period: 3600,
        evaluationPeriods: 1,
        threshold: 10000,
        comparisonOperator: "GreaterThanThreshold",
        alarmActions: [notificationArn]
    })
    RETURN result
END METHOD

METHOD CostAlertManager.CreateBudgetAlert(ctx: Context, budgetName: String, monthlyLimit: Float64, notificationArn: String) RETURNS Result<Void, Error>
    thresholds = [50.0, 80.0, 100.0]

    FOR EACH threshold IN thresholds DO
        result = this.client.PutBudgetAlert(ctx, BudgetAlertInput{
            budgetName: budgetName,
            limit: monthlyLimit,
            thresholdPercent: threshold,
            notificationArn: notificationArn
        })
        IF result.IsError() THEN
            RETURN result.Error()
        END IF
    END FOR

    RETURN Ok(Void)
END METHOD
```

## Verification Checklist

- [ ] Cost Explorer shows costs grouped by service tags
- [ ] All serverless functions analyzed for memory right-sizing
- [ ] VPC endpoints deployed for object storage and NoSQL database (free Gateway endpoints)
- [ ] Database tables have TTL enabled for temporal data
- [ ] Object storage buckets have lifecycle policies for automatic tiering
- [ ] All resources tagged with service, environment, team, cost_center
- [ ] Cost dashboards exist for each major service
- [ ] Anomaly alerts configured at 150% and 200% of baseline
- [ ] Weekly cost review meetings scheduled
- [ ] Untagged resource report runs daily
- [ ] Monthly cost optimization review documented
- [ ] Budget alerts set at 50%, 80%, and 100% thresholds
