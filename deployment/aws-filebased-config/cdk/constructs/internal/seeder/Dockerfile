# syntax=docker/dockerfile:1.7
#
# The seeder reuses the upstream aws-cli image as-is (it already ships
# python3 + PyYAML + coreutils). This Dockerfile exists ONLY to layer
# the seeder shell script on top, keeping the base image immutable.
#
# The FROM line is rewritten by `make update-seeder-image`; treat
# image.txt as the source of truth and keep this in sync.
FROM public.ecr.aws/aws-cli/aws-cli:2.15.0@sha256:0000000000000000000000000000000000000000000000000000000000000000

COPY seeder.sh /usr/local/bin/seeder.sh
RUN chmod 0755 /usr/local/bin/seeder.sh

ENTRYPOINT ["/usr/local/bin/seeder.sh"]
