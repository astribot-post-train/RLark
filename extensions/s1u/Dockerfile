# The image of the Addon s1u-robot (apps/rlark/pkg/addons/catalog/s1u-robot): the node tuner, the Orin's keeper, the
# robot's power and video, and the clock of every S1U robot x86. ROS noetic, as the robot's vendor software runs it,
# for the power and the cameras; what runs on the host is the host's (node_profile.sh through nsenter).
#   docker build -f extensions/s1u/Dockerfile -t <registry>/rlark/s1u-robot:<tag> .
FROM ros:noetic-ros-core@sha256:fd6279f04e51fa61201a791da9a156129941887b95b614ae0cec7865ef49de4f
RUN apt-get update && apt-get install -y --no-install-recommends ros-noetic-cv-bridge python3-opencv python3-pil ffmpeg \
      openssh-client util-linux curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
# MediaMTX, the WebRTC server the browser watches the cameras through: one pinned, verified release.
ARG MEDIAMTX=1.21.3
ARG MEDIAMTX_SHA256=0a2ea7eba96023631efa1bea7804823a9145b043b6646a8a05798d4ac24ed635
RUN curl -fsSL -o /tmp/mediamtx.tar.gz https://github.com/bluenviron/mediamtx/releases/download/v${MEDIAMTX}/mediamtx_v${MEDIAMTX}_linux_amd64.tar.gz \
    && echo "${MEDIAMTX_SHA256}  /tmp/mediamtx.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/mediamtx.tar.gz -C /usr/local/bin mediamtx && rm /tmp/mediamtx.tar.gz
COPY extensions/s1u/tuner /s1u/tuner
COPY extensions/s1u/orin /s1u/orin
COPY extensions/s1u/clock /s1u/clock
COPY extensions/s1u/device /s1u/device
ENTRYPOINT []
