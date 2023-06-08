package server

import (
	"github.com/cybozu-go/cke"
	"github.com/cybozu-go/cke/op"
	"github.com/cybozu-go/cke/op/clusterdns"
	"github.com/cybozu-go/cke/op/etcd"
	"github.com/cybozu-go/cke/op/k8s"
	"github.com/cybozu-go/cke/op/nodedns"
	"github.com/cybozu-go/cke/static"
	"github.com/cybozu-go/log"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"time"
)

type DecideOpsRebootArgs struct {
	RQEntries      []*cke.RebootQueueEntry
	NewlyDrained   []*cke.RebootQueueEntry
	DrainCompleted []*cke.RebootQueueEntry
	DrainTimedout  []*cke.RebootQueueEntry
	RebootDequeued []*cke.RebootQueueEntry
}

// DecideOps returns the next operations to do and the operation phase.
// This returns nil when no operations need to be done.
func DecideOps(c *cke.Cluster, cs *cke.ClusterStatus, constraints *cke.Constraints, resources []cke.ResourceDefinition, rebootArgs DecideOpsRebootArgs, config *Config) ([]cke.Operator, cke.OperationPhase) {
	nf := NewNodeFilter(c, cs)

	// 0. Execute upgrade operation if necessary
	if cs.ConfigVersion != cke.ConfigVersion {
		// Upgrade operations run only when all CPs are SSH reachable
		if len(nf.SSHNotConnectedNodes(nf.cluster.Nodes, true, false)) > 0 {
			log.Warn("cannot upgrade for unreachable nodes", nil)
			return nil, cke.PhaseUpgradeAborted
		}
		return []cke.Operator{op.UpgradeOp(cs.ConfigVersion, nf.ControlPlane())}, cke.PhaseUpgrade
	}

	// 1. Run or restart rivers.  This guarantees:
	// - CKE tools image is pulled on all nodes.
	// - Rivers runs on all nodes and will proxy requests only to control plane nodes.
	if ops := riversOps(c, nf, config.MaxConcurrentUpdates); len(ops) > 0 {
		return ops, cke.PhaseRivers
	}

	// 2. Bootstrap etcd cluster, if not yet.
	if !nf.EtcdBootstrapped() {
		// Etcd boot operations run only when all CPs are SSH reachable
		if len(nf.SSHNotConnectedNodes(nf.cluster.Nodes, true, false)) > 0 {
			log.Warn("cannot bootstrap etcd for unreachable nodes", nil)
			return nil, cke.PhaseEtcdBootAborted
		}
		return []cke.Operator{etcd.BootOp(nf.ControlPlane(), c.Options.Etcd)}, cke.PhaseEtcdBoot
	}

	// 3. Start etcd containers.
	if nodes := nf.SSHConnectedNodes(nf.EtcdStoppedMembers(), true, false); len(nodes) > 0 {
		return []cke.Operator{etcd.StartOp(nodes, c.Options.Etcd)}, cke.PhaseEtcdStart
	}

	// 4. Wait for etcd cluster to become ready
	if !cs.Etcd.IsHealthy {
		return []cke.Operator{etcd.WaitClusterOp(nf.ControlPlane())}, cke.PhaseEtcdWait
	}

	// 5. Run or restart kubernetes components.
	if ops := k8sOps(c, nf, cs, config.MaxConcurrentUpdates); len(ops) > 0 {
		return ops, cke.PhaseK8sStart
	}

	// 6. Maintain etcd cluster, only when all CPs are SSH reachable.
	if len(nf.SSHNotConnectedNodes(nf.cluster.Nodes, true, false)) == 0 {
		if o := etcdMaintOp(c, nf); o != nil {
			return []cke.Operator{o}, cke.PhaseEtcdMaintain
		}
	}

	// 7. Maintain k8s resources.
	if ops := k8sMaintOps(c, cs, resources, rebootArgs.RQEntries, rebootArgs.NewlyDrained, nf); len(ops) > 0 {
		return ops, cke.PhaseK8sMaintain
	}

	// 8. Stop and delete control plane services running on non control plane nodes.
	if ops := cleanOps(c, nf); len(ops) > 0 {
		return ops, cke.PhaseStopCP
	}

	// 9. Uncordon nodes if nodes are cordoned by CKE.
	if o := rebootUncordonOp(rebootArgs.RQEntries, nf); o != nil {
		return []cke.Operator{o}, cke.PhaseUncordonNodes
	}

	// 10. Reboot nodes if reboot request has been arrived to the reboot queue, and the number of unreachable nodes is less than a threshold.
	if ops, phaseReboot := rebootOps(c, constraints, rebootArgs, nf, config.DrainRetryTimes, config.DrainRetryInterval); phaseReboot {
		if !nf.EtcdIsGood() {
			log.Warn("cannot reboot nodes because etcd cluster is not responding and in-sync", nil)
			return nil, cke.PhaseRebootNodes
		}
		return ops, cke.PhaseRebootNodes
	}

	return nil, cke.PhaseCompleted
}

