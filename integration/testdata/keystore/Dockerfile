# The base image is pinned by digest. An unpinned base drifts this fixture's
# own dependency on a schedule nobody controls, so a residence result would stop
# describing the harness and start describing whatever the tag moved to.
FROM debian:trixie-slim@sha256:020c0d20b9880058cbe785a9db107156c3c75c2ac944a6aa7ab59f2add76a7bd

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
