# IAM Least Privilege

The exact task-role and execution-role policies a GoBridge deployment needs,
why each statement is scoped the way it is, and the few places a wildcard is
unavoidable.

Part of the [AWS Deployment Overview](overview.md).

---

Follow the principle of least privilege when configuring IAM roles. The CDK
constructs create scoped policies automatically, but if you manage IAM
manually, use these as a reference.

## Task Role

The task role is assumed by the running container. It needs access to EFS,
SSM, any transport-specific services (e.g. SQS), and DynamoDB when you configure
DynamoDB lease/outbox/DLQ stores.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "EfsAccess",
      "Effect": "Allow",
      "Action": [
        "elasticfilesystem:ClientMount",
        "elasticfilesystem:ClientRead"
      ],
      "Resource": "arn:aws:elasticfilesystem:REGION:ACCOUNT:file-system/fs-XXXXXXXX",
      "Condition": {
        "StringEquals": {
          "elasticfilesystem:AccessPointArn":
            "arn:aws:elasticfilesystem:REGION:ACCOUNT:access-point/fsap-XXXXXXXX"
        }
      }
    },
    {
      "Sid": "SsmParameterAccess",
      "Effect": "Allow",
      "Action": "ssm:GetParameter",
      "Resource": [
        "arn:aws:ssm:REGION:ACCOUNT:parameter/gobridge/admin-key",
        "arn:aws:ssm:REGION:ACCOUNT:parameter/gobridge/monitor-key",
        "arn:aws:ssm:REGION:ACCOUNT:parameter/gobridge/rx-*",
        "arn:aws:ssm:REGION:ACCOUNT:parameter/gobridge/tx-*"
      ]
    },
    {
      "Sid": "SqsAccess",
      "Effect": "Allow",
      "Action": [
        "sqs:SendMessage",
        "sqs:ReceiveMessage",
        "sqs:DeleteMessage",
        "sqs:ChangeMessageVisibility",
        "sqs:GetQueueUrl",
        "sqs:GetQueueAttributes"
      ],
      "Resource": "arn:aws:sqs:REGION:ACCOUNT:my-queue-*"
    },
    {
      "Sid": "DynamoDbStoreAccess",
      "Effect": "Allow",
      "Action": [
        "dynamodb:GetItem",
        "dynamodb:PutItem",
        "dynamodb:UpdateItem",
        "dynamodb:DeleteItem",
        "dynamodb:Query",
        "dynamodb:Scan",
        "dynamodb:TransactWriteItems",
        "dynamodb:DescribeTable",
        "dynamodb:DescribeTimeToLive"
      ],
      "Resource": [
        "arn:aws:dynamodb:REGION:ACCOUNT:table/gobridge-*",
        "arn:aws:dynamodb:REGION:ACCOUNT:table/gobridge-*/index/*"
      ]
    }
  ]
}
```

The SQS statement is optional and should be scoped to the exact queue ARNs
your bridge routes reference. Omit it entirely if your deployment does not use
SQS transport.

`sqs:ChangeMessageVisibility` backs the receiver's `auto_extend` (visibility
renewal at one-third of the timeout); a missing grant surfaces as `NOT_AUTHORIZED`
only after the first extension attempt, not at startup. `sqs:GetQueueUrl` backs
queue-name resolution -- a receiver or sender configured with `queue_name`
(rather than a full `queue_url`) resolves the canonical URL at build time. The
adapter does not call `GetQueueAttributes`; the action is retained here as a
harmless allowance for operators who inspect queues out-of-band, and can be
dropped from a least-privilege policy.

**DynamoDB stores.** The `DynamoDbStoreAccess` statement is needed only when a
store role is configured with `type: dynamodb`. Scope `Resource` to your actual
table ARNs -- the default names are `gobridge-leases`, `gobridge-outbox`, and
`gobridge-dlq` -- and keep the `/index/*` entry, which the outbox and DLQ queries
need for their GSIs. Omit the statement entirely for memory/SQLite-only
deployments. The data-plane actions each role uses, if you split the statement
per table for tighter least privilege:

| Role | Runtime data-plane actions |
|------|----------------------------|
| Lease | `GetItem`, `PutItem`, `UpdateItem` |
| Outbox | `GetItem`, `PutItem`, `UpdateItem`, `Query`, `TransactWriteItems` |
| DLQ | `GetItem`, `PutItem`, `DeleteItem`, `Query`, `Scan` |

Each store also runs a boot-time schema **preflight** that adds control-plane
actions on top of the data-plane set above:

- Outbox, DLQ, and managed-subscriptions additionally call `dynamodb:DescribeTable`.
- Lease additionally calls `dynamodb:DescribeTable` **and**
  `dynamodb:DescribeTimeToLive` -- it enforces that DynamoDB TTL is **disabled**
  on the fencing table, which a reaper would otherwise use to delete lease rows
  and reset the fencing version.

Preflight posture is **fail-closed** and matters for how you grant these actions:

- A **confirmed schema mismatch** -- the table exists but has the wrong key
  schema or is missing a required GSI -- is **fatal at boot**. The store refuses
  to start against a mis-shaped table (the guard against a copy-pasted table name
  silently shredding messages).
- A `DescribeTable` call that **cannot verify** the table -- the permission is
  missing (`AccessDenied`), the control plane throttles it during a mass rollout,
  or the backend does not implement `DescribeTable` -- is **also fatal at boot**.
  An unreadable table is not proof the table is valid, and an unreadable +
  mis-shaped table is the exact silent-shredder scenario the preflight exists to
  catch (the first record per partition writes, the rest ack-and-drop as
  "duplicates"). The store refuses to start.
- On the lease role, an **observed enabled (or enabling) DynamoDB TTL** on the
  fencing table is **fatal at boot**. A `DescribeTimeToLive` call that **cannot
  verify** the TTL state (missing `dynamodb:DescribeTimeToLive`, a throttle, or a
  backend that does not implement it) is **fatal for the same reason**: it proves
  nothing about the TTL state, and a TTL-reaped fence row is a split-brain hazard.
- The generated CDK task-role policy grants `dynamodb:DescribeTable` on every
  configured/default store table and additionally grants
  `dynamodb:DescribeTimeToLive` on the exact lease table. Both are therefore
  **required** for boot under the default posture,
  and TTL must stay disabled on the lease table.

The advisory opt-outs are **Go-code-level factory options, not config keys.**
`WithSchemaPreflightAdvisory()` downgrades an unverifiable `DescribeTable` to a
loud WARN-and-continue; `WithTTLPreflightAdvisory()` does the same for the lease
TTL check (both an observed enabled TTL and an unverifiable `DescribeTimeToLive`).
Neither relaxes a **confirmed** schema mismatch, which stays fatal. Use them only
for a dev/emulator that cannot serve these control-plane calls.

The shipped `aws-filebased-config` deployment builds the factory as
`NewDynamoDBStoreFactory(client)` with no options and exposes **no**
`schema_preflight_advisory` or `ttl_preflight_advisory` config key, so opting into
advisory mode requires code-level wiring in a custom composition root. The
DynamoDB Local (`ddblocal`) test emulator implements both `DescribeTable` and
`DescribeTimeToLive`, so tests and local development against it boot cleanly under
the default fail-closed posture -- only an emulator or backend that lacks these
control-plane calls needs the advisory opt-outs.

Table creation and TTL setup (`dynamodb:CreateTable`, `dynamodb:UpdateTimeToLive`)
are a deploy-time concern; the CDK constructs provision tables out-of-band. Grant
those two actions only if you let the bridge self-provision through its
`EnsureTable` helper. See the
[DynamoDB Store](../processors-and-stores.md#dynamodb-store) reference for store
behavior and [Monitoring](monitoring.md#key-metrics) for the backlog and
store-health signals.

## Execution Role

The execution role is used by the ECS agent to pull images and write logs.
It does not need access to application-level resources.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "EcrPull",
      "Effect": "Allow",
      "Action": [
        "ecr:GetAuthorizationToken",
        "ecr:BatchGetImage",
        "ecr:GetDownloadUrlForLayer",
        "ecr:BatchCheckLayerAvailability"
      ],
      "Resource": "arn:aws:ecr:REGION:ACCOUNT:repository/gobridge"
    },
    {
      "Sid": "EcrAuth",
      "Effect": "Allow",
      "Action": "ecr:GetAuthorizationToken",
      "Resource": "*"
    },
    {
      "Sid": "CloudWatchLogs",
      "Effect": "Allow",
      "Action": [
        "logs:CreateLogStream",
        "logs:PutLogEvents"
      ],
      "Resource": "arn:aws:logs:REGION:ACCOUNT:log-group:/ecs/gobridge-*:*"
    }
  ]
}
```

Note that `ecr:GetAuthorizationToken` requires `Resource: "*"` because the
authorization token is account-scoped, not repository-scoped.

---