func riversOps(c *cke.Cluster, nf *NodeFilter, maxConcurrentUpdates int) (ops []cke.Operator) {
	if nodes := nf.SSHConnectedNodes(nf.RiversStoppedNodes(), true, true); len(nodes) > 0 {
		ops = append(ops, splitOperators(nodes, func(ns []*cke.Node) cke.Operator {
			return op.RiversBootOp(ns, nf.ControlPlane(), c.Options.Rivers, op.RiversContainerName, op.RiversUpstreamPort, op.RiversListenPort)
		}, maxConcurrentUpdates)...)
	}
	if nodes := nf.SSHConnectedNodes(nf.RiversOutdatedNodes(), true, true); len(nodes) > 0 {
		ops = append(ops, splitOperators(nodes, func(ns []*cke.Node) cke.Operator {
			return op.RiversRestartOp(ns, nf.ControlPlane(), c.Options.Rivers, op.RiversContainerName, op.RiversUpstreamPort, op.RiversListenPort)
		}, maxConcurrentUpdates)...)
	}
	if nodes := nf.SSHConnectedNodes(nf.EtcdRiversStoppedNodes(), true, false); len(nodes) > 0 {
		ops = append(ops, splitOperators(nodes, func(ns []*cke.Node) cke.Operator {
			return op.RiversBootOp(ns, nf.ControlPlane(), c.Options.EtcdRivers, op.EtcdRiversContainerName, op.EtcdRiversUpstreamPort, op.EtcdRiversListenPort)
		}, maxConcurrentUpdates)...)
	}
	if nodes := nf.SSHConnectedNodes(nf.EtcdRiversOutdatedNodes(), true, false); len(nodes) > 0 {
		ops = append(ops, splitOperators(nodes, func(ns []*cke.Node) cke.Operator {
			return op.RiversRestartOp(ns, nf.ControlPlane(), c.Options.EtcdRivers, op.EtcdRiversContainerName, op.EtcdRiversUpstreamPort, op.EtcdRiversListenPort)
		}, maxConcurrentUpdates)...)
	}
	return ops
}

func splitOperators(nodes []*cke.Node, createOps func(ns []*cke.Node) cke.Operator, maxConcurrentUpdates int) (ops []cke.Operator) {
	for i := 0; i < len(nodes); i += maxConcurrentUpdates {
		end := i + maxConcurrentUpdates
		if end > len(nodes) {
			end = len(nodes)
		}
		ops = append(ops, createOps(nodes[i:end]))
	}
	return ops
}

