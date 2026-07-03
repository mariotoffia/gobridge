# syntax=docker/dockerfile:1.7
#
# The seeder reuses the upstream aws-cli image as-is (it already ships
# python3 + PyYAML + coreutils). This Dockerfile exists ONLY to layer
# the seeder shell script on top, keeping the base image immutable.
#
# The FROM line is rewritten by `make update-seeder-image`; treat
# image.txt as the source of truth and keep this in sync.
FROM public.ecr.aws/aws-cli/aws-cli:2.35.15@sha256:177c3f33d8b4b3d531a857b54289e8b101790ea9016c78431c417d8e681e7b2e

COPY seeder.sh /usr/local/bin/seeder.sh
RUN chmod 0755 /usr/local/bin/seeder.sh

ENTRYPOINT ["/usr/local/bin/seeder.sh"]
