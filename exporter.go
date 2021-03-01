package main

import (
	"fmt"
	"math"
	"net"
	_ "net/http/pprof"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

// Exporter is a nomad exporter
type Exporter struct {
	client                        *api.Client
	AllowStaleReads               bool
	amILeader                     bool
	PeerMetricsEnabled            bool
	SerfMetricsEnabled            bool
	NodeMetricsEnabled            bool
	JobMetricEnabled              bool
	AllocationsMetricsEnabled     bool
	EvalMetricsEnabled            bool
	DeploymentMetricsEnabled      bool
	AllocationStatsMetricsEnabled bool
	Concurrency                   int
}

func (e *Exporter) shouldReadMetrics() bool {
	return e.amILeader || e.AllowStaleReads
}

// Describe implements Collector interface.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- up
	ch <- nodeInfo
	ch <- clusterServers
	ch <- serfLanMembers
	ch <- serfLanMembersStatus
	ch <- raftAppliedIndex
	ch <- raftCommitIndex
	ch <- raftFsmPending
	ch <- raftLastLogIndex
	ch <- raftLastSnapshotIndex
	ch <- raftNumPeers
	ch <- jobsTotal
	ch <- allocationMemoryBytes
	ch <- allocationCPUPercent
	ch <- allocationCPUTicks
	ch <- allocationCPUUserMode
	ch <- allocationCPUSystemMode
	ch <- allocationCPUThrottled
	ch <- allocationMemoryBytesRequired
	ch <- allocationCPURequired
	ch <- taskCPUPercent
	ch <- taskCPUTotalTicks
	ch <- taskMemoryRssBytes
	ch <- nodeResourceMemory
	ch <- nodeAllocatedMemory
	ch <- nodeUsedMemory
	ch <- nodeResourceCPU
	ch <- nodeResourceIOPS
	ch <- nodeResourceDiskBytes
	ch <- nodeAllocatedCPU
	ch <- nodeUsedCPU

	allocation.Describe(ch)
	allocationZombies.Describe(ch)
	evalCount.Describe(ch)
	taskCount.Describe(ch)

	deploymentCount.Describe(ch)

	deploymentTaskGroupDesiredCanaries.Describe(ch)
	deploymentTaskGroupDesiredTotal.Describe(ch)
	deploymentTaskGroupPlacedAllocs.Describe(ch)
	deploymentTaskGroupHealthyAllocs.Describe(ch)
	deploymentTaskGroupUnhealthyAllocs.Describe(ch)

	clientErrors.Describe(ch)
	apiLatencySummary.Describe(ch)
	apiNodeLatencySummary.Describe(ch)
}

// Collect collects nomad metrics
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {

	if err := measure("leader", func() error {
		return e.collectLeader(ch)
	}); err != nil {
		ch <- prometheus.MustNewConstMetric(
			up, prometheus.GaugeValue, 0,
		)
		logError(err)
		apiLatencySummary.Collect(ch)
		apiNodeLatencySummary.Collect(ch)
		return
	}
	ch <- prometheus.MustNewConstMetric(
		up, prometheus.GaugeValue, 1,
	)

	ch <- clientErrors

	nodes, err := e.fetchNodes()
	if err != nil {
		logError(err)
		return
	}

	if e.NodeMetricsEnabled {
		if err := measure("nodes", func() error { return e.collectNodes(nodes, ch) }); err != nil {
			logError(err)
			return
		}
	}

	if e.AllocationsMetricsEnabled {
		if err := measure("allocations", func() error { return e.collectAllocations(nodes, ch) }); err != nil {
			logError(err)
			return
		}
	}

	if e.PeerMetricsEnabled {
		if err := measure("peers", func() error { return e.collectPeerMetrics(ch) }); err != nil {
			logError(err)
			return
		}
	}

	if e.SerfMetricsEnabled {
		if err := measure("self", func() error { return e.collectSerfMetrics(ch) }); err != nil {
			logError(err)
			return
		}
	}

	if e.JobMetricEnabled {
		if err := measure("jobs", func() error { return e.collectJobsMetrics(ch) }); err != nil {
			logError(err)
			return
		}
	}

	if e.EvalMetricsEnabled {
		if err := measure("eval", func() error { return e.collectEvalMetrics(ch) }); err != nil {
			logError(err)
			return
		}
	}

	if e.DeploymentMetricsEnabled {
		if err := measure("deployment", func() error { return e.collectDeploymentMetrics(ch) }); err != nil {
			logError(err)
			return
		}
	}

	apiLatencySummary.Collect(ch)
	apiNodeLatencySummary.Collect(ch)
}