func k8sOps(c *cke.Cluster, nf *NodeFilter, cs *cke.ClusterStatus, maxConcurrentUpdates int) (ops []cke.Operator) {
	// For cp nodes
	if nodes := nf.SSHConnectedNodes(nf.APIServerStoppedNodes(), true, false); len(nodes) > 0 {
		kubeletConfig := k8s.GenerateKubeletConfiguration(c.Options.Kubelet, "0.0.0.0", nil)
		ops = append(ops, k8s.APIServerRestartOp(nodes, nf.ControlPlane(), c.ServiceSubnet, c.Options.APIServer, kubeletConfig.ClusterDomain))
	}
	if nodes := nf.SSHConnectedNodes(nf.APIServerOutdatedNodes(), true, false); len(nodes) > 0 {
		kubeletConfig := k8s.GenerateKubeletConfiguration(c.Options.Kubelet, "0.0.0.0", nil)
		ops = append(ops, k8s.APIServerRestartOp(nodes, nf.ControlPlane(), c.ServiceSubnet, c.Options.APIServer, kubeletConfig.ClusterDomain))
	}
	if nodes := nf.SSHConnectedNodes(nf.ControllerManagerStoppedNodes(), true, false); len(nodes) > 0 {
		ops = append(ops, k8s.ControllerManagerBootOp(nodes, c.Name, c.ServiceSubnet, c.Options.ControllerManager))
	}
	if nodes := nf.SSHConnectedNodes(nf.ControllerManagerOutdatedNodes(), true, false); len(nodes) > 0 {
		ops = append(ops, k8s.ControllerManagerRestartOp(nodes, c.Name, c.ServiceSubnet, c.Options.ControllerManager))
	}
	if nodes := nf.SSHConnectedNodes(nf.SchedulerStoppedNodes(), true, false); len(nodes) > 0 {
		ops = append(ops, k8s.SchedulerBootOp(nodes, c.Name, c.Options.Scheduler))
	}
	if nodes := nf.SSHConnectedNodes(nf.SchedulerOutdatedNodes(c.Options.Scheduler), true, false); len(nodes) > 0 {
		ops = append(ops, k8s.SchedulerRestartOp(nodes, c.Name, c.Options.Scheduler))
	}

	// For all nodes
	apiServer := nf.HealthyAPIServer()
	if nodes := nf.SSHConnectedNodes(nf.KubeletUnrecognizedNodes(), true, true); len(nodes) > 0 {
		ops = append(ops, splitOperators(nodes, func(ns []*cke.Node) cke.Operator {
			return k8s.KubeletRestartOp(ns, c.Name, c.Options.Kubelet, cs.NodeStatuses)
		}, maxConcurrentUpdates)...)
	}
	if nodes := nf.SSHConnectedNodes(nf.KubeletStoppedNodes(), true, true); len(nodes) > 0 {
		ops = append(ops, splitOperators(nodes, func(ns []*cke.Node) cke.Operator {
			return k8s.KubeletBootOp(ns, nf.KubeletStoppedRegisteredNodes(), apiServer, c.Name, c.Options.Kubelet, cs.NodeStatuses)
		}, maxConcurrentUpdates)...)
	}
	if nodes := nf.SSHConnectedNodes(nf.KubeletOutdatedNodes(), true, true); len(nodes) > 0 {
		ops = append(ops, splitOperators(nodes, func(ns []*cke.Node) cke.Operator {
			return k8s.KubeletRestartOp(ns, c.Name, c.Options.Kubelet, cs.NodeStatuses)
		}, maxConcurrentUpdates)...)
	}
	if nodes := nf.SSHConnectedNodes(nf.ProxyStoppedNodes(), true, true); len(nodes) > 0 {
		ops = append(ops, splitOperators(nodes, func(ns []*cke.Node) cke.Operator {
			return k8s.KubeProxyBootOp(ns, c.Name, "", c.Options.Proxy)
		}, maxConcurrentUpdates)...)
	}
	if nodes := nf.SSHConnectedNodes(nf.ProxyOutdatedNodes(c.Options.Proxy), true, true); len(nodes) > 0 {
		ops = append(ops, splitOperators(nodes, func(ns []*cke.Node) cke.Operator {
			return k8s.KubeProxyRestartOp(ns, c.Name, "", c.Options.Proxy)
		}, maxConcurrentUpdates)...)
	}
	if nodes := nf.SSHConnectedNodes(nf.ProxyRunningUnexpectedlyNodes(), true, true); len(nodes) > 0 {
		ops = append(ops, splitOperators(nodes, func(ns []*cke.Node) cke.Operator {
			return op.ProxyStopOp(ns)
		}, maxConcurrentUpdates)...)
	}
	return ops
}

