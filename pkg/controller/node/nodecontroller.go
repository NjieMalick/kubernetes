/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package node

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/apis/extensions"
	"k8s.io/kubernetes/pkg/client/cache"
	clientset "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	unversionedcore "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset/typed/core/unversioned"
	"k8s.io/kubernetes/pkg/client/record"
	"k8s.io/kubernetes/pkg/cloudprovider"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/controller/framework"
	"k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/util/flowcontrol"
	"k8s.io/kubernetes/pkg/util/metrics"
	utilnode "k8s.io/kubernetes/pkg/util/node"
	utilruntime "k8s.io/kubernetes/pkg/util/runtime"
	"k8s.io/kubernetes/pkg/util/system"
	"k8s.io/kubernetes/pkg/util/wait"
	"k8s.io/kubernetes/pkg/version"
	"k8s.io/kubernetes/pkg/watch"
)

var (
	ErrCloudInstance        = errors.New("cloud provider doesn't support instances.")
	gracefulDeletionVersion = version.MustParse("v1.1.0")
)

const (
	// nodeStatusUpdateRetry controls the number of retries of writing NodeStatus update.
	nodeStatusUpdateRetry = 5
	// controls how often NodeController will try to evict Pods from non-responsive Nodes.
	nodeEvictionPeriod = 100 * time.Millisecond
	// Burst value for all eviction rate limiters
	evictionRateLimiterBurst = 1
	// The amount of time the nodecontroller polls on the list nodes endpoint.
	apiserverStartupGracePeriod = 10 * time.Minute
)

type zoneState string

const (
	stateNormal              = zoneState("Normal")
	stateFullSegmentation    = zoneState("FullSegmentation")
	statePartialSegmentation = zoneState("PartialSegmentation")
)

type nodeStatusData struct {
	probeTimestamp           unversioned.Time
	readyTransitionTimestamp unversioned.Time
	status                   api.NodeStatus
}

type NodeController struct {
	allocateNodeCIDRs bool
	cloud             cloudprovider.Interface
	clusterCIDR       *net.IPNet
	serviceCIDR       *net.IPNet
	knownNodeSet      map[string]*api.Node
	kubeClient        clientset.Interface
	// Method for easy mocking in unittest.
	lookupIP func(host string) ([]net.IP, error)
	// Value used if sync_nodes_status=False. NodeController will not proactively
	// sync node status in this case, but will monitor node status updated from kubelet. If
	// it doesn't receive update for this amount of time, it will start posting "NodeReady==
	// ConditionUnknown". The amount of time before which NodeController start evicting pods
	// is controlled via flag 'pod-eviction-timeout'.
	// Note: be cautious when changing the constant, it must work with nodeStatusUpdateFrequency
	// in kubelet. There are several constraints:
	// 1. nodeMonitorGracePeriod must be N times more than nodeStatusUpdateFrequency, where
	//    N means number of retries allowed for kubelet to post node status. It is pointless
	//    to make nodeMonitorGracePeriod be less than nodeStatusUpdateFrequency, since there
	//    will only be fresh values from Kubelet at an interval of nodeStatusUpdateFrequency.
	//    The constant must be less than podEvictionTimeout.
	// 2. nodeMonitorGracePeriod can't be too large for user experience - larger value takes
	//    longer for user to see up-to-date node status.
	nodeMonitorGracePeriod time.Duration
	// Value controlling NodeController monitoring period, i.e. how often does NodeController
	// check node status posted from kubelet. This value should be lower than nodeMonitorGracePeriod.
	// TODO: Change node status monitor to watch based.
	nodeMonitorPeriod time.Duration
	// Value used if sync_nodes_status=False, only for node startup. When node
	// is just created, e.g. cluster bootstrap or node creation, we give a longer grace period.
	nodeStartupGracePeriod time.Duration
	// per Node map storing last observed Status together with a local time when it was observed.
	// This timestamp is to be used instead of LastProbeTime stored in Condition. We do this
	// to aviod the problem with time skew across the cluster.
	nodeStatusMap map[string]nodeStatusData
	now           func() unversioned.Time
	// Lock to access evictor workers
	evictorLock *sync.Mutex
	// workers that evicts pods from unresponsive nodes.
	zonePodEvictor         map[string]*RateLimitedTimedQueue
	zoneTerminationEvictor map[string]*RateLimitedTimedQueue
	evictionLimiterQPS     float32
	podEvictionTimeout     time.Duration
	// The maximum duration before a pod evicted from a node can be forcefully terminated.
	maximumGracePeriod time.Duration
	recorder           record.EventRecorder
	// Pod framework and store
	podController *framework.Controller
	podStore      cache.StoreToPodLister
	// Node framework and store
	nodeController *framework.Controller
	nodeStore      cache.StoreToNodeLister
	// DaemonSet framework and store
	daemonSetController *framework.Controller
	daemonSetStore      cache.StoreToDaemonSetLister
	// allocate/recycle CIDRs for node if allocateNodeCIDRs == true
	cidrAllocator CIDRAllocator

	forcefullyDeletePod       func(*api.Pod) error
	nodeExistsInCloudProvider func(string) (bool, error)
	computeZoneStateFunc      func(nodeConditions []*api.NodeCondition) zoneState

	zoneStates map[string]zoneState
}

