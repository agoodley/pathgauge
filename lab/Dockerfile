# Tools for running the lab inside a privileged container (make lab-smoke).
FROM ubuntu:24.04
RUN apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
    iproute2 iputils-ping iperf3 python3 ethtool nftables kmod ca-certificates >/dev/null && rm -rf /var/lib/apt/lists/*
