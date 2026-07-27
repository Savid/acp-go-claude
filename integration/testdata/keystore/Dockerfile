# The base image is pinned by digest. An unpinned base drifts this fixture's
# own dependency on a schedule nobody controls, so a residence result would stop
# describing the harness and start describing whatever the tag moved to.
FROM debian:trixie-slim@sha256:b6dd47db7d9e8e6a5c8e64ba22b1e0a2f9e3fd4c19b5c0be3ee2e6ba3ea6a12e

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates \
      dbus-x11 \
      gnome-keyring \
      libsecret-tools \
      procps \
 && rm -rf /var/lib/apt/lists/*

RUN useradd --create-home --shell /bin/bash canary

# start-keyring.sh brings up a session bus and unlocks a keyring collection. The
# unlock password is newline-terminated: fed a bare end of input the daemon
# starts and claims the secret-service bus name while never creating its
# collection, so a half-initialised service looks alive and serves nothing.
COPY start-keyring.sh /usr/local/bin/start-keyring.sh
RUN chmod 0755 /usr/local/bin/start-keyring.sh

USER canary
WORKDIR /home/canary

CMD ["/usr/local/bin/start-keyring.sh"]
