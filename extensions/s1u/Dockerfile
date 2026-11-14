# The image of the Addon s1u-robot (apps/rlark/pkg/addons/catalog/s1u-robot): the node tuner, the Orin's keeper and
# the clock of every S1U robot x86. Python's standard library and ssh only; what runs on the host is the host's
# (node_profile.sh through nsenter).
#   docker build -f extensions/s1u/Dockerfile -t <registry>/rlark/s1u-robot:<tag> .
FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
RUN apt-get update && apt-get install -y --no-install-recommends python3 openssh-client util-linux \
    && rm -rf /var/lib/apt/lists/*
COPY extensions/s1u/tuner /s1u/tuner
COPY extensions/s1u/orin /s1u/orin
COPY extensions/s1u/clock /s1u/clock
COPY extensions/s1u/device /s1u/device
