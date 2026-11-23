# One image for both containers of a dev pod:
#   work:       python /opt/devpod/toolserver.py   (user 1000, port 8080, no credential)
#   git-helper: python /opt/devpod/githelper.py    (user 0, port 8081, credential at /credential/token)
FROM debian:trixie-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends bash ca-certificates git nodejs procps python3 ripgrep \
    && rm -rf /var/lib/apt/lists/* \
    && ln -s /usr/bin/python3 /usr/local/bin/python

# User 1000 does the agent's work. User 2000 owns protected files, so user 1000 can read
# but not change them. Protected folders make /workspace owned by user 2000, so git must
# not insist on the owner of the repository.
RUN groupadd --gid 1000 dev && useradd --uid 1000 --gid 1000 --create-home --shell /bin/bash dev \
    && groupadd --gid 2000 guard && useradd --uid 2000 --gid 2000 --no-create-home --shell /usr/sbin/nologin guard \
    && install -d -o 1000 -g 1000 /workspace \
    && install -d -m 0700 /var/lib/githelper \
    && git config --system safe.directory '*' \
    && git config --system init.defaultBranch main \
    && git config --system advice.detachedHead false

COPY podhttp.py toolserver.py githelper.py /opt/devpod/

ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 HOME=/home/dev \
    DEVPOD_WORKSPACE=/workspace DEVPOD_CREDENTIAL_FILE=/credential/token \
    GITHELPER_STATE_DIR=/var/lib/githelper GITHELPER_HOME=/tmp/githelper-home
WORKDIR /workspace
USER 1000:1000
EXPOSE 8080 8081
CMD ["python", "/opt/devpod/toolserver.py"]