// NewNodeController returns a new node controller to sync instances from cloudprovider.
// This method returns an error if it is unable to initialize the CIDR bitmap with
// podCIDRs it has already allocated to nodes. Since we don't allow podCIDR changes
// currently, this should be handled as a fatal error.
func NewNodeController(
	cloud cloudprovider.Interface,
	kubeClient clientset.Interface,
	podEvictionTimeout time.Duration,
	evictionLimiterQPS float32,
	nodeMonitorGracePeriod time.Duration,
	nodeStartupGracePeriod time.Duration,
	nodeMonitorPeriod time.Duration,
	clusterCIDR *net.IPNet,
	serviceCIDR *net.IPNet,
	nodeCIDRMaskSize int,
	allocateNodeCIDRs bool) (*NodeController, error) {
	eventBroadcaster := record.NewBroadcaster()
	recorder := eventBroadcaster.NewRecorder(api.EventSource{Component: "controllermanager"})
	eventBroadcaster.StartLogging(glog.Infof)
	if kubeClient != nil {
		glog.V(0).Infof("Sending events to api server.")
		eventBroadcaster.StartRecordingToSink(&unversionedcore.EventSinkImpl{Interface: kubeClient.Core().Events("")})
	} else {
		glog.V(0).Infof("No api server defined - no events will be sent to API server.")
	}

	if kubeClient != nil && kubeClient.Core().GetRESTClient().GetRateLimiter() != nil {
		metrics.RegisterMetricAndTrackRateLimiterUsage("node_controller", kubeClient.Core().GetRESTClient().GetRateLimiter())
	}

	if allocateNodeCIDRs {
		if clusterCIDR == nil {
			glog.Fatal("NodeController: Must specify clusterCIDR if allocateNodeCIDRs == true.")
		}
		mask := clusterCIDR.Mask
		if maskSize, _ := mask.Size(); maskSize > nodeCIDRMaskSize {
			glog.Fatal("NodeController: Invalid clusterCIDR, mask size of clusterCIDR must be less than nodeCIDRMaskSize.")
		}
	}
	evictorLock := sync.Mutex{}

	nc := &NodeController{
		cloud:                     cloud,
		knownNodeSet:              make(map[string]*api.Node),
		kubeClient:                kubeClient,
		recorder:                  recorder,
		podEvictionTimeout:        podEvictionTimeout,
		maximumGracePeriod:        5 * time.Minute,
		evictorLock:               &evictorLock,
		zonePodEvictor:            make(map[string]*RateLimitedTimedQueue),
		zoneTerminationEvictor:    make(map[string]*RateLimitedTimedQueue),
		nodeStatusMap:             make(map[string]nodeStatusData),
		nodeMonitorGracePeriod:    nodeMonitorGracePeriod,
		nodeMonitorPeriod:         nodeMonitorPeriod,
		nodeStartupGracePeriod:    nodeStartupGracePeriod,
		lookupIP:                  net.LookupIP,
		now:                       unversioned.Now,
		clusterCIDR:               clusterCIDR,
		serviceCIDR:               serviceCIDR,
		allocateNodeCIDRs:         allocateNodeCIDRs,
		forcefullyDeletePod:       func(p *api.Pod) error { return forcefullyDeletePod(kubeClient, p) },
		nodeExistsInCloudProvider: func(nodeName string) (bool, error) { return nodeExistsInCloudProvider(cloud, nodeName) },
		computeZoneStateFunc:      ComputeZoneState,
		evictionLimiterQPS:        evictionLimiterQPS,
		zoneStates:                make(map[string]zoneState),
	}

	nc.podStore.Indexer, nc.podController = framework.NewIndexerInformer(
		&cache.ListWatch{
			ListFunc: func(options api.ListOptions) (runtime.Object, error) {
				return nc.kubeClient.Core().Pods(api.NamespaceAll).List(options)
			},
			WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
				return nc.kubeClient.Core().Pods(api.NamespaceAll).Watch(options)
			},
		},
		&api.Pod{},
		controller.NoResyncPeriodFunc(),
		framework.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				nc.maybeDeleteTerminatingPod(obj, nc.nodeStore.Store, nc.forcefullyDeletePod)
			},
			UpdateFunc: func(_, obj interface{}) {
				nc.maybeDeleteTerminatingPod(obj, nc.nodeStore.Store, nc.forcefullyDeletePod)
			},
		},
		// We don't need to build a index for podStore here actually, but build one for consistency.
		// It will ensure that if people start making use of the podStore in more specific ways,
		// they'll get the benefits they expect. It will also reserve the name for future refactorings.
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	nodeEventHandlerFuncs := framework.ResourceEventHandlerFuncs{}
	if nc.allocateNodeCIDRs {
		nodeEventHandlerFuncs = framework.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				node := obj.(*api.Node)
				err := nc.cidrAllocator.AllocateOrOccupyCIDR(node)
				if err != nil {
					glog.Errorf("Error allocating CIDR: %v", err)
				}
			},
			UpdateFunc: func(_, obj interface{}) {
				node := obj.(*api.Node)
				// If the PodCIDR is not empty we either:
				// - already processed a Node that already had a CIDR after NC restarted
				//   (cidr is marked as used),
				// - already processed a Node successfully and allocated a CIDR for it
				//   (cidr is marked as used),
				// - already processed a Node but we did saw a "timeout" response and
				//   request eventually got through in this case we haven't released
				//   the allocated CIDR (cidr is still marked as used).
				// There's a possible error here:
				// - NC sees a new Node and assigns a CIDR X to it,
				// - Update Node call fails with a timeout,
				// - Node is updated by some other component, NC sees an update and
				//   assigns CIDR Y to the Node,
				// - Both CIDR X and CIDR Y are marked as used in the local cache,
				//   even though Node sees only CIDR Y
				// The problem here is that in in-memory cache we see CIDR X as marked,
				// which prevents it from being assigned to any new node. The cluster
				// state is correct.
				// Restart of NC fixes the issue.
				if node.Spec.PodCIDR == "" {
					err := nc.cidrAllocator.AllocateOrOccupyCIDR(node)
					if err != nil {
						glog.Errorf("Error allocating CIDR: %v", err)
					}
				}
			},
			DeleteFunc: func(obj interface{}) {
				node := obj.(*api.Node)
				err := nc.cidrAllocator.ReleaseCIDR(node)
				if err != nil {
					glog.Errorf("Error releasing CIDR: %v", err)
				}
			},
		}
	}

	nc.nodeStore.Store, nc.nodeController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc: func(options api.ListOptions) (runtime.Object, error) {
				return nc.kubeClient.Core().Nodes().List(options)
			},
			WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
				return nc.kubeClient.Core().Nodes().Watch(options)
			},
		},
		&api.Node{},
		controller.NoResyncPeriodFunc(),
		nodeEventHandlerFuncs,
	)

	nc.daemonSetStore.Store, nc.daemonSetController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc: func(options api.ListOptions) (runtime.Object, error) {
				return nc.kubeClient.Extensions().DaemonSets(api.NamespaceAll).List(options)
			},
			WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
				return nc.kubeClient.Extensions().DaemonSets(api.NamespaceAll).Watch(options)
			},
		},
		&extensions.DaemonSet{},
		controller.NoResyncPeriodFunc(),
		framework.ResourceEventHandlerFuncs{},
	)

	if allocateNodeCIDRs {
		var nodeList *api.NodeList
		var err error
		// We must poll because apiserver might not be up. This error causes
		// controller manager to restart.
		if pollErr := wait.Poll(10*time.Second, apiserverStartupGracePeriod, func() (bool, error) {
			nodeList, err = kubeClient.Core().Nodes().List(api.ListOptions{
				FieldSelector: fields.Everything(),
				LabelSelector: labels.Everything(),
			})
			if err != nil {
				glog.Errorf("Failed to list all nodes: %v", err)
				return false, nil
			}
			return true, nil
		}); pollErr != nil {
			return nil, fmt.Errorf("Failed to list all nodes in %v, cannot proceed without updating CIDR map", apiserverStartupGracePeriod)
		}
		nc.cidrAllocator, err = NewCIDRRangeAllocator(kubeClient, clusterCIDR, serviceCIDR, nodeCIDRMaskSize, nodeList)
		if err != nil {
			return nil, err
		}
	}

	return nc, nil
}