func (e *Exporter) collectLeader(ch chan<- prometheus.Metric) error {
	leader, err := e.client.Status().Leader()
	if err != nil {
		return fmt.Errorf("could not collect leader: %s", err)
	}

	logrus.Debugf("Leader is %s", leader)
	logrus.Debugf("Client address is %s", e.client.Address())

	leaderHostname, _, err := net.SplitHostPort(leader)
	if err != nil {
		return fmt.Errorf("leader is not a host:port but %s: %s", leader, err)
	}

	clientHost, err := url.Parse(e.client.Address())
	if err != nil {
		return fmt.Errorf("client address %s can't be parsed as a url: %s", e.client.Address(), err)
	}

	logrus.Debugf("Client Hostname is %s", clientHost.Hostname())
	logrus.Debugf("Leader Hostname is %s", leaderHostname)

	var isLeader float64
	if leaderHostname == clientHost.Hostname() {
		isLeader = 1
	}

	e.amILeader = isLeader == 1

	ch <- prometheus.MustNewConstMetric(
		clusterLeader, prometheus.GaugeValue, isLeader,
	)
	return nil
}

func (e *Exporter) collectJobsMetrics(ch chan<- prometheus.Metric) error {
	if !e.shouldReadMetrics() {
		return nil
	}

	jobs, _, err := e.client.Jobs().List(&api.QueryOptions{
		AllowStale: true,
		WaitTime:   1 * time.Millisecond,
	})
	if err != nil {
		return fmt.Errorf("could not get jobs: %s", err)
	}
	logrus.Debugf("collected job metrics %d", len(jobs))
	ch <- prometheus.MustNewConstMetric(
		jobsTotal, prometheus.GaugeValue, float64(len(jobs)),
	)
	return nil
}