func etcdMaintOp(c *cke.Cluster, nf *NodeFilter) cke.Operator {
	// this function is called only when all the CPs are reachable.
	// so, filtering by SSHConnectedNodes(nodes, true, ...) is not required.

	if members := nf.EtcdNonClusterMembers(false); len(members) > 0 {
		return etcd.RemoveMemberOp(nf.ControlPlane(), members)
	}
	if nodes, ids := nf.EtcdNonCPMembers(false); len(nodes) > 0 {
		return etcd.DestroyMemberOp(nf.ControlPlane(), nf.SSHConnectedNodes(nodes, false, true), ids)
	}
	if nodes := nf.EtcdUnstartedMembers(); len(nodes) > 0 {
		return etcd.AddMemberOp(nf.ControlPlane(), nodes[0], c.Options.Etcd)
	}
	if nodes := nf.EtcdUnmarkedMembers(); len(nodes) > 0 {
		return etcd.MarkMemberOp(nodes)
	}

	if !nf.EtcdIsGood() {
		log.Warn("etcd is not good for maintenance", nil)
		// return nil to proceed to k8s maintenance.
		return nil
	}

	// Adding members or removing/restarting healthy members is done only when
	// all members are in sync.

	if nodes := nf.EtcdNewMembers(); len(nodes) > 0 {
		return etcd.AddMemberOp(nf.ControlPlane(), nodes[0], c.Options.Etcd)
	}
	if members := nf.EtcdNonClusterMembers(true); len(members) > 0 {
		return etcd.RemoveMemberOp(nf.ControlPlane(), members)
	}
	if nodes, ids := nf.EtcdNonCPMembers(true); len(nodes) > 0 {
		return etcd.DestroyMemberOp(nf.ControlPlane(), nf.SSHConnectedNodes(nodes, false, true), ids)
	}
	if nodes := nf.EtcdOutdatedMembers(); len(nodes) > 0 {
		return etcd.RestartOp(nf.ControlPlane(), nodes[0], c.Options.Etcd)
	}

	return nil
}

func k8sMaintOps(c *cke.Cluster, cs *cke.ClusterStatus, resources []cke.ResourceDefinition, rqEntries []*cke.RebootQueueEntry, newlyDrained []*cke.RebootQueueEntry, nf *NodeFilter) (ops []cke.Operator) {
	ks := cs.Kubernetes
	apiServer := nf.HealthyAPIServer()

	if !ks.IsControlPlaneReady {
		return []cke.Operator{op.KubeWaitOp(apiServer)}
	}

	ops = append(ops, decideResourceOps(apiServer, ks, resources, ks.IsReady(c))...)

	ops = append(ops, decideClusterDNSOps(apiServer, c, ks)...)

	ops = append(ops, decideNodeDNSOps(apiServer, c, ks)...)

	var masterReadyAddresses, masterNotReadyAddresses []string
OUTER_MASTER:
	for _, n := range nf.HealthyAPIServerNodes() {
		for _, entry := range rqEntries {
			if entry.Node != n.Address {
				continue
			}
			switch entry.Status {
			case cke.RebootStatusDraining, cke.RebootStatusRebooting:
				masterNotReadyAddresses = append(masterNotReadyAddresses, n.Address)
				continue OUTER_MASTER
			}
		}
		for _, entry := range newlyDrained {
			if entry.Node == n.Address {
				masterNotReadyAddresses = append(masterNotReadyAddresses, n.Address)
				continue OUTER_MASTER
			}
		}
		masterReadyAddresses = append(masterReadyAddresses, n.Address)
	}
	for _, n := range nf.UnhealthyAPIServerNodes() {
		masterNotReadyAddresses = append(masterNotReadyAddresses, n.Address)
	}

	masterEP := &endpointParams{}
	masterEP.namespace = metav1.NamespaceDefault
	masterEP.name = "kubernetes"
	masterEP.readyIPs = masterReadyAddresses
	masterEP.notReadyIPs = masterNotReadyAddresses
	masterEP.portName = "https"
	masterEP.port = 6443
	masterEP.serviceName = "kubernetes"
	epOps := decideEpEpsOps(masterEP, ks.MasterEndpoints, ks.MasterEndpointSlice, apiServer)
	ops = append(ops, epOps...)

	// Endpoints needs a corresponding Service.
	// If an Endpoints lacks such a Service, it will be removed.
	// https://github.com/kubernetes/kubernetes/blob/b7c2d923ef4e166b9572d3aa09ca72231b59b28b/pkg/controller/endpoint/endpoints_controller.go#L392-L397
	svcOp := decideEtcdServiceOps(apiServer, ks.EtcdService)
	if svcOp != nil {
		ops = append(ops, svcOp)
	}

	var etcdReadyAddresses, etcdNotReadyAddresses []string
OUTER_ETCD:
	for _, n := range nf.ControlPlane() {
		for _, entry := range rqEntries {
			if entry.Node != n.Address {
				continue
			}
			switch entry.Status {
			case cke.RebootStatusDraining, cke.RebootStatusRebooting:
				etcdNotReadyAddresses = append(etcdNotReadyAddresses, n.Address)
				continue OUTER_ETCD
			}
		}
		for _, entry := range newlyDrained {
			if entry.Node == n.Address {
				etcdNotReadyAddresses = append(etcdNotReadyAddresses, n.Address)
				continue OUTER_ETCD
			}
		}
		etcdReadyAddresses = append(etcdReadyAddresses, n.Address)
	}
	etcdEP := &endpointParams{}
	etcdEP.namespace = metav1.NamespaceSystem
	etcdEP.name = op.EtcdEndpointsName
	etcdEP.readyIPs = etcdReadyAddresses
	etcdEP.notReadyIPs = etcdNotReadyAddresses
	etcdEP.port = 2379
	etcdEP.serviceName = op.EtcdServiceName
	epOps = decideEpEpsOps(etcdEP, ks.EtcdEndpoints, ks.EtcdEndpointSlice, apiServer)
	ops = append(ops, epOps...)

	if nodes := nf.OutdatedAttrsNodes(); len(nodes) > 0 {
		ops = append(ops, op.KubeNodeUpdateOp(apiServer, nodes))
	}

	if nodes := nf.NonClusterNodes(); len(nodes) > 0 {
		ops = append(ops, op.KubeNodeRemoveOp(apiServer, nodes))
	}

	return ops
}

