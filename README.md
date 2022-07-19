# Fabric Director

Layer 3 load balancer

## Overview

Fabric director...

- creates a full mesh of GRE tunnels between nodes,
- sends a healthcheck ICMP packet once a second to each node,
- creates a list of candidate failover nodes (latency below a certain threshold),
- exposes latency and candidate nodes as a Prometheus endpoint.