func (e *Exporter) collectNodes(nodes nodeMap, ch chan<- prometheus.Metric) error {
	ch <- prometheus.MustNewConstMetric(
		serfLanMembers, prometheus.GaugeValue, float64(len(nodes)),
	)
	logrus.Debugf("I've the nodes list with %d nodes", len(nodes))

	if !e.shouldReadMetrics() {
		return nil
	}

	var w sync.WaitGroup
	pool := make(chan func(), e.Concurrency)
	go func() {
		for f := range pool {
			go f()
		}
	}()

	for _, node := range nodes {
		w.Add(1)
		pool <- func(node api.NodeListStub) func() {
			return func() {
				defer w.Done()
				state := 1
				drain := strconv.FormatBool(node.Drain)

				ch <- prometheus.MustNewConstMetric(
					nodeInfo, prometheus.GaugeValue, 1,
					node.NodeClass, node.Datacenter, drain, node.Name,
					node.ID, node.SchedulingEligibility, node.Status, node.Version,
				)

				if !nodes.IsReady(node.ID) {
					state = 0
				}
				ch <- prometheus.MustNewConstMetric(
					serfLanMembersStatus, prometheus.GaugeValue, float64(state),
					node.NodeClass, node.Datacenter, node.Name, node.ID, drain,
				)

				if !nodes.IsReady(node.ID) {
					logrus.Debugf("Skipping node information and allocations %s because it is %s", node.Name, node.Status)
					return
				}

				if !validVersion(node.Name, node.Version) {
					return
				}

				if !e.AllocationStatsMetricsEnabled {
					return
				}

				logrus.Debugf("Fetching node %#v", node)
				o := newNodeLatencyObserver(node.Name, "fetch_node")
				n, _, err := e.client.Nodes().Info(node.ID, &api.QueryOptions{
					AllowStale: true,
					WaitTime:   1 * time.Millisecond,
				})
				o.observe()
				if err != nil {
					logError(fmt.Errorf("Failed to get node %s info: %s", node.Name, err))
					return
				}

				logrus.Debugf("Node %s fetched", n.Name)

				o = newNodeLatencyObserver(n.Name, "get_running_allocs")
				runningAllocs, err := e.getRunningAllocs(n.ID)
				o.observe()
				if err != nil {
					logError(fmt.Errorf("failed to get node %s running allocs: %s", n.Name, err))
					return
				}

				var allocatedCPU, allocatedMemory int
				for _, alloc := range runningAllocs {
					allocatedCPU += *alloc.Resources.CPU
					allocatedMemory += *alloc.Resources.MemoryMB
				}

				nodeLabels := []string{n.Name, n.Datacenter}
				ch <- prometheus.MustNewConstMetric(
					nodeResourceMemory, prometheus.GaugeValue, float64(*n.Resources.MemoryMB)*1024*1024,
					nodeLabels...,
				)
				ch <- prometheus.MustNewConstMetric(
					nodeAllocatedMemory, prometheus.GaugeValue, float64(allocatedMemory)*1024*1024,
					nodeLabels...,
				)
				ch <- prometheus.MustNewConstMetric(
					nodeAllocatedCPU, prometheus.GaugeValue, float64(allocatedCPU),
					nodeLabels...,
				)
				ch <- prometheus.MustNewConstMetric(
					nodeResourceCPU, prometheus.GaugeValue, float64(*n.Resources.CPU),
					nodeLabels...,
				)
				ch <- prometheus.MustNewConstMetric(
					nodeResourceIOPS, prometheus.GaugeValue, float64(*n.Resources.IOPS),
					nodeLabels...,
				)
				ch <- prometheus.MustNewConstMetric(
					nodeResourceDiskBytes, prometheus.GaugeValue, float64(*n.Resources.DiskMB)*1024*1024,
					nodeLabels...,
				)

				o = newNodeLatencyObserver(n.Name, "get_stats")
				nodeStats, err := e.client.Nodes().Stats(n.ID, &api.QueryOptions{
					AllowStale: true,
					WaitTime:   1 * time.Millisecond,
				})
				o.observe()
				if err != nil {
					logError(fmt.Errorf("failed to get node %s stats: %s", n.Name, err))
					return
				}
				logrus.Debugf("Fetched node %s stats", n.Name)

				ch <- prometheus.MustNewConstMetric(
					nodeUsedMemory, prometheus.GaugeValue, float64(nodeStats.Memory.Used),
					nodeLabels...,
				)
				ch <- prometheus.MustNewConstMetric(
					nodeUsedCPU, prometheus.GaugeValue, float64(math.Floor(nodeStats.CPUTicksConsumed)),
					nodeLabels...,
				)
			}
		}(*node)
	}

	w.Wait()

	logrus.Debugf("done waiting for node metrics")
	return nil
}

func (e *Exporter) getRunningAllocs(nodeID string) ([]*api.Allocation, error) {
	var allocs []*api.Allocation

	// Query the node allocations
	nodeAllocs, _, err := e.client.Nodes().Allocations(nodeID, &api.QueryOptions{
		AllowStale: true,
		WaitTime:   1 * time.Millisecond,
	})

	// Filter list to only running allocations
	for _, alloc := range nodeAllocs {
		if alloc.ClientStatus == "running" {
			allocs = append(allocs, alloc)
		}
	}
	return allocs, err
}

func (e *Exporter) collectPeerMetrics(ch chan<- prometheus.Metric) error {
	if !e.shouldReadMetrics() {
		return nil
	}

	peers, err := e.client.Status().Peers()
	if err != nil {
		return fmt.Errorf("failed to get peer metrics: %s", err)
	}
	ch <- prometheus.MustNewConstMetric(
		clusterServers, prometheus.GaugeValue, float64(len(peers)),
	)
	return nil
}