func decideClusterDNSOps(apiServer *cke.Node, c *cke.Cluster, ks cke.KubernetesClusterStatus) (ops []cke.Operator) {
	desiredDNSServers := c.DNSServers
	if ks.DNSService != nil {
		switch ip := ks.DNSService.Spec.ClusterIP; ip {
		case "", "None":
		default:
			desiredDNSServers = []string{ip}
		}
	}

	kubeletConfig := k8s.GenerateKubeletConfiguration(c.Options.Kubelet, "0.0.0.0", nil)
	desiredClusterDomain := kubeletConfig.ClusterDomain

	if ks.ClusterDNS.ConfigMap == nil {
		ops = append(ops, clusterdns.CreateConfigMapOp(apiServer, desiredClusterDomain, desiredDNSServers))
	} else {
		actualConfigData := ks.ClusterDNS.ConfigMap.Data
		expectedConfig := clusterdns.ConfigMap(desiredClusterDomain, desiredDNSServers)
		if actualConfigData["Corefile"] != expectedConfig.Data["Corefile"] {
			ops = append(ops, clusterdns.UpdateConfigMapOp(apiServer, expectedConfig))
		}
	}

	return ops
}

func decideNodeDNSOps(apiServer *cke.Node, c *cke.Cluster, ks cke.KubernetesClusterStatus) (ops []cke.Operator) {
	if len(ks.ClusterDNS.ClusterIP) == 0 {
		return nil
	}

	desiredDNSServers := c.DNSServers
	if ks.DNSService != nil {
		switch ip := ks.DNSService.Spec.ClusterIP; ip {
		case "", "None":
		default:
			desiredDNSServers = []string{ip}
		}
	}

	kubeletConfig := k8s.GenerateKubeletConfiguration(c.Options.Kubelet, "0.0.0.0", nil)
	desiredClusterDomain := kubeletConfig.ClusterDomain

	if ks.NodeDNS.ConfigMap == nil {
		ops = append(ops, nodedns.CreateConfigMapOp(apiServer, ks.ClusterDNS.ClusterIP, desiredClusterDomain, desiredDNSServers))
	} else {
		actualConfigData := ks.NodeDNS.ConfigMap.Data
		expectedConfig := nodedns.ConfigMap(ks.ClusterDNS.ClusterIP, desiredClusterDomain, desiredDNSServers, true)
		if actualConfigData["unbound.conf"] != expectedConfig.Data["unbound.conf"] {
			ops = append(ops, nodedns.UpdateConfigMapOp(apiServer, expectedConfig))
		}
	}

	return ops
}

type endpointParams struct {
	namespace   string
	name        string
	readyIPs    []string
	notReadyIPs []string
	port        int32
	portName    string
	serviceName string
}

