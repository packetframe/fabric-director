package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-ping/ping"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"gopkg.in/yaml.v3"
)

var version = "dev"

var (
	localNode        = flag.Uint("node", 10, "Local node ID")
	netPrefix        = flag.String("prefix", "172.17.104.", "/24 network prefix without last octet")
	nodesFile        = flag.String("nodes", "nodes.yml", "Node definition file")
	localIP          = flag.String("ip", "192.0.2.100", "Local IP for GRE tunnels")
	pingInterval     = flag.Duration("ping", 1*time.Second, "Time between ICMP pings")
	latencyThreshold = flag.Duration("threshold", 100*time.Millisecond, "Latency threshold")
	down             = flag.Bool("down", false, "Teardown tunnels and exit")
	metricsListen    = flag.String("metrics-listen", ":9090", "Prometheus metrics listen address")
)

var (
	metricIsRerouting = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "fabricdirector_is_rerouting",
		Help: "Is this node rerouting?",
	})

	metricNodeLatency = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "fabricdirector_node_latency",
			Help: "Latency from node to node",
		},
		[]string{"src", "dst"},
	)
)

// Node represents an edge node
type Node struct {
	ID uint8  `yaml:"id"`
	IP string `yaml:"ip"`
}

// addGre adds a GRE tunnel
func addGre(name, local, remote, ip string) error {
	// Create GRE interface
	la := netlink.NewLinkAttrs()
	la.Name = name
	la.MTU = 1436 // 1500 - 20 byte TCP header - 20 byte IP header - 24 byte GRE header + IP header
	gre := &netlink.Gretun{
		Local:     net.ParseIP(local),
		Remote:    net.ParseIP(remote),
		LinkAttrs: la,
	}
	if err := netlink.LinkAdd(gre); err != nil {
		return fmt.Errorf("error adding GRE tunnel %s: %s", name, err)
	}

	// Add IP address to interface
	_, ipNet, err := net.ParseCIDR(ip)
	if err != nil {
		return fmt.Errorf("error parsing IP %s for GRE interface %s: %s", ip, name, err)
	}
	if err := netlink.AddrAdd(gre, &netlink.Addr{IPNet: ipNet}); err != nil {
		return fmt.Errorf("error adding IP %s to GRE interface %s: %s", ip, name, err)
	}
	if err := netlink.LinkSetUp(gre); err != nil {
		return fmt.Errorf("error bringing up GRE interface %s: %s", name, err)
	}
	return nil
}

// teardown deletes all GRE interfaces
func teardown() error {
	links, err := netlink.LinkList()
	if err != nil {
		return err
	}
	for _, iface := range links {
		if strings.HasPrefix(iface.Attrs().Name, "fd-") {
			log.Debugf("Deleting interface %s", iface.Attrs().Name)
			if err := netlink.LinkDel(iface); err != nil {
				return err
			}
		}
	}
	return nil
}

// icmpLatency uses ICMP pings to measure the latency of a remote host
func icmpLatency(src, dst string) (time.Duration, error) {
	pinger, err := ping.NewPinger(dst)
	if err != nil {
		return 0, err
	}
	pinger.Source = src
	pinger.Count = 3
	pinger.Timeout = 500 * time.Millisecond
	err = pinger.Run()
	if err != nil {
		return 0, err
	}
	return pinger.Statistics().AvgRtt, nil
}

// loadNodes loads the nodes from a YAML file
func loadNodes(filename string) (map[string]Node, error) {
	yamlBytes, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var nodes map[string]Node
	if err = yaml.Unmarshal(yamlBytes, &nodes); err != nil {
		return nil, err
	}

	return nodes, nil
}

func main() {
	flag.Parse()
	log.Infof("Starting fabricdirector %s prefix %s", version, *netPrefix)

	// Load nodes
	nodes, err := loadNodes(*nodesFile)
	if err != nil {
		log.Fatalf("Error loading nodes: %s", err)
	}
	log.Infof("Loaded %d nodes from %s", len(nodes), *nodesFile)

	if *down {
		log.Info("Teardown requested")
		if err := teardown(); err != nil {
			log.Errorf("Error tearing down interfaces: %s", err)
		}
		log.Info("Teardown complete")
		os.Exit(0)
	}

	// Create GRE tunnels
	var localNodeName string
	for name, node := range nodes {
		// Skip local node
		if uint(node.ID) == *localNode {
			localNodeName = name
			continue
		}

		log.Infof("Adding GRE tunnel to %s", name)
		if err := addGre("fd-"+name, *localIP, node.IP, fmt.Sprintf("%s%d/32", *netPrefix, node.ID)); err != nil {
			log.Warn(err)
		}
	}

	// Start Prometheus metrics server
	go func() {
		log.Infof("Starting Prometheus metrics server on %s", *metricsListen)
		http.Handle("/metrics", promhttp.Handler())
		log.Fatal(http.ListenAndServe(*metricsListen, nil))
	}()

	// Start ICMP pinger in a new ticker
	ticker := time.NewTicker(*pingInterval)
	for range ticker.C {
		for name, node := range nodes {
			// Ping node
			latency, err := icmpLatency(fmt.Sprintf("%s%d", *netPrefix, *localNode), node.IP)
			if err != nil {
				log.Warnf("Error pinging %s: %s", node.IP, err)
			}
			metricNodeLatency.With(prometheus.Labels{
				"src": localNodeName,
				"dst": name,
			}).Set(latency.Seconds())
		}
	}
}