func (e *Exporter) collectSerfMetrics(ch chan<- prometheus.Metric) error {
	self, err := e.client.Agent().Self()
	if err != nil {
		return fmt.Errorf("failed to get self metrics: %s", err)
	}

	if amIServer, err := strconv.ParseBool(self.Stats["nomad"]["server"]); err == nil && !amIServer {
		return fmt.Errorf("I am not a server")
	}
	raft := self.Stats["raft"]
	datacenter, err := e.client.Agent().Datacenter()
	if err != nil {
		return fmt.Errorf("unable to fetch the datacenter")
	}
	nodeName, err := e.client.Agent().NodeName()
	if err != nil {
		return fmt.Errorf("unable to fetch the node name")
	}
	appliedIndex, err := strconv.ParseFloat(raft["applied_index"], 64)
	if err == nil {
		ch <- prometheus.MustNewConstMetric(
			raftAppliedIndex, prometheus.GaugeValue, appliedIndex,
			datacenter, nodeName,
		)
	}

	commitIndex, err := strconv.ParseFloat(raft["commit_index"], 64)
	if err == nil {
		ch <- prometheus.MustNewConstMetric(
			raftCommitIndex, prometheus.GaugeValue, commitIndex,
			datacenter, nodeName,
		)
	}

	lastLogIndex, err := strconv.ParseFloat(raft["last_log_index"], 64)
	if err == nil {
		ch <- prometheus.MustNewConstMetric(
			raftLastLogIndex, prometheus.GaugeValue, lastLogIndex,
			datacenter, nodeName,
		)
	}

	fsmPending, err := strconv.ParseFloat(raft["fsm_pending"], 64)
	if err == nil {
		ch <- prometheus.MustNewConstMetric(
			raftFsmPending, prometheus.GaugeValue, fsmPending,
			datacenter, nodeName,
		)
	}

	lastSnapshotIndex, err := strconv.ParseFloat(raft["last_snapshot_index"], 64)
	if err == nil {
		ch <- prometheus.MustNewConstMetric(
			raftLastSnapshotIndex, prometheus.GaugeValue, lastSnapshotIndex,
			datacenter, nodeName,
		)
	}

	numPeers, err := strconv.ParseFloat(raft["num_peers"], 64)
	if err == nil {
		ch <- prometheus.MustNewConstMetric(
			raftNumPeers, prometheus.GaugeValue, numPeers,
			datacenter, nodeName,
		)
	}
	return nil
}