// Run starts an asynchronous loop that monitors the status of cluster nodes.
func (nc *NodeController) Run(period time.Duration) {
	go nc.nodeController.Run(wait.NeverStop)
	go nc.podController.Run(wait.NeverStop)
	go nc.daemonSetController.Run(wait.NeverStop)

	// Incorporate the results of node status pushed from kubelet to master.
	go wait.Until(func() {
		if err := nc.monitorNodeStatus(); err != nil {
			glog.Errorf("Error monitoring node status: %v", err)
		}
	}, nc.nodeMonitorPeriod, wait.NeverStop)

	// Managing eviction of nodes:
	// 1. when we delete pods off a node, if the node was not empty at the time we then
	//    queue a termination watcher
	//    a. If we hit an error, retry deletion
	// 2. The terminator loop ensures that pods are eventually cleaned and we never
	//    terminate a pod in a time period less than nc.maximumGracePeriod. AddedAt
	//    is the time from which we measure "has this pod been terminating too long",
	//    after which we will delete the pod with grace period 0 (force delete).
	//    a. If we hit errors, retry instantly
	//    b. If there are no pods left terminating, exit
	//    c. If there are pods still terminating, wait for their estimated completion
	//       before retrying
	go wait.Until(func() {
		nc.evictorLock.Lock()
		defer nc.evictorLock.Unlock()
		for k := range nc.zonePodEvictor {
			nc.zonePodEvictor[k].Try(func(value TimedValue) (bool, time.Duration) {
				remaining, err := deletePods(nc.kubeClient, nc.recorder, value.Value, nc.daemonSetStore)
				if err != nil {
					utilruntime.HandleError(fmt.Errorf("unable to evict node %q: %v", value.Value, err))
					return false, 0
				}

				if remaining {
					nc.zoneTerminationEvictor[k].Add(value.Value)
				}
				return true, 0
			})
		}
	}, nodeEvictionPeriod, wait.NeverStop)

	// TODO: replace with a controller that ensures pods that are terminating complete
	// in a particular time period
	go wait.Until(func() {
		nc.evictorLock.Lock()
		defer nc.evictorLock.Unlock()
		for k := range nc.zoneTerminationEvictor {
			nc.zoneTerminationEvictor[k].Try(func(value TimedValue) (bool, time.Duration) {
				completed, remaining, err := terminatePods(nc.kubeClient, nc.recorder, value.Value, value.AddedAt, nc.maximumGracePeriod)
				if err != nil {
					utilruntime.HandleError(fmt.Errorf("unable to terminate pods on node %q: %v", value.Value, err))
					return false, 0
				}

				if completed {
					glog.V(2).Infof("All pods terminated on %s", value.Value)
					recordNodeEvent(nc.recorder, value.Value, api.EventTypeNormal, "TerminatedAllPods", fmt.Sprintf("Terminated all Pods on Node %s.", value.Value))
					return true, 0
				}

				glog.V(2).Infof("Pods terminating since %s on %q, estimated completion %s", value.AddedAt, value.Value, remaining)
				// clamp very short intervals
				if remaining < nodeEvictionPeriod {
					remaining = nodeEvictionPeriod
				}
				return false, remaining
			})
		}
	}, nodeEvictionPeriod, wait.NeverStop)

	go wait.Until(func() {
		pods, err := nc.podStore.List(labels.Everything())
		if err != nil {
			utilruntime.HandleError(err)
			return
		}
		cleanupOrphanedPods(pods, nc.nodeStore.Store, nc.forcefullyDeletePod)
	}, 30*time.Second, wait.NeverStop)
}