func decideEpEpsOps(expect *endpointParams, actualEP *corev1.Endpoints, actualEPS *discoveryv1.EndpointSlice, apiserver *cke.Node) []cke.Operator {
	var ops []cke.Operator

	readyAddresses := make([]corev1.EndpointAddress, len(expect.readyIPs))
	for i, ip := range expect.readyIPs {
		readyAddresses[i] = corev1.EndpointAddress{
			IP: ip,
		}
	}
	notReadyAddresses := make([]corev1.EndpointAddress, len(expect.notReadyIPs))
	for i, ip := range expect.notReadyIPs {
		notReadyAddresses[i] = corev1.EndpointAddress{
			IP: ip,
		}
	}

	ep := &corev1.Endpoints{}
	ep.Namespace = expect.namespace
	ep.Name = expect.name
	ep.Labels = map[string]string{
		"endpointslice.kubernetes.io/skip-mirror": "true",
	}
	ep.Subsets = []corev1.EndpointSubset{
		{
			Addresses:         readyAddresses,
			NotReadyAddresses: notReadyAddresses,
			Ports: []corev1.EndpointPort{
				{
					Name: expect.portName,
					Port: expect.port,
				},
			},
		},
	}
	epOp := decideEpOp(ep, actualEP, apiserver)
	if epOp != nil {
		ops = append(ops, epOp)
	}

	eps := &discoveryv1.EndpointSlice{}
	eps.Namespace = expect.namespace
	eps.Name = expect.name
	eps.Labels = map[string]string{
		"endpointslice.kubernetes.io/managed-by": "cke.cybozu.com",
		"kubernetes.io/service-name":             expect.serviceName,
	}
	eps.AddressType = discoveryv1.AddressTypeIPv4
	eps.Endpoints = make([]discoveryv1.Endpoint, len(expect.readyIPs)+len(expect.notReadyIPs))
	readyTrue := true
	for i := range expect.readyIPs {
		eps.Endpoints[i] = discoveryv1.Endpoint{
			Addresses: expect.readyIPs[i : i+1],
			Conditions: discoveryv1.EndpointConditions{
				Ready: &readyTrue,
			},
		}
	}
	readyFalse := false
	for i := range expect.notReadyIPs {
		eps.Endpoints[len(expect.readyIPs)+i] = discoveryv1.Endpoint{
			Addresses: expect.notReadyIPs[i : i+1],
			Conditions: discoveryv1.EndpointConditions{
				Ready: &readyFalse,
			},
		}
	}
	eps.Ports = []discoveryv1.EndpointPort{
		{
			Name: &expect.portName,
			Port: &expect.port,
		},
	}
	epsOp := decideEpsOp(eps, actualEPS, apiserver)
	if epsOp != nil {
		ops = append(ops, epsOp)
	}

	return ops
}

func decideEpOp(expect, actual *corev1.Endpoints, apiServer *cke.Node) cke.Operator {
	if actual == nil {
		return op.KubeEndpointsCreateOp(apiServer, expect)
	}

	updateOp := op.KubeEndpointsUpdateOp(apiServer, expect)
	if len(actual.Subsets) != 1 {
		return updateOp
	}

	for k, v := range expect.Labels {
		if actual.Labels[k] != v {
			return updateOp
		}
	}

	subset := actual.Subsets[0]
	if len(subset.Ports) != 1 || subset.Ports[0].Port != expect.Subsets[0].Ports[0].Port {
		return updateOp
	}

	if len(subset.Addresses) != len(expect.Subsets[0].Addresses) || len(subset.NotReadyAddresses) != len(expect.Subsets[0].NotReadyAddresses) {
		return updateOp
	}

	endpoints := make(map[string]bool)
	for _, a := range expect.Subsets[0].Addresses {
		endpoints[a.IP] = true
	}
	for _, a := range subset.Addresses {
		if !endpoints[a.IP] {
			return updateOp
		}
	}

	endpoints = make(map[string]bool)
	for _, a := range expect.Subsets[0].NotReadyAddresses {
		endpoints[a.IP] = true
	}
	for _, a := range subset.NotReadyAddresses {
		if !endpoints[a.IP] {
			return updateOp
		}
	}

	return nil
}

