package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
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
	configFile = flag.String("c", "config.yml", "Configuration file")
	down       = flag.Bool("d", false, "Teardown tunnels and exit")
	verbose    = flag.Bool("v", false, "Verbose output")
)

var candidateNodes = map[string]Node{} // Node name to node

type Config struct {
	LocalID          uint8           `yaml:"local-id"`
	Prefix4          string          `yaml:"prefix4"`
	Prefix6          string          `yaml:"prefix6"`
	PingInterval     time.Duration   `yaml:"ping-interval"`
	LatencyThreshold time.Duration   `yaml:"latency-threshold"`
	LossThreshold    float64         `yaml:"loss-threshold"`
	Listen           string          `yaml:"listen"`
	Prefixes         []string        `yaml:"prefixes"`
	Nodes            map[string]Node `yaml:"nodes"`
}

var (
	metricIsRerouting = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "fabric_director_is_rerouting",
		Help: "Is this node rerouting?",
	})

	metricCandidateNodes = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "fabric_director_candidate_nodes",
		Help: "Number of candidate nodes",
	})

	metricNodeLatency = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "fabric_director_node_latency",
			Help: "Latency from node to node",
		},
		[]string{"src", "dst"},
	)
)

// Node represents an edge node
type Node struct {
	ID      uint8  `yaml:"id"`
	IP      string `yaml:"ip"`
	Latency time.Duration
}

// parseCIDR parses a CIDR string into an IPNet preserving the last octet
func parseCIDR(cidr string) (net.IPNet, error) {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return net.IPNet{}, err
	}
	full := net.IPNet{}
	full.IP = ip
	full.Mask = ipNet.Mask
	return full, nil
}

// internalIP returns the GRE internal IP of a node
func internalIP(prefix string, node, mask uint8) string {
	out := fmt.Sprintf("%s%d", prefix, node)
	if mask != 0 {
		out += fmt.Sprintf("/%d", mask)
	}
	return out
}

// addGRE adds a GRE tunnel and returns the interface index
func addGRE(name, local, remote, ip4, ip6 string) (int, error) {
	log.Debugf("Adding GRE tunnel %s from %s to %s and adding %s and %s", name, local, remote, ip4, ip6)

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
		return -1, fmt.Errorf("error adding GRE tunnel %s: %s", name, err)
	}

	// Add IP address to interface
	ipNet4, err := parseCIDR(ip4)
	if err != nil {
		return -1, fmt.Errorf("error parsing IPv4 %s for GRE interface %s: %s", ip4, name, err)
	}
	ipNet6, err := parseCIDR(ip6)
	if err != nil {
		return -1, fmt.Errorf("error parsing IPv6 %s for GRE interface %s: %s", ip6, name, err)
	}
	if err := netlink.AddrAdd(gre, &netlink.Addr{IPNet: &ipNet4}); err != nil {
		return -1, fmt.Errorf("error adding IPv4 %s to GRE interface %s: %s", ip4, name, err)
	}
	if err := netlink.AddrAdd(gre, &netlink.Addr{IPNet: &ipNet6}); err != nil {
		return -1, fmt.Errorf("error adding IPv6 %s to GRE interface %s: %s", ip6, name, err)
	}
	if err := netlink.LinkSetUp(gre); err != nil {
		return -1, fmt.Errorf("error bringing up GRE interface %s: %s", name, err)
	}
	return gre.Attrs().Index, nil
}

// addRoute adds a static route from a prefix to an interface
func addRoute(prefix, nexthop4, nexthop6 string) error {
	_, ipNet, err := net.ParseCIDR(prefix)
	if err != nil {
		return err
	}

	var nexthop string
	if ipNet.IP.To4() != nil {
		nexthop = nexthop4
	} else {
		nexthop = nexthop6
	}

	log.Debugf("Adding route %s via %s", prefix, nexthop)
	route := &netlink.Route{
		Dst:      ipNet,
		Gw:       net.ParseIP(nexthop),
		Priority: 1,
	}
	return netlink.RouteAdd(route)
}

// setPFNet controls the pf-net service state
func setPFNet(state bool) error {
	if state {
		return exec.Command("/opt/packetframe/net.sh").Run()
	} else {
		return netlink.LinkDel(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "local"}})
	}
}

// setReroute controls the rerouting state
func setReroute(reroute bool, prefixes []string, nexthop4, nexthop6 string) error {
	if reroute {
		metricIsRerouting.Set(1)
		if err := setPFNet(false); err != nil {
			return err
		}
		for _, prefix := range prefixes {
			if err := addRoute(prefix, nexthop4, nexthop6); err != nil {
				return err
			}
		}
	} else {
		for _, prefix := range prefixes {
			_, ipNet, err := net.ParseCIDR(prefix)
			if err != nil {
				return err
			}
			if err := netlink.RouteDel(&netlink.Route{Dst: ipNet, Scope: netlink.SCOPE_UNIVERSE}); err != nil {
				return err
			}
		}
		if err := setPFNet(true); err != nil {
			return err
		}
		metricIsRerouting.Set(0)
	}
	return nil
}

