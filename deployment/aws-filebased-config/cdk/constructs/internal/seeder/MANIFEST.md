# Seeder Image Manifest

The seeder init container runs the upstream `public.ecr.aws/aws-cli/aws-cli`
image directly. That image already ships everything the seeder needs:

- `aws` CLI v2 (S3 download, SigV4 IRSA support).
- `python3` with `PyYAML` (canonicalizer).
- Coreutils (`mktemp`, `mv`, `sha256sum`).

## Pin format

[`image.txt`](image.txt) is intentionally a **single-line, parseable manifest**
(not YAML/JSON) so the Go CDK construct (T10) can read it with a trivial
`os.ReadFile` + `strings.TrimSpace`. The line shape is:

```
public.ecr.aws/aws-cli/aws-cli:<tag>@sha256:<digest>
```

Both tag and digest are mandatory. The tag is informational/debuggable; the
digest is what the container runtime resolves.

## Override

The `Seeder` CDK construct accepts a `SeederImage: string` prop. When set, it
takes precedence over `image.txt`. Operators wanting to mirror the upstream
image to a private registry (e.g. for VPC-private pulls) MUST supply a
fully-qualified `repo:tag@sha256:digest` reference.

## Refresh

```
make -C deployment/aws-filebased-config update-seeder-image
```

Discovers the highest concrete `aws-cli:2.x.y` tag (the upstream image has no
floating `2` tag), resolves that tag's top-level multi-platform index digest via
`crane` (preferred) or `docker buildx imagetools inspect`, verifies the index
includes both `linux/amd64` and `linux/arm64`, and rewrites BOTH `image.txt` and
the seeder `Dockerfile` FROM to the pinned `repo:tag@sha256:digest`. Fails closed
on a missing tag, digest, or platform, and never installs a tool. Run periodically (e.g. weekly via CI
cron); commit the diff. Tested resolver versions: crane v0.21.7 or docker buildx
v0.34.1 (exact, not floors).