func decideEpsOp(expect, actual *discoveryv1.EndpointSlice, apiServer *cke.Node) cke.Operator {
	if actual == nil {
		return op.KubeEndpointSliceCreateOp(apiServer, expect)
	}

	updateOp := op.KubeEndpointSliceUpdateOp(apiServer, expect)

	for k, v := range expect.Labels {
		if actual.Labels[k] != v {
			return updateOp
		}
	}

	if actual.AddressType != expect.AddressType {
		return updateOp
	}

	if len(actual.Endpoints) != len(expect.Endpoints) {
		return updateOp
	}
	for i := range actual.Endpoints {
		actualEP := actual.Endpoints[i]
		expectEP := expect.Endpoints[i]

		if actualEP.Conditions.Ready == nil || *actualEP.Conditions.Ready != *expectEP.Conditions.Ready {
			return updateOp
		}

		if len(actualEP.Addresses) != len(expectEP.Addresses) {
			return updateOp
		}

		addresses := make(map[string]bool)
		for _, a := range expectEP.Addresses {
			addresses[a] = true
		}
		for _, a := range actualEP.Addresses {
			if !addresses[a] {
				return updateOp
			}
		}
	}

	if len(actual.Ports) != 1 {
		return updateOp
	}
	if actual.Ports[0].Name == nil || *actual.Ports[0].Name != *expect.Ports[0].Name {
		return updateOp
	}
	if actual.Ports[0].Port == nil || *actual.Ports[0].Port != *expect.Ports[0].Port {
		return updateOp
	}

	return nil
}

func decideEtcdServiceOps(apiServer *cke.Node, svc *corev1.Service) cke.Operator {
	if svc == nil {
		return op.KubeEtcdServiceCreateOp(apiServer)
	}

	updateOp := op.KubeEtcdServiceUpdateOp(apiServer)

	if len(svc.Spec.Ports) != 1 {
		return updateOp
	}
	if svc.Spec.Ports[0].Port != 2379 {
		return updateOp
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		return updateOp
	}
	if svc.Spec.ClusterIP != corev1.ClusterIPNone {
		return updateOp
	}

	return nil
}

func decideResourceOps(apiServer *cke.Node, ks cke.KubernetesClusterStatus, resources []cke.ResourceDefinition, isReady bool) (ops []cke.Operator) {

	for _, res := range static.Resources {
		// To avoid thundering herd problem. Deployments need to be created only after enough nodes become ready.
		if res.Kind == cke.KindDeployment && !isReady {
			continue
		}
		// We don't have to wait to complete resource creation or update,
		status, ok := ks.ResourceStatuses[res.Key]
		if !ok || res.NeedUpdate(&status) {
			ops = append(ops, op.ResourceApplyOp(apiServer, res, !status.HasBeenSSA))
			return ops
		}
	}

	// decide to apply user defined resources
	if len(resources) == 0 {
		return ops
	}
	rank := resources[0].Rank

	// resources are sorted by rank in advance
	for _, res := range resources {
		if res.Rank != rank && len(ops) > 0 {
			return ops
		}
		if res.Kind == cke.KindDeployment && !isReady {
			continue
		}
		status, ok := ks.ResourceStatuses[res.Key]
		if !ok {
			ops = append(ops, op.ResourceApplyOp(apiServer, res, !status.HasBeenSSA))
			log.Info("need to create", map[string]interface{}{
				"resource_name":      res.Name,
				"resource_namespace": res.Namespace,
				"kind":               res.Kind,
				"completed":          status.Completed,
			})
		} else {
			if res.NeedUpdate(&status) {
				log.Info("need to update", map[string]interface{}{
					"resource_name":      res.Name,
					"resource_namespace": res.Namespace,
					"kind":               res.Kind,
					"completed":          status.Completed,
				})
				ops = append(ops, op.ResourceApplyOp(apiServer, res, !status.HasBeenSSA))
			} else {
				if !status.Completed {
					log.Info("need to wait", map[string]interface{}{
						"resource_name":      res.Name,
						"resource_namespace": res.Namespace,
						"kind":               res.Kind,
						"completed":          status.Completed,
					})
					ops = append(ops, op.NopOp())
				}
			}
		}
		rank = res.Rank
	}
	return ops
}