// monitorNodeStatus verifies node status are constantly updated by kubelet, and if not,
// post "NodeReady==ConditionUnknown". It also evicts all pods if node is not ready or
// not reachable for a long period of time.
func (nc *NodeController) monitorNodeStatus() error {
	nodes, err := nc.kubeClient.Core().Nodes().List(api.ListOptions{})
	if err != nil {
		return err
	}
	added, deleted := nc.checkForNodeAddedDeleted(nodes)
	for i := range added {
		glog.V(1).Infof("NodeController observed a new Node: %#v", added[i].Name)
		recordNodeEvent(nc.recorder, added[i].Name, api.EventTypeNormal, "RegisteredNode", fmt.Sprintf("Registered Node %v in NodeController", added[i].Name))
		nc.knownNodeSet[added[i].Name] = added[i]
		// When adding new Nodes we need to check if new zone appeared, and if so add new evictor.
		zone := utilnode.GetZoneKey(added[i])
		if _, found := nc.zonePodEvictor[zone]; !found {
			nc.zonePodEvictor[zone] =
				NewRateLimitedTimedQueue(
					flowcontrol.NewTokenBucketRateLimiter(nc.evictionLimiterQPS, evictionRateLimiterBurst))
		}
		if _, found := nc.zoneTerminationEvictor[zone]; !found {
			nc.zoneTerminationEvictor[zone] = NewRateLimitedTimedQueue(
				flowcontrol.NewTokenBucketRateLimiter(nc.evictionLimiterQPS, evictionRateLimiterBurst))
		}
		nc.cancelPodEviction(added[i])
	}

	for i := range deleted {
		glog.V(1).Infof("NodeController observed a Node deletion: %v", deleted[i].Name)
		recordNodeEvent(nc.recorder, deleted[i].Name, api.EventTypeNormal, "RemovingNode", fmt.Sprintf("Removing Node %v from NodeController", deleted[i].Name))
		nc.evictPods(deleted[i])
		delete(nc.knownNodeSet, deleted[i].Name)
	}

	zoneToNodeConditions := map[string][]*api.NodeCondition{}
	for i := range nodes.Items {
		var gracePeriod time.Duration
		var observedReadyCondition api.NodeCondition
		var currentReadyCondition *api.NodeCondition
		node := &nodes.Items[i]
		for rep := 0; rep < nodeStatusUpdateRetry; rep++ {
			gracePeriod, observedReadyCondition, currentReadyCondition, err = nc.tryUpdateNodeStatus(node)
			if err == nil {
				break
			}
			name := node.Name
			node, err = nc.kubeClient.Core().Nodes().Get(name)
			if err != nil {
				glog.Errorf("Failed while getting a Node to retry updating NodeStatus. Probably Node %s was deleted.", name)
				break
			}
		}
		if err != nil {
			glog.Errorf("Update status  of Node %v from NodeController exceeds retry count."+
				"Skipping - no pods will be evicted.", node.Name)
			continue
		}
		// We do not treat a master node as a part of the cluster for network segmentation checking.
		if !system.IsMasterNode(node) {
			zoneToNodeConditions[utilnode.GetZoneKey(node)] = append(zoneToNodeConditions[utilnode.GetZoneKey(node)], currentReadyCondition)
		}

		decisionTimestamp := nc.now()
		if currentReadyCondition != nil {
			// Check eviction timeout against decisionTimestamp
			if observedReadyCondition.Status == api.ConditionFalse &&
				decisionTimestamp.After(nc.nodeStatusMap[node.Name].readyTransitionTimestamp.Add(nc.podEvictionTimeout)) {
				if nc.evictPods(node) {
					glog.V(4).Infof("Evicting pods on node %s: %v is later than %v + %v", node.Name, decisionTimestamp, nc.nodeStatusMap[node.Name].readyTransitionTimestamp, nc.podEvictionTimeout)
				}
			}
			if observedReadyCondition.Status == api.ConditionUnknown &&
				decisionTimestamp.After(nc.nodeStatusMap[node.Name].probeTimestamp.Add(nc.podEvictionTimeout)) {
				if nc.evictPods(node) {
					glog.V(4).Infof("Evicting pods on node %s: %v is later than %v + %v", node.Name, decisionTimestamp, nc.nodeStatusMap[node.Name].readyTransitionTimestamp, nc.podEvictionTimeout-gracePeriod)
				}
			}
			if observedReadyCondition.Status == api.ConditionTrue {
				if nc.cancelPodEviction(node) {
					glog.V(2).Infof("Node %s is ready again, cancelled pod eviction", node.Name)
				}
			}

			// Report node event.
			if currentReadyCondition.Status != api.ConditionTrue && observedReadyCondition.Status == api.ConditionTrue {
				recordNodeStatusChange(nc.recorder, node, "NodeNotReady")
				if err = markAllPodsNotReady(nc.kubeClient, node.Name); err != nil {
					utilruntime.HandleError(fmt.Errorf("Unable to mark all pods NotReady on node %v: %v", node.Name, err))
				}
			}

			// Check with the cloud provider to see if the node still exists. If it
			// doesn't, delete the node immediately.
			if currentReadyCondition.Status != api.ConditionTrue && nc.cloud != nil {
				exists, err := nc.nodeExistsInCloudProvider(node.Name)
				if err != nil {
					glog.Errorf("Error determining if node %v exists in cloud: %v", node.Name, err)
					continue
				}
				if !exists {
					glog.V(2).Infof("Deleting node (no longer present in cloud provider): %s", node.Name)
					recordNodeEvent(nc.recorder, node.Name, api.EventTypeNormal, "DeletingNode", fmt.Sprintf("Deleting Node %v because it's not present according to cloud provider", node.Name))
					go func(nodeName string) {
						defer utilruntime.HandleCrash()
						// Kubelet is not reporting and Cloud Provider says node
						// is gone. Delete it without worrying about grace
						// periods.
						if err := forcefullyDeleteNode(nc.kubeClient, nodeName, nc.forcefullyDeletePod); err != nil {
							glog.Errorf("Unable to forcefully delete node %q: %v", nodeName, err)
						}
					}(node.Name)
					continue
				}
			}
		}
	}

	for k, v := range zoneToNodeConditions {
		newState := nc.computeZoneStateFunc(v)
		if newState == nc.zoneStates[k] {
			continue
		}
		if newState == stateFullSegmentation {
			glog.V(2).Infof("NodeController is entering network segmentation mode in zone %v.", k)
		} else if newState == stateNormal {
			glog.V(2).Infof("NodeController exited network segmentation mode in zone %v.", k)
		}
		for i := range nodes.Items {
			if utilnode.GetZoneKey(&nodes.Items[i]) == k {
				if newState == stateFullSegmentation {
					// When zone is fully segmented we stop the eviction all together.
					nc.cancelPodEviction(&nodes.Items[i])
				}
				if newState == stateNormal && nc.zoneStates[k] == stateFullSegmentation {
					// When exiting segmentation mode update probe timestamps on all Nodes.
					now := nc.now()
					v := nc.nodeStatusMap[nodes.Items[i].Name]
					v.probeTimestamp = now
					v.readyTransitionTimestamp = now
					nc.nodeStatusMap[nodes.Items[i].Name] = v
				}
			}
		}
		nc.zoneStates[k] = newState
	}

	return nil
}

