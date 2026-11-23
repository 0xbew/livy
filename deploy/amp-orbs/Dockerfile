# Path A: a self-hosted orb image with Livy baked in. The orb records into a folder inside
# itself (a local sink) and holds no credential; dispatch.sh copies the records out on pause
# and delete. Build with build.sh image, which supplies the livy binary and the profile.
ARG BASE=us-central1-docker.pkg.dev/amp-prod-0/amp-orb/amp-orb:latest
FROM ${BASE}
COPY --chmod=0755 livy /usr/local/bin/livy
COPY --chown=user:user profile.json /tmp/livy-profile.json
USER user
# ~/.config/amp is in the base image, so install writes the Amp plugin there. No schedule, OTEL
# receiver or transcript discovery: the orb ships only when dispatch.sh asks.
RUN livy install --config /tmp/livy-profile.json --no-schedule --no-discover --otel-to none \
    && rm /tmp/livy-profile.json && mkdir -m 0700 -p /home/user/.livy-outbox
