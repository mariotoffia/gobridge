# registrychk

CI gate that enforces every AWS-deployable plugin kind registered into
the local `*ports.Registry` (built by calling each adapter's exported
`Register(reg *ports.Registry) error`) has matching CDK helpers in
`deployment/aws-filebased-config/cdk`.

For each canonical AWS-deployable kind (after collapsing aliases like
`aws.sqs` → `sqs` and `mqtt.paho` → `mqtt`) it checks:

- A bridgecfg builder symbol exists with prefix `With<Kind>*` under
  `deployment/aws-filebased-config/cdk/bridgecfg/`.
- A grants helper file exists at
  `deployment/aws-filebased-config/cdk/constructs/internal/grants/<kind>.go`
  when the kind has an IAM surface (transport-only kinds like `http`
  and in-process stores like `memory`/`sqlite` are exempt).

Pure non-AWS kinds (`azure.*`, `amqp.*`, bare `servicebus`, `amqp10`,
`amqp091`) are skipped — the CDK only deploys to AWS. Run with `-v`
to print skipped kinds.

## Usage

```sh
# From the repository root
go run ./scripts/registrychk            # exit 1 on missing coverage
go run ./scripts/registrychk -v         # also print skipped non-AWS kinds

# Or via the Makefile (used by CI)
make lint-registrychk
```

Flags:

- `-bridgecfg-dir <path>` — override the bridgecfg directory.
- `-grants-dir <path>` — override the grants directory.
- `-v` — verbose; print non-AWS kinds that were skipped.

## Adding a new AWS-deployable kind

1. Add the adapter's `Register(reg *ports.Registry) error` call to
   `buildRegisteredKinds` in `main.go` (and any alias kinds).
2. Add a bridgecfg builder under `cdk/bridgecfg/` exporting a
   `With<Kind>*` symbol.
3. If the kind needs IAM grants, add `cdk/constructs/internal/grants/<kind>.go`.
4. Extend `awsDeployableKinds` in `main.go` with the curated
   `builderPrefix` + `grantsFile` for the new kind.

The tool will fail the gate until all four are present.