func (e *Exporter) collectAllocations(nodes nodeMap, ch chan<- prometheus.Metric) error {
	allocation.Reset()
	taskCount.Reset()

	if !e.shouldReadMetrics() {
		return nil
	}

	o := newLatencyObserver("get_allocations")
	allocStubs, _, err := e.client.Allocations().List(&api.QueryOptions{
		AllowStale: true,
		WaitTime:   1 * time.Millisecond,
	})
	o.observe()
	if err != nil {
		return fmt.Errorf("could not get allocations: %s", err)
	}

	var w sync.WaitGroup
	allocationZombies.Set(0)

	for _, allocStub := range allocStubs {
		w.Add(1)

		go func(allocStub api.AllocationListStub) {
			defer w.Done()

			n := nodes[allocStub.NodeID]
			if n == nil {
				logrus.Debugf("Allocation %s doesn't have a node associated. Skipping",
					allocStub.ID)
				allocationZombies.Add(1)
				return
			}

			if !nodes.IsReady(allocStub.NodeID) {
				logrus.Debugf("Skipping fetching allocation %s for node %s because it's not in ready state but %s",
					allocStub.Name, n.Name, n.Status)
				return
			}
			if !validVersion(n.Name, n.Version) {
				logrus.Debugf("Skipping fetching allocation %s for node %s because it's not a supported version but %s",
					allocStub.Name, n.Name, n.Version)
				return
			}
			if allocStub.DesiredStatus != "run" {
				logrus.Debugf("Skipping fetching allocation %s because it's not desired to be run",
					allocStub.Name)
				return
			}
			o = newLatencyObserver("get_allocation_info")
			alloc, _, err := e.client.Allocations().Info(allocStub.ID, &api.QueryOptions{
				AllowStale: true,
				WaitTime:   1 * time.Millisecond,
			})
			o.observe()
			if err != nil {
				logError(err)
				return
			}

			job := alloc.Job

			allocation.With(prometheus.Labels{
				"status":      alloc.ClientStatus,
				"job_type":    *job.Type,
				"job_id":      alloc.JobID,
				"job_version": fmt.Sprintf("%d", *alloc.Job.Version),
				"task_group":  alloc.TaskGroup,
				"node":        n.Name,
			}).Add(1)

			taskStates := alloc.TaskStates

			for _, task := range taskStates {
				taskCount.With(prometheus.Labels{
					"state":    task.State,
					"job_type": *job.Type,
					"node":     n.Name,
				}).Add(1)
			}

			// Return unless the allocation is running
			if allocStub.ClientStatus != "running" {
				return
			}

			no := newNodeLatencyObserver(n.Name, "get_allocation_stats")
			stats, err := e.client.Allocations().Stats(alloc, &api.QueryOptions{
				AllowStale: true,
				WaitTime:   1 * time.Millisecond,
			})
			no.observe()
			if err != nil {
				logError(err)
				return
			}

			allocationLabels := []string{
				*alloc.Job.Name,
				fmt.Sprintf("%d", *alloc.Job.Version),
				alloc.TaskGroup,
				alloc.ID,
				*alloc.Job.Region,
				n.Datacenter,
				n.Name,
			}
			ch <- prometheus.MustNewConstMetric(
				allocationCPUPercent, prometheus.GaugeValue, stats.ResourceUsage.CpuStats.Percent, allocationLabels...,
			)
			ch <- prometheus.MustNewConstMetric(
				allocationCPUThrottled, prometheus.GaugeValue, float64(stats.ResourceUsage.CpuStats.ThrottledTime), allocationLabels...,
			)
			ch <- prometheus.MustNewConstMetric(
				allocationMemoryBytes, prometheus.GaugeValue, float64(stats.ResourceUsage.MemoryStats.RSS), allocationLabels...,
			)
			ch <- prometheus.MustNewConstMetric(
				allocationCPUTicks, prometheus.GaugeValue, stats.ResourceUsage.CpuStats.TotalTicks, allocationLabels...,
			)
			ch <- prometheus.MustNewConstMetric(
				allocationCPUUserMode, prometheus.GaugeValue, stats.ResourceUsage.CpuStats.UserMode, allocationLabels...,
			)
			ch <- prometheus.MustNewConstMetric(
				allocationCPUSystemMode, prometheus.GaugeValue, stats.ResourceUsage.CpuStats.SystemMode, allocationLabels...,
			)

			ch <- prometheus.MustNewConstMetric(
				allocationMemoryBytesRequired, prometheus.GaugeValue, float64(*alloc.Resources.MemoryMB)*1024*1024, allocationLabels...,
			)
			ch <- prometheus.MustNewConstMetric(
				allocationCPURequired, prometheus.GaugeValue, float64(*alloc.Resources.CPU), allocationLabels...,
			)

			for taskName, taskStats := range stats.Tasks {
				taskLabels := append(allocationLabels, taskName)
				ch <- prometheus.MustNewConstMetric(
					taskCPUPercent, prometheus.GaugeValue, taskStats.ResourceUsage.CpuStats.Percent, taskLabels...,
				)
				ch <- prometheus.MustNewConstMetric(
					taskCPUTotalTicks, prometheus.GaugeValue, taskStats.ResourceUsage.CpuStats.TotalTicks, taskLabels...,
				)
				ch <- prometheus.MustNewConstMetric(
					taskMemoryRssBytes, prometheus.GaugeValue, float64(taskStats.ResourceUsage.MemoryStats.RSS), taskLabels...,
				)
			}

		}(*allocStub)
	}

	w.Wait()

	allocation.Collect(ch)
	taskCount.Collect(ch)
	allocationZombies.Collect(ch)
	return nil
}

