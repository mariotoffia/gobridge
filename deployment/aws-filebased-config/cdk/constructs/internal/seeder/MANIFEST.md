# Seeder Image Manifest

The default seeder is published at `docker.io/mariotoffia/gobridge-seeder`.
[`image.txt`](image.txt) pins its immutable multi-platform index digest.
Both `linux/amd64` and `linux/arm64` are included and can be pulled publicly.

[`Dockerfile`](Dockerfile) starts from a digest-pinned AWS CLI image and adds
PyYAML plus both seeder scripts. It runs the file and DynamoDB suites on each
platform before an image can be published. Tests use fixture AWS responses;
no deployment configuration, credentials or test fixtures are baked into the
image. The resulting image supports both file and DynamoDB config sources.

## Pin format

The manifest is one line:

```text
docker.io/mariotoffia/gobridge-seeder@sha256:<64-hex-index-digest>
```

The digest, not a mutable tag, identifies the image. The upstream AWS CLI
digest in `Dockerfile` is a separate pin: substituting it into `image.txt`
would remove PyYAML and break file seeding.

## Publication

The release workflow publishes the seeder after the final stable command tag
passes release validation and the external-consumer smoke. It uses the
`DOCKERHUB_USERNAME` repository variable and `DOCKERHUB_TOKEN` Actions secret
(a Docker Hub read/write access token, without delete permission).

The job builds both platforms, publishes by digest without a mutable tag,
verifies the registry index and saves `gobridge-seeder-image-digest.txt` in its
workflow artifact and summary. A rerun can produce another digest because
build attestations differ. It does not change the committed default or overwrite
an existing digest. Retain any digest still pinned by a deployed revision.

## Refresh

Copy the published reference from the workflow output:

```sh
SEEDER_IMAGE='docker.io/mariotoffia/gobridge-seeder@sha256:<digest>' \
  make -C deployment/aws-filebased-config update-seeder-image
```

The updater uses `crane` or `docker buildx`, verifies the exact manifest bytes
against the supplied digest, requires both supported platforms, and replaces
only `image.txt`. Missing or invalid references leave the pin unchanged.
Review and commit the resulting diff. It never installs tools or builds an image.
To refresh the upstream base, change `Dockerfile`, rebuild and publish the
seeder, then pin that new image; never pin the base as the runnable seeder.

## Override

`SeederImage` overrides the default with another digest-pinned reference,
including a private ECR mirror. The execution role must be able to pull it,
and the image must contain `aws`, `bash`, `python3` and PyYAML and support the
task's architecture. Public Docker Hub pulls are subject to Docker Hub limits;
use a mirror when production pull volume or private networking requires it.