func cleanOps(c *cke.Cluster, nf *NodeFilter) (ops []cke.Operator) {
	var apiServers, controllerManagers, schedulers, etcds, etcdRivers []*cke.Node

	for _, n := range c.Nodes {
		if !nf.status.NodeStatuses[n.Address].SSHConnected || n.ControlPlane {
			continue
		}

		st := nf.nodeStatus(n)
		if st.Etcd.Running && nf.EtcdIsGood() {
			etcds = append(etcds, n)
		}
		if st.APIServer.Running {
			apiServers = append(apiServers, n)
		}
		if st.ControllerManager.Running {
			controllerManagers = append(controllerManagers, n)
		}
		if st.Scheduler.Running {
			schedulers = append(schedulers, n)
		}
		if st.EtcdRivers.Running {
			etcdRivers = append(etcdRivers, n)
		}
	}

	if len(apiServers) > 0 {
		ops = append(ops, op.APIServerStopOp(apiServers))
	}
	if len(controllerManagers) > 0 {
		ops = append(ops, op.ControllerManagerStopOp(controllerManagers))
	}
	if len(schedulers) > 0 {
		ops = append(ops, op.SchedulerStopOp(schedulers))
	}
	if len(etcds) > 0 {
		ops = append(ops, op.EtcdStopOp(etcds))
	}
	if len(etcdRivers) > 0 {
		ops = append(ops, op.EtcdRiversStopOp(etcdRivers))
	}
	return ops
}

func rebootOps(c *cke.Cluster, constraints *cke.Constraints, rebootArgs DecideOpsRebootArgs, nf *NodeFilter, drainRetryTimes int, drainRetryInterval time.Duration) (ops []cke.Operator, phaseReboot bool) {
	if len(rebootArgs.RQEntries) == 0 {
		return nil, false
	}
	if len(c.Reboot.RebootCommand) == 0 {
		log.Warn("reboot command is not specified in the cluster configuration", nil)
		return nil, false
	}
	if len(c.Reboot.BootCheckCommand) == 0 {
		log.Warn("boot check command is not specified in the cluster configuration", nil)
		return nil, false
	}

	if len(rebootArgs.NewlyDrained) > 0 {
		phaseReboot = true
		sshCheckNodes := make([]*cke.Node, 0, len(nf.cluster.Nodes))
		for _, node := range nf.cluster.Nodes {
			if !rebootProcessing(rebootArgs.RQEntries, node.Address) {
				sshCheckNodes = append(sshCheckNodes, node)
			}
		}
		if len(nf.SSHNotConnectedNodes(sshCheckNodes, true, true)) > constraints.RebootMaximumUnreachable {
			log.Warn("cannot reboot nodes because too many nodes are unreachable", nil)
		} else {
			ops = append(ops, op.RebootDrainStartOp(nf.HealthyAPIServer(), rebootArgs.NewlyDrained, &c.Reboot, drainRetryTimes, drainRetryInterval))
		}
	}
	if len(rebootArgs.DrainCompleted) > 0 {
		phaseReboot = true
		ops = append(ops, op.RebootRebootOp(nf.HealthyAPIServer(), rebootArgs.DrainCompleted, &c.Reboot))
	}
	if len(rebootArgs.DrainTimedout) > 0 {
		phaseReboot = true
		ops = append(ops, op.RebootDrainTimeoutOp(rebootArgs.DrainTimedout))
	}
	if len(rebootArgs.RebootDequeued) > 0 {
		phaseReboot = true
		ops = append(ops, op.RebootDequeueOp(rebootArgs.RebootDequeued))
	}
	if len(ops) > 0 {
		phaseReboot = true
		ops = append(ops, op.RebootRecalcMetricsOp())
	}

	return ops, phaseReboot
}

func rebootUncordonOp(rqEntries []*cke.RebootQueueEntry, nf *NodeFilter) cke.Operator {
	attrNodes := nf.CordonedNodes()
	if len(attrNodes) == 0 {
		return nil
	}
	nodes := make([]string, 0, len(attrNodes))
	for _, n := range attrNodes {
		if !rebootProcessing(rqEntries, n.Name) {
			nodes = append(nodes, n.Name)
		}
	}
	if len(nodes) == 0 {
		return nil
	}
	return op.RebootUncordonOp(nf.HealthyAPIServer(), nodes)
}

func rebootProcessing(rqEntries []*cke.RebootQueueEntry, node string) bool {
	for _, entry := range rqEntries {
		switch entry.Status {
		case cke.RebootStatusDraining, cke.RebootStatusRebooting:
			if node == entry.Node {
				return true
			}
		}
	}
	return false
}
