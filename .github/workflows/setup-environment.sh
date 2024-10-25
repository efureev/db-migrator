#!/bin/bash
set -exo pipefail

env

# configure skopeo.
install -d -m 755 /etc/containers/registries.conf.d
cat >/etc/containers/registries.conf.d/localhost-5000.conf <<'EOF'
[[registry]]
location = 'localhost:5000'
insecure = true
EOF

# configure.
systemctl stop docker
cat >/etc/docker/daemon.json <<'EOF'
{
    "experimental": false,
    "debug": false,
    "features": {
        "buildkit": true
    },
    "log-driver": "journald",
    "labels": [
        "os=linux"
    ],
    "hosts": [
        "unix://"
    ]
}
EOF

# start docker without any command line flags as its entirely configured from daemon.json.
install -d /etc/systemd/system/docker.service.d
cat >/etc/systemd/system/docker.service.d/override.conf <<'EOF'
[Service]
ExecStart=
ExecStart=/usr/bin/dockerd
EOF

# start.
systemctl daemon-reload
systemctl start docker

# show information.
docker info

# install dependencies.
apt-get update
apt-get install -y qemu-user-static httpie

docker login --username "$DOCKER_HUB_USER" --password-stdin <<EOF
$DOCKER_HUB_ACCESS_TOKEN
EOF
# start a local registry.
docker run -d --restart=unless-stopped --name registry -p 5000:5000 registry:2.8.3
docker exec registry registry --version