func (e *Exporter) collectEvalMetrics(ch chan<- prometheus.Metric) error {
	evalCount.Reset()

	if !e.shouldReadMetrics() {
		return nil
	}

	evals, _, err := e.client.Evaluations().List(&api.QueryOptions{
		AllowStale: true,
		WaitTime:   1 * time.Millisecond,
	})
	if err != nil {
		return fmt.Errorf("could not get evaluation metrics: %s", err)
	}

	for _, eval := range evals {
		evalCount.With(prometheus.Labels{
			"status": eval.Status,
		}).Add(1)
	}

	evalCount.Collect(ch)

	return nil
}

func (e *Exporter) collectDeploymentMetrics(ch chan<- prometheus.Metric) error {
	deploymentCount.Reset()
	deploymentTaskGroupDesiredCanaries.Reset()
	deploymentTaskGroupDesiredTotal.Reset()
	deploymentTaskGroupPlacedAllocs.Reset()
	deploymentTaskGroupHealthyAllocs.Reset()
	deploymentTaskGroupUnhealthyAllocs.Reset()

	if !e.shouldReadMetrics() {
		return nil
	}

	deployments, _, err := e.client.Deployments().List(&api.QueryOptions{
		AllowStale: true,
		WaitTime:   1 * time.Millisecond,
	})
	if err != nil {
		return err
	}

	for _, dep := range deployments {
		taskGroups := dep.TaskGroups

		deploymentCount.With(prometheus.Labels{
			"status":      dep.Status,
			"job_id":      dep.JobID,
			"job_version": fmt.Sprintf("%d", dep.JobVersion),
		}).Add(1)

		for taskGroupName, taskGroup := range taskGroups {
			deploymentLabels := []string{
				dep.Status,
				dep.JobID,
				fmt.Sprintf("%d", dep.JobVersion),
				taskGroupName,
				strconv.FormatBool(taskGroup.Promoted),
				strconv.FormatBool(taskGroup.AutoRevert),
			}

			deploymentTaskGroupDesiredCanaries.WithLabelValues(
				deploymentLabels...).Set(float64(taskGroup.DesiredCanaries))
			deploymentTaskGroupDesiredTotal.WithLabelValues(
				deploymentLabels...).Set(float64(taskGroup.DesiredTotal))
			deploymentTaskGroupPlacedAllocs.WithLabelValues(
				deploymentLabels...).Set(float64(taskGroup.PlacedAllocs))
			deploymentTaskGroupHealthyAllocs.WithLabelValues(
				deploymentLabels...).Set(float64(taskGroup.HealthyAllocs))
			deploymentTaskGroupUnhealthyAllocs.WithLabelValues(
				deploymentLabels...).Set(float64(taskGroup.UnhealthyAllocs))
		}
	}

	deploymentCount.Collect(ch)
	deploymentTaskGroupDesiredCanaries.Collect(ch)
	deploymentTaskGroupDesiredTotal.Collect(ch)
	deploymentTaskGroupPlacedAllocs.Collect(ch)
	deploymentTaskGroupHealthyAllocs.Collect(ch)
	deploymentTaskGroupUnhealthyAllocs.Collect(ch)

	return nil
}

func (e Exporter) fetchNodes() (nodeMap, error) {
	o := newLatencyObserver("fetch_nodes")
	nodes, _, err := e.client.Nodes().List(&api.QueryOptions{
		AllowStale: true,
		WaitTime:   1 * time.Millisecond,
	})
	o.observe()
	if err != nil {
		return nil, fmt.Errorf("failed to get nodes list: %s", err)
	}

	m := make(map[string]*api.NodeListStub)
	for _, n := range nodes {
		m[n.ID] = n
	}
	return m, nil
}

// Probe checks that the service can talk to the nomad server
func (e Exporter) Probe() error {
	_, err := e.client.Status().Leader()
	if err != nil {
		return fmt.Errorf("could not collect leader: %s", err)
	}
	return nil
}

type nodeMap map[string]*api.NodeListStub

func (n nodeMap) IsReady(id string) bool {
	node, ok := n[id]
	if !ok {
		return false
	}
	return node.Status == "ready"
}

type nodeList struct {
	nodes    nodeMap
	nodeInfo map[string]*api.Node
}