// For a given node checks its conditions and tries to update it. Returns grace period to which given node
// is entitled, state of current and last observed Ready Condition, and an error if it occurred.
func (nc *NodeController) tryUpdateNodeStatus(node *api.Node) (time.Duration, api.NodeCondition, *api.NodeCondition, error) {
	var err error
	var gracePeriod time.Duration
	var observedReadyCondition api.NodeCondition
	_, currentReadyCondition := api.GetNodeCondition(&node.Status, api.NodeReady)
	if currentReadyCondition == nil {
		// If ready condition is nil, then kubelet (or nodecontroller) never posted node status.
		// A fake ready condition is created, where LastProbeTime and LastTransitionTime is set
		// to node.CreationTimestamp to avoid handle the corner case.
		observedReadyCondition = api.NodeCondition{
			Type:               api.NodeReady,
			Status:             api.ConditionUnknown,
			LastHeartbeatTime:  node.CreationTimestamp,
			LastTransitionTime: node.CreationTimestamp,
		}
		gracePeriod = nc.nodeStartupGracePeriod
		nc.nodeStatusMap[node.Name] = nodeStatusData{
			status:                   node.Status,
			probeTimestamp:           node.CreationTimestamp,
			readyTransitionTimestamp: node.CreationTimestamp,
		}
	} else {
		// If ready condition is not nil, make a copy of it, since we may modify it in place later.
		observedReadyCondition = *currentReadyCondition
		gracePeriod = nc.nodeMonitorGracePeriod
	}

	savedNodeStatus, found := nc.nodeStatusMap[node.Name]
	// There are following cases to check:
	// - both saved and new status have no Ready Condition set - we leave everything as it is,
	// - saved status have no Ready Condition, but current one does - NodeController was restarted with Node data already present in etcd,
	// - saved status have some Ready Condition, but current one does not - it's an error, but we fill it up because that's probably a good thing to do,
	// - both saved and current statuses have Ready Conditions and they have the same LastProbeTime - nothing happened on that Node, it may be
	//   unresponsive, so we leave it as it is,
	// - both saved and current statuses have Ready Conditions, they have different LastProbeTimes, but the same Ready Condition State -
	//   everything's in order, no transition occurred, we update only probeTimestamp,
	// - both saved and current statuses have Ready Conditions, different LastProbeTimes and different Ready Condition State -
	//   Ready Condition changed it state since we last seen it, so we update both probeTimestamp and readyTransitionTimestamp.
	// TODO: things to consider:
	//   - if 'LastProbeTime' have gone back in time its probably an error, currently we ignore it,
	//   - currently only correct Ready State transition outside of Node Controller is marking it ready by Kubelet, we don't check
	//     if that's the case, but it does not seem necessary.
	var savedCondition *api.NodeCondition
	if found {
		_, savedCondition = api.GetNodeCondition(&savedNodeStatus.status, api.NodeReady)
	}
	_, observedCondition := api.GetNodeCondition(&node.Status, api.NodeReady)
	if !found {
		glog.Warningf("Missing timestamp for Node %s. Assuming now as a timestamp.", node.Name)
		savedNodeStatus = nodeStatusData{
			status:                   node.Status,
			probeTimestamp:           nc.now(),
			readyTransitionTimestamp: nc.now(),
		}
	} else if savedCondition == nil && observedCondition != nil {
		glog.V(1).Infof("Creating timestamp entry for newly observed Node %s", node.Name)
		savedNodeStatus = nodeStatusData{
			status:                   node.Status,
			probeTimestamp:           nc.now(),
			readyTransitionTimestamp: nc.now(),
		}
	} else if savedCondition != nil && observedCondition == nil {
		glog.Errorf("ReadyCondition was removed from Status of Node %s", node.Name)
		// TODO: figure out what to do in this case. For now we do the same thing as above.
		savedNodeStatus = nodeStatusData{
			status:                   node.Status,
			probeTimestamp:           nc.now(),
			readyTransitionTimestamp: nc.now(),
		}
	} else if savedCondition != nil && observedCondition != nil && savedCondition.LastHeartbeatTime != observedCondition.LastHeartbeatTime {
		var transitionTime unversioned.Time
		// If ReadyCondition changed since the last time we checked, we update the transition timestamp to "now",
		// otherwise we leave it as it is.
		if savedCondition.LastTransitionTime != observedCondition.LastTransitionTime {
			glog.V(3).Infof("ReadyCondition for Node %s transitioned from %v to %v", node.Name, savedCondition.Status, observedCondition)

			transitionTime = nc.now()
		} else {
			transitionTime = savedNodeStatus.readyTransitionTimestamp
		}
		if glog.V(5) {
			glog.V(5).Infof("Node %s ReadyCondition updated. Updating timestamp: %+v vs %+v.", node.Name, savedNodeStatus.status, node.Status)
		} else {
			glog.V(3).Infof("Node %s ReadyCondition updated. Updating timestamp.", node.Name)
		}
		savedNodeStatus = nodeStatusData{
			status:                   node.Status,
			probeTimestamp:           nc.now(),
			readyTransitionTimestamp: transitionTime,
		}
	}
	nc.nodeStatusMap[node.Name] = savedNodeStatus

	if nc.now().After(savedNodeStatus.probeTimestamp.Add(gracePeriod)) {
		// NodeReady condition was last set longer ago than gracePeriod, so update it to Unknown
		// (regardless of its current value) in the master.
		if currentReadyCondition == nil {
			glog.V(2).Infof("node %v is never updated by kubelet", node.Name)
			node.Status.Conditions = append(node.Status.Conditions, api.NodeCondition{
				Type:               api.NodeReady,
				Status:             api.ConditionUnknown,
				Reason:             "NodeStatusNeverUpdated",
				Message:            fmt.Sprintf("Kubelet never posted node status."),
				LastHeartbeatTime:  node.CreationTimestamp,
				LastTransitionTime: nc.now(),
			})
		} else {
			glog.V(4).Infof("node %v hasn't been updated for %+v. Last ready condition is: %+v",
				node.Name, nc.now().Time.Sub(savedNodeStatus.probeTimestamp.Time), observedReadyCondition)
			if observedReadyCondition.Status != api.ConditionUnknown {
				currentReadyCondition.Status = api.ConditionUnknown
				currentReadyCondition.Reason = "NodeStatusUnknown"
				currentReadyCondition.Message = fmt.Sprintf("Kubelet stopped posting node status.")
				// LastProbeTime is the last time we heard from kubelet.
				currentReadyCondition.LastHeartbeatTime = observedReadyCondition.LastHeartbeatTime
				currentReadyCondition.LastTransitionTime = nc.now()
			}
		}

		// Like NodeReady condition, NodeOutOfDisk was last set longer ago than gracePeriod, so update
		// it to Unknown (regardless of its current value) in the master.
		// TODO(madhusudancs): Refactor this with readyCondition to remove duplicated code.
		_, oodCondition := api.GetNodeCondition(&node.Status, api.NodeOutOfDisk)
		if oodCondition == nil {
			glog.V(2).Infof("Out of disk condition of node %v is never updated by kubelet", node.Name)
			node.Status.Conditions = append(node.Status.Conditions, api.NodeCondition{
				Type:               api.NodeOutOfDisk,
				Status:             api.ConditionUnknown,
				Reason:             "NodeStatusNeverUpdated",
				Message:            fmt.Sprintf("Kubelet never posted node status."),
				LastHeartbeatTime:  node.CreationTimestamp,
				LastTransitionTime: nc.now(),
			})
		} else {
			glog.V(4).Infof("node %v hasn't been updated for %+v. Last out of disk condition is: %+v",
				node.Name, nc.now().Time.Sub(savedNodeStatus.probeTimestamp.Time), oodCondition)
			if oodCondition.Status != api.ConditionUnknown {
				oodCondition.Status = api.ConditionUnknown
				oodCondition.Reason = "NodeStatusUnknown"
				oodCondition.Message = fmt.Sprintf("Kubelet stopped posting node status.")
				oodCondition.LastTransitionTime = nc.now()
			}
		}

		_, currentCondition := api.GetNodeCondition(&node.Status, api.NodeReady)
		if !api.Semantic.DeepEqual(currentCondition, &observedReadyCondition) {
			if _, err = nc.kubeClient.Core().Nodes().UpdateStatus(node); err != nil {
				glog.Errorf("Error updating node %s: %v", node.Name, err)
				return gracePeriod, observedReadyCondition, currentReadyCondition, err
			} else {
				nc.nodeStatusMap[node.Name] = nodeStatusData{
					status:                   node.Status,
					probeTimestamp:           nc.nodeStatusMap[node.Name].probeTimestamp,
					readyTransitionTimestamp: nc.now(),
				}
				return gracePeriod, observedReadyCondition, currentReadyCondition, nil
			}
		}
	}

	return gracePeriod, observedReadyCondition, currentReadyCondition, err
}

