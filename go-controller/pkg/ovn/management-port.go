package ovn

import (
	"fmt"
	"net"
	"os/exec"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
)

func configureManagementPort(nodeName, clusterSubnet, routerIP, interfaceName, interfaceIP string) error {
	// Up the interface.
	_, err := exec.Command("ip", "link", "set", interfaceName, "up").CombinedOutput()
	if err != nil {
		return err
	}

	// The interface may already exist, in which case delete the routes and IP.
	_, err = exec.Command("ip", "addr", "flush", "dev", interfaceName).CombinedOutput()
	if err != nil {
		return err
	}

	// Assign IP address to the internal interface.
	_, err = exec.Command("ip", "addr", "add", interfaceIP, "dev", interfaceName).CombinedOutput()
	if err != nil {
		return err
	}

	// Flush the route for the entire subnet (in case it was added before).
	_, err = exec.Command("ip", "route", "flush", clusterSubnet).CombinedOutput()
	if err != nil {
		return err
	}

	// Create a route for the entire subnet.
	_, err = exec.Command("ip", "route", "add", clusterSubnet, "via", routerIP).CombinedOutput()
	return err
}

// CreateManagementPort creates a logical switch for the node and connect it to the distributed router. This switch will start with one logical port (A OVS internal interface).
// 1. This logical port is via which a node can access all other nodes and the containers running inside them using the private IP addresses.
// 2. When this port is created on the master node, the K8s daemons become reachable from the containers without any NAT.
// 3. The nodes can health-check the pod IP addresses.
func CreateManagementPort(nodeName, localSubnet, clusterSubnet string) error {
	// Create a router port and provide it the first address in the 'local_subnet'.
	ip, localSubnetNet, err := net.ParseCIDR(localSubnet)
	if err != nil {
		return fmt.Errorf("Failed to parse local subnet %v : %v", localSubnetNet, err)
	}
	ip = util.NextIP(ip)
	n, _ := localSubnetNet.Mask.Size()
	routerIPMask := fmt.Sprintf("%s/%d", ip.String(), n)
	routerIP := ip.String()
	// Kubernetes emits events when pods are created. The event will contain
	// only lowercase letters of the hostname even though the kubelet is
	// started with a hostname that contains lowercase and uppercase letters.
	// When the kubelet is started with a hostname containing lowercase and
	// uppercase letters, this causes a mismatch between what the watcher
	// will try to fetch and what kubernetes provides, thus failing to
	// create the port on the logical switch.
	// Until the above is changed, switch to a lowercase hostname for
	// initMinion.
	nodeName = strings.ToLower(nodeName)

	routerMac, stderr, err := util.RunOVNNbctl("--if-exist", "get", "logical_router_port", "rtos-"+nodeName, "mac")
	if err != nil {
		logrus.Errorf("Failed to get logical router port,stderr: %q, error: %v", stderr, err)
		return err
	}

	var clusterRouter string
	if routerMac == "" {
		routerMac = util.GenerateMac()
		clusterRouter, err = util.GetK8sClusterRouter()
		if err != nil {
			return err
		}

		_, stderr, err = util.RunOVNNbctl("--may-exist", "lrp-add", clusterRouter, "rtos-"+nodeName, routerMac, routerIPMask)
		if err != nil {
			logrus.Errorf("Failed to add logical port to router, stderr: %q, error: %v", stderr, err)
			return err
		}
	}

	// Create a logical switch and set its subnet.
	stdout, stderr, err := util.RunOVNNbctl("--", "--may-exist", "ls-add", nodeName, "--", "set", "logical_switch", nodeName, "other-config:subnet="+localSubnet, "external-ids:gateway_ip="+routerIPMask)
	if err != nil {
		logrus.Errorf("Failed to create a logical switch %v, stdout: %q, stderr: %q, error: %v", nodeName, stdout, stderr, err)
		return err
	}

	// Connect the switch to the router.
	stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", nodeName, "stor-"+nodeName, "--", "set", "logical_switch_port", "stor-"+nodeName, "type=router", "options:router-port=rtos-"+nodeName, "addresses="+"\""+routerMac+"\"")
	if err != nil {
		logrus.Errorf("Failed to add logical port to switch, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Make sure br-int is created.
	stdout, stderr, err = util.RunOVSVsctl("--", "--may-exist", "add-br", "br-int")
	if err != nil {
		logrus.Errorf("Failed to create br-int, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Create a OVS internal interface.
	var interfaceName string
	if len(nodeName) > 11 {
		interfaceName = "k8s-" + (nodeName[:11])
	} else {
		interfaceName = "k8s-" + nodeName
	}

	stdout, stderr, err = util.RunOVSVsctl("--", "--may-exist", "add-port",
		"br-int", interfaceName, "--", "set", "interface", interfaceName,
		"type=internal", "mtu_request="+fmt.Sprintf("%d", config.MTU),
		"external-ids:iface-id=k8s-"+nodeName)
	if err != nil {
		logrus.Errorf("Failed to add port to br-int, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}
	macAddress, stderr, err := util.RunOVSVsctl("--if-exists", "get", "interface", interfaceName, "mac_in_use")
	if err != nil {
		logrus.Errorf("Failed to get mac address of ovn-k8s-master, stderr: %q, error: %v", stderr, err)
		return err
	}
	if macAddress == "" {
		return fmt.Errorf("Failed to get mac address of ovn-k8s-master")
	}

	// TODO (runtime.GOOS == "win32"&&macAddress == "00:00:00:00:00:00")

	// Create the OVN logical port.
	ip = util.NextIP(ip)
	portIP := ip.String()
	portIPMask := fmt.Sprintf("%s/%d", portIP, n)
	stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", nodeName, "k8s-"+nodeName, "--", "lsp-set-addresses", "k8s-"+nodeName, macAddress+" "+portIP)
	if err != nil {
		logrus.Errorf("Failed to add logical port to switch, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}
	err = configureManagementPort(nodeName, clusterSubnet, routerIP, interfaceName, portIPMask)
	if err != nil {
		return err
	}

	// Add the load_balancer to the switch.
	k8sClusterLbTCP, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "load_balancer", "external_ids:k8s-cluster-lb-tcp=yes")
	if err != nil {
		logrus.Errorf("Failed to get k8sClusterLbTCP, stderr: %q, error: %v", stderr, err)
		return err
	}
	if k8sClusterLbTCP == "" {
		return fmt.Errorf("Failed to get k8sClusterLbTCP")
	}

	stdout, stderr, err = util.RunOVNNbctl("set", "logical_switch", nodeName, "load_balancer="+k8sClusterLbTCP)
	if err != nil {
		logrus.Errorf("Failed to set logical switch %v's loadbalancer, stdout: %q, stderr: %q, error: %v", nodeName, stdout, stderr, err)
		return err
	}

	k8sClusterLbUDP, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "load_balancer", "external_ids:k8s-cluster-lb-udp=yes")
	if err != nil {
		logrus.Errorf("Failed to get k8sClusterLbUDP, stderr: %q, error: %v", stderr, err)
		return err
	}
	if k8sClusterLbUDP == "" {
		return fmt.Errorf("Failed to get k8sClusterLbUDP")
	}

	stdout, stderr, err = util.RunOVNNbctl("add", "logical_switch", nodeName, "load_balancer", k8sClusterLbUDP)
	if err != nil {
		logrus.Errorf("Failed to add logical switch %v's loadbalancer, stdout: %q, stderr: %q, error: %v", nodeName, stdout, stderr, err)
		return err
	}

	return nil
}