// closestNode returns the node with the lowest latency
func closestNode() (*Node, string) {
	var closest *Node
	var closestName string
	for name, node := range candidateNodes {
		if closest == nil || node.Latency < closest.Latency {
			closest = &node
			closestName = name
		}
	}
	return closest, closestName
}

// teardownGRE deletes all GRE interfaces
func teardownGRE() error {
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
func icmpLatency(src, dst string) (time.Duration, float64, error) {
	log.Debugf("Pinging %s from %s", dst, src)
	pinger, err := ping.NewPinger(dst)
	if err != nil {
		return 0, 0, err
	}
	pinger.Source = src
	pinger.Count = 3
	pinger.Timeout = 500 * time.Millisecond
	pinger.SetPrivileged(false)
	err = pinger.Run()
	if err != nil {
		return 0, 0, err
	}
	stats := pinger.Statistics()
	return stats.AvgRtt, stats.PacketLoss, nil
}

func main() {
	flag.Parse()
	if *verbose {
		log.SetLevel(log.DebugLevel)
	}
	log.Infof("Starting fabric-director %s", version)

	// Load configuration
	yamlBytes, err := os.ReadFile(*configFile)
	if err != nil {
		log.Fatal(err)
	}

	var config Config
	if err = yaml.Unmarshal(yamlBytes, &config); err != nil {
		log.Fatal(err)
	}

	log.Infof("Loaded %d nodes from %s", len(config.Nodes), *configFile)

	if err := teardownGRE(); err != nil {
		log.Errorf("Error tearing down interfaces: %s", err)
	}
	if *down {
		log.Info("Teardown complete")
		os.Exit(0)
	}

	// Find local node from nodes file
	var localNodeName, localNodeIP string
	for name, node := range config.Nodes {
		if node.ID == config.LocalID {
			localNodeName = name
			localNodeIP = node.IP
			log.Infof("Found local node %s (%s)", name, localNodeIP)
			break
		}
	}
	if localNodeIP == "" || localNodeName == "" {
		log.Fatalf("Could not find local node %d in %s", config.LocalID, *configFile)
	}

	// Create GRE tunnels
	for name, node := range config.Nodes {
		// Skip local node
		if node.ID == config.LocalID {
			continue
		}

		log.Infof("Adding GRE tunnel to %s", name)
		_, err := addGRE(
			"fd-"+name,
			localNodeIP,
			node.IP,
			internalIP(config.Prefix4, config.LocalID, 24),
			internalIP(config.Prefix6, config.LocalID, 112),
		)
		if err != nil {
			log.Warn(err)
		}
	}

	// Start API server
	go func() {
		log.Infof("Starting API on %s", config.Listen)

		http.HandleFunc("/reroute", func(w http.ResponseWriter, r *http.Request) {
			var node *Node
			to := r.URL.Query().Get("to")
			if to == "" {
				node, to = closestNode()
			} else {
				n := config.Nodes[to]
				node = &n
			}
			log.Debugf("Rerouting to %s %+v", to, node)
			if err := setReroute(
				true,
				config.Prefixes,
				internalIP(config.Prefix4, node.ID, 0),
				internalIP(config.Prefix6, node.ID, 0),
			); err != nil {
				_, _ = fmt.Fprintf(w, "Error rerouting to %s: %s\n", to, err)
				return
			}
			_, _ = fmt.Fprintf(w, "Rerouting to %s\n", to)
			return
		})

		http.HandleFunc("/noreroute", func(w http.ResponseWriter, r *http.Request) {
			if err := setReroute(false, config.Prefixes, "", ""); err != nil {
				_, _ = fmt.Fprintf(w, "Error disabling reroute: %s\n", err)
				return
			}
			_, _ = fmt.Fprintf(w, "Reroute disabled\n")
		})

		http.HandleFunc("/candidates", func(w http.ResponseWriter, r *http.Request) {
			for name, node := range candidateNodes {
				_, _ = fmt.Fprintf(w, "%s %+v\n", name, node)
			}
		})

		http.Handle("/metrics", promhttp.Handler())
		log.Fatal(http.ListenAndServe(config.Listen, nil))
	}()

	// Start ICMP pinger in a new ticker
	ticker := time.NewTicker(config.PingInterval)
	for range ticker.C {
		for name, node := range config.Nodes {
			// Skip local node
			if node.ID == config.LocalID {
				continue
			}

			log.Debugf("Pinging %s %+v", name, node)

			// Ping node
			latency, loss, err := icmpLatency(internalIP(config.Prefix4, config.LocalID, 0), internalIP(config.Prefix4, node.ID, 0))
			if err != nil {
				log.Warnf("Error pinging %s: %s", name, err)
			}
			if latency <= config.LatencyThreshold && loss < config.LossThreshold {
				node.Latency = latency
				log.Debugf("Adding candidate node %+v", node)
				candidateNodes[name] = node
			} else {
				delete(candidateNodes, name)
			}

			metricCandidateNodes.Set(float64(len(candidateNodes)))
			metricNodeLatency.With(prometheus.Labels{
				"src": localNodeName,
				"dst": name,
			}).Set(latency.Seconds())
		}
	}
}
