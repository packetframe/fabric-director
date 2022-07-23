# Fabric Director

Packetframe's distributed layer 3 load balancer. 

Fabric director...

- Maintains a full mesh of GRE tunnels between nodes,
- Monitors latency and packet loss between nodes.
- Sends a healthcheck ICMP packet once a second to each node,
- Creates a list of candidate failover nodes (latency below a certain threshold),
- Exposes latency and candidate nodes as a Prometheus endpoint.
