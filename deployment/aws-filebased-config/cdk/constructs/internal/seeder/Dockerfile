# syntax=docker/dockerfile:1.7
#
# The seeder runs on the upstream aws-cli image, which ships `aws`, python3
# and coreutils — but NOT PyYAML, which the canonicalizer needs. Without it
# seeder.sh exits 50 (`canonicalizer_missing`) and the main container never
# gets a config, so this image layers the missing package and the seeder shell
# script on top and leaves the base image otherwise untouched.
#
# This pins the upstream base. image.txt separately pins the published image
# built from this Dockerfile, including PyYAML and both seeder scripts.
FROM public.ecr.aws/aws-cli/aws-cli:2.35.15@sha256:177c3f33d8b4b3d531a857b54289e8b101790ea9016c78431c417d8e681e7b2e

# python3-pyyaml is the canonicalizer seeder.sh gates on. Verified in the same
# layer so a base image that ever stops providing python3 fails the build here
# rather than at deploy time.
USER root
RUN dnf install -y python3-pyyaml \
 && dnf clean all \
 && rm -rf /var/cache/dnf \
 && python3 -c 'import yaml'

COPY seeder.sh seeder-ddb.sh /usr/local/bin/
RUN chmod 0755 /usr/local/bin/seeder.sh /usr/local/bin/seeder-ddb.sh

# Exercise both seeding modes with the image's actual Python dependencies.
# Bind-mounted fixtures and tests do not become part of the published image.
RUN --mount=type=bind,source=.,target=/seeder,ro bash /seeder/tests/run.sh

ENTRYPOINT ["/usr/local/bin/seeder.sh"]