func (nc *NodeController) checkForNodeAddedDeleted(nodes *api.NodeList) (added, deleted []*api.Node) {
	for i := range nodes.Items {
		if _, has := nc.knownNodeSet[nodes.Items[i].Name]; !has {
			added = append(added, &nodes.Items[i])
		}
	}
	// If there's a difference between lengths of known Nodes and observed nodes
	// we must have removed some Node.
	if len(nc.knownNodeSet)+len(added) != len(nodes.Items) {
		knowSetCopy := map[string]*api.Node{}
		for k, v := range nc.knownNodeSet {
			knowSetCopy[k] = v
		}
		for i := range nodes.Items {
			delete(knowSetCopy, nodes.Items[i].Name)
		}
		for i := range knowSetCopy {
			deleted = append(deleted, knowSetCopy[i])
		}
	}
	return
}

// cancelPodEviction removes any queued evictions, typically because the node is available again. It
// returns true if an eviction was queued.
func (nc *NodeController) cancelPodEviction(node *api.Node) bool {
	zone := utilnode.GetZoneKey(node)
	nc.evictorLock.Lock()
	defer nc.evictorLock.Unlock()
	wasDeleting := nc.zonePodEvictor[zone].Remove(node.Name)
	wasTerminating := nc.zoneTerminationEvictor[zone].Remove(node.Name)
	if wasDeleting || wasTerminating {
		glog.V(2).Infof("Cancelling pod Eviction on Node: %v", node.Name)
		return true
	}
	return false
}

// evictPods queues an eviction for the provided node name, and returns false if the node is already
// queued for eviction.
func (nc *NodeController) evictPods(node *api.Node) bool {
	nc.evictorLock.Lock()
	defer nc.evictorLock.Unlock()
	foundHealty := false
	for _, state := range nc.zoneStates {
		if state != stateFullSegmentation {
			foundHealty = true
			break
		}
	}
	if !foundHealty {
		return false
	}
	zone := utilnode.GetZoneKey(node)
	return nc.zonePodEvictor[zone].Add(node.Name)
}
