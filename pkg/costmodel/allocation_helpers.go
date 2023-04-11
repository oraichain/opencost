package costmodel

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/opencost/opencost/pkg/cloud"
	"github.com/opencost/opencost/pkg/env"
	"github.com/opencost/opencost/pkg/kubecost"
	"github.com/opencost/opencost/pkg/log"
	"github.com/opencost/opencost/pkg/prom"
	"github.com/opencost/opencost/pkg/util/timeutil"
	"k8s.io/apimachinery/pkg/labels"
)

// This is a bit of a hack to work around garbage data from cadvisor
// Ideally you cap each pod to the max CPU on its node, but that involves a bit more complexity, as it it would need to be done when allocations joins with asset data.
const MAX_CPU_CAP = 512

/* Pod Helpers */

func (cm *CostModel) buildPodMap(window kubecost.Window, resolution, maxBatchSize time.Duration, podMap map[podKey]*pod, clusterStart, clusterEnd map[string]time.Time, ingestPodUID bool, podUIDKeyMap map[podKey][]podKey) error {
	// Assumes that window is positive and closed
	start, end := *window.Start(), *window.End()

	// Convert resolution duration to a query-ready string
	resStr := timeutil.DurationString(resolution)

	ctx := prom.NewNamedContext(cm.PrometheusClient, prom.AllocationContextName)

	// Query for (start, end) by (pod, namespace, cluster) over the given
	// window, using the given resolution, and if necessary in batches no
	// larger than the given maximum batch size. If working in batches, track
	// overall progress by starting with (window.start, window.start) and
	// querying in batches no larger than maxBatchSize from start-to-end,
	// folding each result set into podMap as the results come back.
	coverage := kubecost.NewWindow(&start, &start)

	numQuery := 1
	for coverage.End().Before(end) {
		// Determine the (start, end) of the current batch
		batchStart := *coverage.End()
		batchEnd := coverage.End().Add(maxBatchSize)
		if batchEnd.After(end) {
			batchEnd = end
		}

		var resPods []*prom.QueryResult
		var err error
		maxTries := 3
		numTries := 0
		for resPods == nil && numTries < maxTries {
			numTries++

			// Query for the duration between start and end
			durStr := timeutil.DurationString(batchEnd.Sub(batchStart))
			if durStr == "" {
				// Negative duration, so set empty results and don't query
				resPods = []*prom.QueryResult{}
				err = nil
				break
			}

			// Submit and profile query

			var queryPods string
			// If ingesting UIDs, avg on them
			if ingestPodUID {
				queryPods = fmt.Sprintf(queryFmtPodsUID, env.GetPromClusterLabel(), durStr, resStr)
			} else {
				queryPods = fmt.Sprintf(queryFmtPods, env.GetPromClusterLabel(), durStr, resStr)
			}

			queryProfile := time.Now()
			resPods, err = ctx.QueryAtTime(queryPods, batchEnd).Await()
			if err != nil {
				log.Profile(queryProfile, fmt.Sprintf("CostModel.ComputeAllocation: pod query %d try %d failed: %s", numQuery, numTries, queryPods))
				resPods = nil
			}
		}

		if err != nil {
			return err
		}

		// queryFmtPodsUID will return both UID-containing results, and non-UID-containing results,
		// so filter out the non-containing results so we don't duplicate pods. This is due to the
		// default setup of Kubecost having replicated kube_pod_container_status_running and
		// included KSM kube_pod_container_status_running. Querying w/ UID will return both.
		if ingestPodUID {
			var resPodsUID []*prom.QueryResult

			for _, res := range resPods {
				_, err := res.GetString("uid")
				if err == nil {
					resPodsUID = append(resPodsUID, res)
				}
			}

			if len(resPodsUID) > 0 {
				resPods = resPodsUID
			} else {
				log.DedupedWarningf(5, "CostModel.ComputeAllocation: UID ingestion enabled, but query did not return any results with UID")
			}
		}

		applyPodResults(window, resolution, podMap, clusterStart, clusterEnd, resPods, ingestPodUID, podUIDKeyMap)

		coverage = coverage.ExpandEnd(batchEnd)
		numQuery++
	}

	return nil
}

func applyPodResults(window kubecost.Window, resolution time.Duration, podMap map[podKey]*pod, clusterStart, clusterEnd map[string]time.Time, resPods []*prom.QueryResult, ingestPodUID bool, podUIDKeyMap map[podKey][]podKey) {
	for _, res := range resPods {
		if len(res.Values) == 0 {
			log.Warnf("CostModel.ComputeAllocation: empty minutes result")
			continue
		}

		cluster, err := res.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		labels, err := res.GetStrings("namespace", "pod")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: minutes query result missing field: %s", err)
			continue
		}

		namespace := labels["namespace"]
		podName := labels["pod"]
		key := newPodKey(cluster, namespace, podName)

		// If thisPod UIDs are being used to ID pods, append them to the thisPod name in
		// the podKey.
		if ingestPodUID {

			uid, err := res.GetString("uid")
			if err != nil {
				log.Warnf("CostModel.ComputeAllocation: UID ingestion enabled, but query result missing field: %s", err)
			} else {

				newKey := newPodKey(cluster, namespace, podName+" "+uid)
				podUIDKeyMap[key] = append(podUIDKeyMap[key], newKey)

				key = newKey

			}

		}

		allocStart, allocEnd := calculateStartEndFromIsRunning(res, resolution, window)
		if allocStart.IsZero() || allocEnd.IsZero() {
			continue
		}

		// Set start if unset or this datum's start time is earlier than the
		// current earliest time.
		if _, ok := clusterStart[cluster]; !ok || allocStart.Before(clusterStart[cluster]) {
			clusterStart[cluster] = allocStart
		}

		// Set end if unset or this datum's end time is later than the
		// current latest time.
		if _, ok := clusterEnd[cluster]; !ok || allocEnd.After(clusterEnd[cluster]) {
			clusterEnd[cluster] = allocEnd
		}

		if thisPod, ok := podMap[key]; ok {
			// Pod has already been recorded, so update it accordingly
			if allocStart.Before(thisPod.Start) {
				thisPod.Start = allocStart
			}
			if allocEnd.After(thisPod.End) {
				thisPod.End = allocEnd
			}
		} else {
			// pod has not been recorded yet, so insert it
			podMap[key] = &pod{
				Window:      window.Clone(),
				Start:       allocStart,
				End:         allocEnd,
				Key:         key,
				Allocations: map[string]*kubecost.Allocation{},
			}
		}
	}
}

func applyCPUCoresAllocated(podMap map[podKey]*pod, resCPUCoresAllocated []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	for _, res := range resCPUCoresAllocated {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: CPU allocation result missing field: %s", err)
			continue
		}

		container, err := res.GetString("container")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: CPU allocation query result missing 'container': %s", key)
			continue
		}

		var pods []*pod
		if thisPod, ok := podMap[key]; !ok {
			if uidKeys, ok := podUIDKeyMap[key]; ok {
				for _, uidKey := range uidKeys {
					thisPod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, thisPod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*pod{thisPod}
		}

		for _, thisPod := range pods {

			if _, ok := thisPod.Allocations[container]; !ok {
				thisPod.appendContainer(container)
			}

			cpuCores := res.Values[0].Value
			if cpuCores > MAX_CPU_CAP {
				log.Infof("[WARNING] Very large cpu allocation, clamping to %f", res.Values[0].Value*(thisPod.Allocations[container].Minutes()/60.0))
				cpuCores = 0.0
			}
			hours := thisPod.Allocations[container].Minutes() / 60.0
			thisPod.Allocations[container].CPUCoreHours = cpuCores * hours

			node, err := res.GetString("node")
			if err != nil {
				log.Warnf("CostModel.ComputeAllocation: CPU allocation query result missing 'node': %s", key)
				continue
			}
			thisPod.Allocations[container].Properties.Node = node
			thisPod.Node = node
		}
	}
}

func applyCPUCoresRequested(podMap map[podKey]*pod, resCPUCoresRequested []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	for _, res := range resCPUCoresRequested {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: CPU request result missing field: %s", err)
			continue
		}

		container, err := res.GetString("container")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: CPU request query result missing 'container': %s", key)
			continue
		}

		var pods []*pod
		if thisPod, ok := podMap[key]; !ok {
			if uidKeys, ok := podUIDKeyMap[key]; ok {
				for _, uidKey := range uidKeys {
					thisPod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, thisPod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*pod{thisPod}
		}

		for _, thisPod := range pods {

			if _, ok := thisPod.Allocations[container]; !ok {
				thisPod.appendContainer(container)
			}

			thisPod.Allocations[container].CPUCoreRequestAverage = res.Values[0].Value

			// If CPU allocation is less than requests, set CPUCoreHours to
			// request level.
			if thisPod.Allocations[container].CPUCores() < res.Values[0].Value {
				thisPod.Allocations[container].CPUCoreHours = res.Values[0].Value * (thisPod.Allocations[container].Minutes() / 60.0)
			}
			if thisPod.Allocations[container].CPUCores() > MAX_CPU_CAP {
				log.Infof("[WARNING] Very large cpu allocation, clamping! to %f", res.Values[0].Value*(thisPod.Allocations[container].Minutes()/60.0))
				thisPod.Allocations[container].CPUCoreHours = res.Values[0].Value * (thisPod.Allocations[container].Minutes() / 60.0)
			}
			node, err := res.GetString("node")
			if err != nil {
				log.Warnf("CostModel.ComputeAllocation: CPU request query result missing 'node': %s", key)
				continue
			}
			thisPod.Allocations[container].Properties.Node = node
			thisPod.Node = node
		}
	}
}

func applyCPUCoresUsedAvg(podMap map[podKey]*pod, resCPUCoresUsedAvg []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	for _, res := range resCPUCoresUsedAvg {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: CPU usage avg result missing field: %s", err)
			continue
		}

		container, err := res.GetString("container")
		if container == "" || err != nil {
			container, err = res.GetString("container_name")
			if err != nil {
				log.DedupedWarningf(10, "CostModel.ComputeAllocation: CPU usage avg query result missing 'container': %s", key)
				continue
			}
		}

		var pods []*pod
		if thisPod, ok := podMap[key]; !ok {
			if uidKeys, ok := podUIDKeyMap[key]; ok {
				for _, uidKey := range uidKeys {
					thisPod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, thisPod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*pod{thisPod}
		}

		for _, thisPod := range pods {

			if _, ok := thisPod.Allocations[container]; !ok {
				thisPod.appendContainer(container)
			}

			thisPod.Allocations[container].CPUCoreUsageAverage = res.Values[0].Value
			if res.Values[0].Value > MAX_CPU_CAP {
				log.Infof("[WARNING] Very large cpu USAGE, dropping outlier")
				thisPod.Allocations[container].CPUCoreUsageAverage = 0.0
			}
		}
	}
}

func applyCPUCoresUsedMax(podMap map[podKey]*pod, resCPUCoresUsedMax []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	for _, res := range resCPUCoresUsedMax {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: CPU usage max result missing field: %s", err)
			continue
		}

		container, err := res.GetString("container")
		if container == "" || err != nil {
			container, err = res.GetString("container_name")
			if err != nil {
				log.DedupedWarningf(10, "CostModel.ComputeAllocation: CPU usage max query result missing 'container': %s", key)
				continue
			}
		}

		var pods []*pod
		if thisPod, ok := podMap[key]; !ok {
			if uidKeys, ok := podUIDKeyMap[key]; ok {
				for _, uidKey := range uidKeys {
					thisPod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, thisPod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*pod{thisPod}
		}

		for _, thisPod := range pods {

			if _, ok := thisPod.Allocations[container]; !ok {
				thisPod.appendContainer(container)
			}

			if thisPod.Allocations[container].RawAllocationOnly == nil {
				thisPod.Allocations[container].RawAllocationOnly = &kubecost.RawAllocationOnlyData{
					CPUCoreUsageMax: res.Values[0].Value,
				}
			} else {
				thisPod.Allocations[container].RawAllocationOnly.CPUCoreUsageMax = res.Values[0].Value
			}
		}
	}
}

func applyRAMBytesAllocated(podMap map[podKey]*pod, resRAMBytesAllocated []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	for _, res := range resRAMBytesAllocated {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: RAM allocation result missing field: %s", err)
			continue
		}

		container, err := res.GetString("container")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: RAM allocation query result missing 'container': %s", key)
			continue
		}

		var pods []*pod
		if thisPod, ok := podMap[key]; !ok {
			if uidKeys, ok := podUIDKeyMap[key]; ok {
				for _, uidKey := range uidKeys {
					thisPod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, thisPod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*pod{thisPod}
		}

		for _, thisPod := range pods {

			if _, ok := thisPod.Allocations[container]; !ok {
				thisPod.appendContainer(container)
			}

			ramBytes := res.Values[0].Value
			hours := thisPod.Allocations[container].Minutes() / 60.0
			thisPod.Allocations[container].RAMByteHours = ramBytes * hours

			node, err := res.GetString("node")
			if err != nil {
				log.Warnf("CostModel.ComputeAllocation: RAM allocation query result missing 'node': %s", key)
				continue
			}
			thisPod.Allocations[container].Properties.Node = node
			thisPod.Node = node
		}
	}
}

func applyRAMBytesRequested(podMap map[podKey]*pod, resRAMBytesRequested []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	for _, res := range resRAMBytesRequested {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: RAM request result missing field: %s", err)
			continue
		}

		container, err := res.GetString("container")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: RAM request query result missing 'container': %s", key)
			continue
		}

		var pods []*pod
		if thisPod, ok := podMap[key]; !ok {
			if uidKeys, ok := podUIDKeyMap[key]; ok {
				for _, uidKey := range uidKeys {
					thisPod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, thisPod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*pod{thisPod}
		}

		for _, pod := range pods {

			if _, ok := pod.Allocations[container]; !ok {
				pod.appendContainer(container)
			}

			pod.Allocations[container].RAMBytesRequestAverage = res.Values[0].Value

			// If RAM allocation is less than requests, set RAMByteHours to
			// request level.
			if pod.Allocations[container].RAMBytes() < res.Values[0].Value {
				pod.Allocations[container].RAMByteHours = res.Values[0].Value * (pod.Allocations[container].Minutes() / 60.0)
			}

			node, err := res.GetString("node")
			if err != nil {
				log.Warnf("CostModel.ComputeAllocation: RAM request query result missing 'node': %s", key)
				continue
			}
			pod.Allocations[container].Properties.Node = node
			pod.Node = node
		}
	}
}

func applyRAMBytesUsedAvg(podMap map[podKey]*pod, resRAMBytesUsedAvg []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	for _, res := range resRAMBytesUsedAvg {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: RAM avg usage result missing field: %s", err)
			continue
		}

		container, err := res.GetString("container")
		if container == "" || err != nil {
			container, err = res.GetString("container_name")
			if err != nil {
				log.DedupedWarningf(10, "CostModel.ComputeAllocation: RAM usage avg query result missing 'container': %s", key)
				continue
			}
		}

		var pods []*pod
		if thisPod, ok := podMap[key]; !ok {
			if uidKeys, ok := podUIDKeyMap[key]; ok {
				for _, uidKey := range uidKeys {
					thisPod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, thisPod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*pod{thisPod}
		}

		for _, thisPod := range pods {

			if _, ok := thisPod.Allocations[container]; !ok {
				thisPod.appendContainer(container)
			}

			thisPod.Allocations[container].RAMBytesUsageAverage = res.Values[0].Value
		}
	}
}

func applyRAMBytesUsedMax(podMap map[podKey]*pod, resRAMBytesUsedMax []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	for _, res := range resRAMBytesUsedMax {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: RAM usage max result missing field: %s", err)
			continue
		}

		container, err := res.GetString("container")
		if container == "" || err != nil {
			container, err = res.GetString("container_name")
			if err != nil {
				log.DedupedWarningf(10, "CostModel.ComputeAllocation: RAM usage max query result missing 'container': %s", key)
				continue
			}
		}

		var pods []*pod
		if thisPod, ok := podMap[key]; !ok {
			if uidKeys, ok := podUIDKeyMap[key]; ok {
				for _, uidKey := range uidKeys {
					thisPod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, thisPod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*pod{thisPod}
		}

		for _, thisPod := range pods {

			if _, ok := thisPod.Allocations[container]; !ok {
				thisPod.appendContainer(container)
			}

			if thisPod.Allocations[container].RawAllocationOnly == nil {
				thisPod.Allocations[container].RawAllocationOnly = &kubecost.RawAllocationOnlyData{
					RAMBytesUsageMax: res.Values[0].Value,
				}
			} else {
				thisPod.Allocations[container].RawAllocationOnly.RAMBytesUsageMax = res.Values[0].Value
			}
		}
	}
}

func applyGPUsAllocated(podMap map[podKey]*pod, resGPUsRequested []*prom.QueryResult, resGPUsAllocated []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	if len(resGPUsAllocated) > 0 { // Use the new query, when it's become available in a window
		resGPUsRequested = resGPUsAllocated
	}
	for _, res := range resGPUsRequested {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: GPU request result missing field: %s", err)
			continue
		}

		container, err := res.GetString("container")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: GPU request query result missing 'container': %s", key)
			continue
		}

		var pods []*pod
		if thisPod, ok := podMap[key]; !ok {
			if uidKeys, ok := podUIDKeyMap[key]; ok {
				for _, uidKey := range uidKeys {
					thisPod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, thisPod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*pod{thisPod}
		}

		for _, thisPod := range pods {

			if _, ok := thisPod.Allocations[container]; !ok {
				thisPod.appendContainer(container)
			}

			hrs := thisPod.Allocations[container].Minutes() / 60.0
			thisPod.Allocations[container].GPUHours = res.Values[0].Value * hrs
		}
	}
}

func applyNetworkTotals(podMap map[podKey]*pod, resNetworkTransferBytes []*prom.QueryResult, resNetworkReceiveBytes []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	for _, res := range resNetworkTransferBytes {
		podKey, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: Network Transfer Bytes query result missing field: %s", err)
			continue
		}

		var pods []*pod

		if thisPod, ok := podMap[podKey]; !ok {
			if uidKeys, ok := podUIDKeyMap[podKey]; ok {
				for _, uidKey := range uidKeys {
					thisPod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, thisPod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*pod{thisPod}
		}

		for _, thisPod := range pods {
			for _, alloc := range thisPod.Allocations {
				alloc.NetworkTransferBytes = res.Values[0].Value / float64(len(thisPod.Allocations)) / float64(len(pods))
			}
		}
	}
	for _, res := range resNetworkReceiveBytes {
		podKey, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: Network Receive Bytes query result missing field: %s", err)
			continue
		}

		var pods []*pod

		if thisPod, ok := podMap[podKey]; !ok {
			if uidKeys, ok := podUIDKeyMap[podKey]; ok {
				for _, uidKey := range uidKeys {
					thisPod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, thisPod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*pod{thisPod}
		}

		for _, thisPod := range pods {
			for _, alloc := range thisPod.Allocations {
				alloc.NetworkReceiveBytes = res.Values[0].Value / float64(len(thisPod.Allocations)) / float64(len(pods))
			}
		}
	}
}

func applyNetworkAllocation(podMap map[podKey]*pod, resNetworkGiB []*prom.QueryResult, resNetworkCostPerGiB []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey, networkCostSubType string) {
	costPerGiBByCluster := map[string]float64{}

	for _, res := range resNetworkCostPerGiB {
		cluster, err := res.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		costPerGiBByCluster[cluster] = res.Values[0].Value
	}

	for _, res := range resNetworkGiB {
		podKey, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: Network allocation query result missing field: %s", err)
			continue
		}

		var pods []*pod

		if thisPod, ok := podMap[podKey]; !ok {
			if uidKeys, ok := podUIDKeyMap[podKey]; ok {
				for _, uidKey := range uidKeys {
					thisPod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, thisPod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*pod{thisPod}
		}

		for _, thisPod := range pods {
			for _, alloc := range thisPod.Allocations {
				gib := res.Values[0].Value / float64(len(thisPod.Allocations))
				costPerGiB := costPerGiBByCluster[podKey.Cluster]
				currentNetworkSubCost := gib * costPerGiB / float64(len(pods))
				switch networkCostSubType {
				case networkCrossZoneCost:
					alloc.NetworkCrossZoneCost = currentNetworkSubCost
				case networkCrossRegionCost:
					alloc.NetworkCrossRegionCost = currentNetworkSubCost
				case networkInternetCost:
					alloc.NetworkInternetCost = currentNetworkSubCost
				default:
					log.Warnf("CostModel.applyNetworkAllocation: unknown network subtype passed to the function: %s", networkCostSubType)
				}
				alloc.NetworkCost += currentNetworkSubCost
			}
		}
	}
}

func resToNodeLabels(resNodeLabels []*prom.QueryResult) map[nodeKey]map[string]string {
	nodeLabels := map[nodeKey]map[string]string{}

	for _, res := range resNodeLabels {
		nodeKey, err := resultNodeKey(res, env.GetPromClusterLabel(), "node")
		if err != nil {
			continue
		}

		if _, ok := nodeLabels[nodeKey]; !ok {
			nodeLabels[nodeKey] = map[string]string{}
		}

		for _, rawK := range env.GetAllocationNodeLabelsIncludeList() {
			labels := res.GetLabels()

			// Sanitize the given label name to match Prometheus formatting
			// e.g. topology.kubernetes.io/zone => topology_kubernetes_io_zone
			k := prom.SanitizeLabelName(rawK)
			if v, ok := labels[k]; ok {
				nodeLabels[nodeKey][k] = v
				continue
			}

			// Try with the "label_" prefix, if not found
			// e.g. topology_kubernetes_io_zone => label_topology_kubernetes_io_zone
			k = fmt.Sprintf("label_%s", k)
			if v, ok := labels[k]; ok {
				nodeLabels[nodeKey][k] = v
			}
		}
	}

	return nodeLabels
}

func resToNamespaceLabels(resNamespaceLabels []*prom.QueryResult) map[namespaceKey]map[string]string {
	namespaceLabels := map[namespaceKey]map[string]string{}

	for _, res := range resNamespaceLabels {
		nsKey, err := resultNamespaceKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			continue
		}

		if _, ok := namespaceLabels[nsKey]; !ok {
			namespaceLabels[nsKey] = map[string]string{}
		}

		for k, l := range res.GetLabels() {
			namespaceLabels[nsKey][k] = l
		}
	}

	return namespaceLabels
}

func resToPodLabels(resPodLabels []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey, ingestPodUID bool) map[podKey]map[string]string {
	podLabels := map[podKey]map[string]string{}

	for _, res := range resPodLabels {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			continue
		}

		var keys []podKey

		if ingestPodUID {
			if uidKeys, ok := podUIDKeyMap[key]; ok {

				keys = append(keys, uidKeys...)

			}
		} else {
			keys = []podKey{key}
		}

		for _, key := range keys {
			if _, ok := podLabels[key]; !ok {
				podLabels[key] = map[string]string{}
			}

			for k, l := range res.GetLabels() {
				podLabels[key][k] = l
			}
		}
	}

	return podLabels
}

func resToNamespaceAnnotations(resNamespaceAnnotations []*prom.QueryResult) map[string]map[string]string {
	namespaceAnnotations := map[string]map[string]string{}

	for _, res := range resNamespaceAnnotations {
		namespace, err := res.GetString("namespace")
		if err != nil {
			continue
		}

		if _, ok := namespaceAnnotations[namespace]; !ok {
			namespaceAnnotations[namespace] = map[string]string{}
		}

		for k, l := range res.GetAnnotations() {
			namespaceAnnotations[namespace][k] = l
		}
	}

	return namespaceAnnotations
}

func resToPodAnnotations(resPodAnnotations []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey, ingestPodUID bool) map[podKey]map[string]string {
	podAnnotations := map[podKey]map[string]string{}

	for _, res := range resPodAnnotations {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			continue
		}

		var keys []podKey

		if ingestPodUID {
			if uidKeys, ok := podUIDKeyMap[key]; ok {

				keys = append(keys, uidKeys...)

			}
		} else {
			keys = []podKey{key}
		}

		for _, key := range keys {
			if _, ok := podAnnotations[key]; !ok {
				podAnnotations[key] = map[string]string{}
			}

			for k, l := range res.GetAnnotations() {
				podAnnotations[key][k] = l
			}
		}
	}

	return podAnnotations
}

func applyLabels(podMap map[podKey]*pod, nodeLabels map[nodeKey]map[string]string, namespaceLabels map[namespaceKey]map[string]string, podLabels map[podKey]map[string]string) {
	for podKey, pod := range podMap {
		for _, alloc := range pod.Allocations {
			allocLabels := alloc.Properties.Labels
			if allocLabels == nil {
				allocLabels = make(map[string]string)
			}

			// Apply node labels first, then namespace labels, then pod labels
			// so that pod labels overwrite namespace labels, which overwrite
			// node labels.

			if nodeLabels != nil {
				nodeKey := newNodeKey(pod.Key.Cluster, pod.Node)
				if labels, ok := nodeLabels[nodeKey]; ok {
					for k, v := range labels {
						allocLabels[k] = v
					}
				}
			}

			nsKey := podKey.namespaceKey
			if labels, ok := namespaceLabels[nsKey]; ok {
				for k, v := range labels {
					allocLabels[k] = v
				}
			}

			if labels, ok := podLabels[podKey]; ok {
				for k, v := range labels {
					allocLabels[k] = v
				}
			}

			alloc.Properties.Labels = allocLabels
		}
	}
}

func applyAnnotations(podMap map[podKey]*pod, namespaceAnnotations map[string]map[string]string, podAnnotations map[podKey]map[string]string) {
	for key, pod := range podMap {
		for _, alloc := range pod.Allocations {
			allocAnnotations := alloc.Properties.Annotations
			if allocAnnotations == nil {
				allocAnnotations = make(map[string]string)
			}
			// Apply namespace annotations first, then pod annotations so that
			// pod labels overwrite namespace labels.
			if labels, ok := namespaceAnnotations[key.Namespace]; ok {
				for k, v := range labels {
					allocAnnotations[k] = v
				}
			}
			if labels, ok := podAnnotations[key]; ok {
				for k, v := range labels {
					allocAnnotations[k] = v
				}
			}

			alloc.Properties.Annotations = allocAnnotations
		}
	}
}

func resToDeploymentLabels(resDeploymentLabels []*prom.QueryResult) map[controllerKey]map[string]string {
	deploymentLabels := map[controllerKey]map[string]string{}

	for _, res := range resDeploymentLabels {
		controllerKey, err := resultDeploymentKey(res, env.GetPromClusterLabel(), "namespace", "deployment")
		if err != nil {
			continue
		}

		if _, ok := deploymentLabels[controllerKey]; !ok {
			deploymentLabels[controllerKey] = map[string]string{}
		}

		for k, l := range res.GetLabels() {
			deploymentLabels[controllerKey][k] = l
		}
	}

	// Prune duplicate deployments. That is, if the same deployment exists with
	// hyphens instead of underscores, keep the one that uses hyphens.
	for key := range deploymentLabels {
		if strings.Contains(key.Controller, "_") {
			duplicateController := strings.Replace(key.Controller, "_", "-", -1)
			duplicateKey := newControllerKey(key.Cluster, key.Namespace, key.ControllerKind, duplicateController)
			if _, ok := deploymentLabels[duplicateKey]; ok {
				delete(deploymentLabels, key)
			}
		}
	}

	return deploymentLabels
}

func resToStatefulSetLabels(resStatefulSetLabels []*prom.QueryResult) map[controllerKey]map[string]string {
	statefulSetLabels := map[controllerKey]map[string]string{}

	for _, res := range resStatefulSetLabels {
		controllerKey, err := resultStatefulSetKey(res, env.GetPromClusterLabel(), "namespace", "statefulSet")
		if err != nil {
			continue
		}

		if _, ok := statefulSetLabels[controllerKey]; !ok {
			statefulSetLabels[controllerKey] = map[string]string{}
		}

		for k, l := range res.GetLabels() {
			statefulSetLabels[controllerKey][k] = l
		}
	}

	// Prune duplicate stateful sets. That is, if the same stateful set exists
	// with hyphens instead of underscores, keep the one that uses hyphens.
	for key := range statefulSetLabels {
		if strings.Contains(key.Controller, "_") {
			duplicateController := strings.Replace(key.Controller, "_", "-", -1)
			duplicateKey := newControllerKey(key.Cluster, key.Namespace, key.ControllerKind, duplicateController)
			if _, ok := statefulSetLabels[duplicateKey]; ok {
				delete(statefulSetLabels, key)
			}
		}
	}

	return statefulSetLabels
}

func labelsToPodControllerMap(podLabels map[podKey]map[string]string, controllerLabels map[controllerKey]map[string]string) map[podKey]controllerKey {
	podControllerMap := map[podKey]controllerKey{}

	// For each controller, turn the labels into a selector and attempt to
	// match it with each set of pod labels. A match indicates that the pod
	// belongs to the controller.
	for cKey, cLabels := range controllerLabels {
		selector := labels.Set(cLabels).AsSelectorPreValidated()

		for pKey, pLabels := range podLabels {
			// If the pod is in a different cluster or namespace, there is
			// no need to compare the labels.
			if cKey.Cluster != pKey.Cluster || cKey.Namespace != pKey.Namespace {
				continue
			}

			podLabelSet := labels.Set(pLabels)
			if selector.Matches(podLabelSet) {
				if _, ok := podControllerMap[pKey]; ok {
					log.DedupedWarningf(5, "CostModel.ComputeAllocation: PodControllerMap match already exists: %s matches %s and %s", pKey, podControllerMap[pKey], cKey)
				}
				podControllerMap[pKey] = cKey
			}
		}
	}

	return podControllerMap
}

func resToPodDaemonSetMap(resDaemonSetLabels []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey, ingestPodUID bool) map[podKey]controllerKey {
	daemonSetLabels := map[podKey]controllerKey{}

	for _, res := range resDaemonSetLabels {
		controllerKey, err := resultDaemonSetKey(res, env.GetPromClusterLabel(), "namespace", "owner_name")
		if err != nil {
			continue
		}

		pod, err := res.GetString("pod")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: DaemonSetLabel result without pod: %s", controllerKey)
		}

		key := newPodKey(controllerKey.Cluster, controllerKey.Namespace, pod)

		var keys []podKey

		if ingestPodUID {
			if uidKeys, ok := podUIDKeyMap[key]; ok {

				keys = append(keys, uidKeys...)

			}
		} else {
			keys = []podKey{key}
		}

		for _, key := range keys {
			daemonSetLabels[key] = controllerKey
		}
	}

	return daemonSetLabels
}

func resToPodJobMap(resJobLabels []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey, ingestPodUID bool) map[podKey]controllerKey {
	jobLabels := map[podKey]controllerKey{}

	for _, res := range resJobLabels {
		controllerKey, err := resultJobKey(res, env.GetPromClusterLabel(), "namespace", "owner_name")
		if err != nil {
			continue
		}

		// Convert the name of Jobs generated by CronJobs to the name of the
		// CronJob by stripping the timestamp off the end.
		match := isCron.FindStringSubmatch(controllerKey.Controller)
		if match != nil {
			controllerKey.Controller = match[1]
		}

		pod, err := res.GetString("pod")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: JobLabel result without pod: %s", controllerKey)
		}

		key := newPodKey(controllerKey.Cluster, controllerKey.Namespace, pod)

		var keys []podKey

		if ingestPodUID {
			if uidKeys, ok := podUIDKeyMap[key]; ok {

				keys = append(keys, uidKeys...)

			}
		} else {
			keys = []podKey{key}
		}

		for _, key := range keys {
			jobLabels[key] = controllerKey
		}
	}

	return jobLabels
}

func resToPodReplicaSetMap(resPodsWithReplicaSetOwner []*prom.QueryResult, resReplicaSetsWithoutOwners []*prom.QueryResult, resReplicaSetsWithRolloutOwner []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey, ingestPodUID bool) map[podKey]controllerKey {
	// Build out set of ReplicaSets that have no owners, themselves, such that
	// the ReplicaSet should be used as the owner of the Pods it controls.
	// (This should exclude, for example, ReplicaSets that are controlled by
	// Deployments, in which case the Deployment should be the pod's owner.)
	// Additionally, add to this set of ReplicaSets those ReplicaSets that
	// are owned by a Rollout
	replicaSets := map[controllerKey]struct{}{}

	// Create unowned ReplicaSet controller keys
	for _, res := range resReplicaSetsWithoutOwners {
		controllerKey, err := resultReplicaSetKey(res, env.GetPromClusterLabel(), "namespace", "replicaset")
		if err != nil {
			continue
		}

		replicaSets[controllerKey] = struct{}{}
	}

	// Create Rollout-owned ReplicaSet controller keys
	for _, res := range resReplicaSetsWithRolloutOwner {
		controllerKey, err := resultReplicaSetRolloutKey(res, env.GetPromClusterLabel(), "namespace", "replicaset")
		if err != nil {
			continue
		}

		replicaSets[controllerKey] = struct{}{}
	}

	// Create the mapping of Pods to ReplicaSets, ignoring any ReplicaSets that
	// do not appear in the set of unowned/Rollout-owned ReplicaSets above.
	podToReplicaSet := map[podKey]controllerKey{}

	for _, res := range resPodsWithReplicaSetOwner {
		// First, check if this pod is owned by an unowned ReplicaSet
		controllerKey, err := resultReplicaSetKey(res, env.GetPromClusterLabel(), "namespace", "owner_name")
		if err != nil {
			continue
		} else if _, ok := replicaSets[controllerKey]; !ok {
			// If the pod is not owned by an unowned ReplicaSet, check if
			// it's owned by a Rollout-owned ReplicaSet
			controllerKey, err = resultReplicaSetRolloutKey(res, env.GetPromClusterLabel(), "namespace", "owner_name")
			if err != nil {
				continue
			} else if _, ok := replicaSets[controllerKey]; !ok {
				continue
			}
		}

		pod, err := res.GetString("pod")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: ReplicaSet result without pod: %s", controllerKey)
		}

		key := newPodKey(controllerKey.Cluster, controllerKey.Namespace, pod)

		var keys []podKey

		if ingestPodUID {
			if uidKeys, ok := podUIDKeyMap[key]; ok {
				keys = append(keys, uidKeys...)
			}
		} else {
			keys = []podKey{key}
		}

		for _, key := range keys {
			podToReplicaSet[key] = controllerKey
		}
	}

	return podToReplicaSet
}

func applyControllersToPods(podMap map[podKey]*pod, podControllerMap map[podKey]controllerKey) {
	for key, pod := range podMap {
		for _, alloc := range pod.Allocations {
			if controllerKey, ok := podControllerMap[key]; ok {
				alloc.Properties.ControllerKind = controllerKey.ControllerKind
				alloc.Properties.Controller = controllerKey.Controller
			}
		}
	}
}

/* Service Helpers */

func getServiceLabels(resServiceLabels []*prom.QueryResult) map[serviceKey]map[string]string {
	serviceLabels := map[serviceKey]map[string]string{}

	for _, res := range resServiceLabels {
		serviceKey, err := resultServiceKey(res, env.GetPromClusterLabel(), "namespace", "service")
		if err != nil {
			continue
		}

		if _, ok := serviceLabels[serviceKey]; !ok {
			serviceLabels[serviceKey] = map[string]string{}
		}

		for k, l := range res.GetLabels() {
			serviceLabels[serviceKey][k] = l
		}
	}

	// Prune duplicate services. That is, if the same service exists with
	// hyphens instead of underscores, keep the one that uses hyphens.
	for key := range serviceLabels {
		if strings.Contains(key.Service, "_") {
			duplicateService := strings.Replace(key.Service, "_", "-", -1)
			duplicateKey := newServiceKey(key.Cluster, key.Namespace, duplicateService)
			if _, ok := serviceLabels[duplicateKey]; ok {
				delete(serviceLabels, key)
			}
		}
	}

	return serviceLabels
}

func applyServicesToPods(podMap map[podKey]*pod, podLabels map[podKey]map[string]string, allocsByService map[serviceKey][]*kubecost.Allocation, serviceLabels map[serviceKey]map[string]string) {
	podServicesMap := map[podKey][]serviceKey{}

	// For each service, turn the labels into a selector and attempt to
	// match it with each set of pod labels. A match indicates that the pod
	// belongs to the service.
	for sKey, sLabels := range serviceLabels {
		selector := labels.Set(sLabels).AsSelectorPreValidated()

		for pKey, pLabels := range podLabels {
			// If the pod is in a different cluster or namespace, there is
			// no need to compare the labels.
			if sKey.Cluster != pKey.Cluster || sKey.Namespace != pKey.Namespace {
				continue
			}

			podLabelSet := labels.Set(pLabels)
			if selector.Matches(podLabelSet) {
				if _, ok := podServicesMap[pKey]; !ok {
					podServicesMap[pKey] = []serviceKey{}
				}
				podServicesMap[pKey] = append(podServicesMap[pKey], sKey)
			}
		}
	}

	// For each allocation in each pod, attempt to find and apply the list of
	// services associated with the allocation's pod.
	for key, pod := range podMap {
		for _, alloc := range pod.Allocations {
			if sKeys, ok := podServicesMap[key]; ok {
				services := []string{}
				for _, sKey := range sKeys {
					services = append(services, sKey.Service)
					allocsByService[sKey] = append(allocsByService[sKey], alloc)
				}
				alloc.Properties.Services = services

			}
		}
	}
}

func getLoadBalancerCosts(lbMap map[serviceKey]*lbCost, resLBCost, resLBActiveMins []*prom.QueryResult, resolution time.Duration) {
	for _, res := range resLBActiveMins {
		serviceKey, err := resultServiceKey(res, env.GetPromClusterLabel(), "namespace", "service_name")
		if err != nil || len(res.Values) == 0 {
			continue
		}

		lbStart, lbEnd := calculateStartAndEnd(res, resolution)
		if lbStart.IsZero() || lbEnd.IsZero() {
			log.Warnf("CostModel.ComputeAllocation: pvc %s has no running time", serviceKey)
		}

		lbMap[serviceKey] = &lbCost{
			Start: lbStart,
			End:   lbEnd,
		}
	}

	for _, res := range resLBCost {
		serviceKey, err := resultServiceKey(res, env.GetPromClusterLabel(), "namespace", "service_name")
		if err != nil {
			continue
		}
		// Apply cost as price-per-hour * hours
		if lb, ok := lbMap[serviceKey]; ok {
			lbPricePerHr := res.Values[0].Value
			hours := lb.End.Sub(lb.Start).Hours()
			lb.TotalCost += lbPricePerHr * hours
		} else {
			log.DedupedWarningf(20, "CostModel: found minutes for key that does not exist: %s", serviceKey)
		}
	}
}

func applyLoadBalancersToPods(window kubecost.Window, podMap map[podKey]*pod, lbMap map[serviceKey]*lbCost, allocsByService map[serviceKey][]*kubecost.Allocation) {
	for sKey, lb := range lbMap {
		totalHours := 0.0
		allocHours := make(map[*kubecost.Allocation]float64)

		allocs, ok := allocsByService[sKey]
		// if there are no allocations using the service, add its cost to the Unmounted pod for its cluster
		if !ok {
			pod := getUnmountedPodForCluster(window, podMap, sKey.Cluster)
			pod.Allocations[kubecost.UnmountedSuffix].LoadBalancerCost += lb.TotalCost
			pod.Allocations[kubecost.UnmountedSuffix].Properties.Services = append(pod.Allocations[kubecost.UnmountedSuffix].Properties.Services, sKey.Service)
		}
		// Add portion of load balancing cost to each allocation
		// proportional to the total number of hours allocations used the load balancer
		for _, alloc := range allocs {
			// Determine the (start, end) of the relationship between the
			// given lbCost and the associated Allocation so that a precise
			// number of hours can be used to compute cumulative cost.
			s, e := alloc.Start, alloc.End
			if lb.Start.After(alloc.Start) {
				s = lb.Start
			}
			if lb.End.Before(alloc.End) {
				e = lb.End
			}
			hours := e.Sub(s).Hours()
			// A negative number of hours signifies no overlap between the windows
			if hours > 0 {
				totalHours += hours
				allocHours[alloc] = hours
			}
		}

		// Distribute cost of service once total hours is calculated
		for alloc, hours := range allocHours {
			alloc.LoadBalancerCost += lb.TotalCost * hours / totalHours
		}

		// If there was no overlap apply to Unmounted pod
		if len(allocHours) == 0 {
			pod := getUnmountedPodForCluster(window, podMap, sKey.Cluster)
			pod.Allocations[kubecost.UnmountedSuffix].LoadBalancerCost += lb.TotalCost
			pod.Allocations[kubecost.UnmountedSuffix].Properties.Services = append(pod.Allocations[kubecost.UnmountedSuffix].Properties.Services, sKey.Service)
		}
	}
}

/* Node Helpers */

func applyNodeCostPerCPUHr(nodeMap map[nodeKey]*nodePricing, resNodeCostPerCPUHr []*prom.QueryResult) {
	for _, res := range resNodeCostPerCPUHr {
		cluster, err := res.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		node, err := res.GetString("node")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: Node CPU cost query result missing field: %s", err)
			continue
		}

		instanceType, err := res.GetString("instance_type")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: Node CPU cost query result missing field: %s", err)
			continue
		}

		providerID, err := res.GetString("provider_id")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: Node CPU cost query result missing field: %s", err)
			continue
		}

		key := newNodeKey(cluster, node)
		if _, ok := nodeMap[key]; !ok {
			nodeMap[key] = &nodePricing{
				Name:       node,
				NodeType:   instanceType,
				ProviderID: cloud.ParseID(providerID),
			}
		}

		nodeMap[key].CostPerCPUHr = res.Values[0].Value
	}
}

func applyNodeCostPerRAMGiBHr(nodeMap map[nodeKey]*nodePricing, resNodeCostPerRAMGiBHr []*prom.QueryResult) {
	for _, res := range resNodeCostPerRAMGiBHr {
		cluster, err := res.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		node, err := res.GetString("node")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: Node RAM cost query result missing field: %s", err)
			continue
		}

		instanceType, err := res.GetString("instance_type")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: Node RAM cost query result missing field: %s", err)
			continue
		}

		providerID, err := res.GetString("provider_id")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: Node RAM cost query result missing field: %s", err)
			continue
		}

		key := newNodeKey(cluster, node)
		if _, ok := nodeMap[key]; !ok {
			nodeMap[key] = &nodePricing{
				Name:       node,
				NodeType:   instanceType,
				ProviderID: cloud.ParseID(providerID),
			}
		}

		nodeMap[key].CostPerRAMGiBHr = res.Values[0].Value
	}
}

func applyNodeCostPerGPUHr(nodeMap map[nodeKey]*nodePricing, resNodeCostPerGPUHr []*prom.QueryResult) {
	for _, res := range resNodeCostPerGPUHr {
		cluster, err := res.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		node, err := res.GetString("node")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: Node GPU cost query result missing field: %s", err)
			continue
		}

		instanceType, err := res.GetString("instance_type")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: Node GPU cost query result missing field: %s", err)
			continue
		}

		providerID, err := res.GetString("provider_id")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: Node GPU cost query result missing field: %s", err)
			continue
		}

		key := newNodeKey(cluster, node)
		if _, ok := nodeMap[key]; !ok {
			nodeMap[key] = &nodePricing{
				Name:       node,
				NodeType:   instanceType,
				ProviderID: cloud.ParseID(providerID),
			}
		}

		nodeMap[key].CostPerGPUHr = res.Values[0].Value
	}
}

func applyNodeSpot(nodeMap map[nodeKey]*nodePricing, resNodeIsSpot []*prom.QueryResult) {
	for _, res := range resNodeIsSpot {
		cluster, err := res.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		node, err := res.GetString("node")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: Node spot query result missing field: %s", err)
			continue
		}

		key := newNodeKey(cluster, node)
		if _, ok := nodeMap[key]; !ok {
			log.Warnf("CostModel.ComputeAllocation: Node spot  query result for missing node: %s", key)
			continue
		}

		nodeMap[key].Preemptible = res.Values[0].Value > 0
	}
}

func applyNodeDiscount(nodeMap map[nodeKey]*nodePricing, cm *CostModel) {
	if cm == nil {
		return
	}

	c, err := cm.Provider.GetConfig()
	if err != nil {
		log.Errorf("CostModel.ComputeAllocation: applyNodeDiscount: %s", err)
		return
	}

	discount, err := ParsePercentString(c.Discount)
	if err != nil {
		log.Errorf("CostModel.ComputeAllocation: applyNodeDiscount: %s", err)
		return
	}

	negotiatedDiscount, err := ParsePercentString(c.NegotiatedDiscount)
	if err != nil {
		log.Errorf("CostModel.ComputeAllocation: applyNodeDiscount: %s", err)
		return
	}

	for _, node := range nodeMap {
		// TODO GKE Reserved Instances into account
		node.Discount = cm.Provider.CombinedDiscountForNode(node.NodeType, node.Preemptible, discount, negotiatedDiscount)
		node.CostPerCPUHr *= (1.0 - node.Discount)
		node.CostPerRAMGiBHr *= (1.0 - node.Discount)
	}
}

func (cm *CostModel) applyNodesToPod(podMap map[podKey]*pod, nodeMap map[nodeKey]*nodePricing) {
	for _, pod := range podMap {
		for _, alloc := range pod.Allocations {
			cluster := alloc.Properties.Cluster
			nodeName := alloc.Properties.Node
			thisNodeKey := newNodeKey(cluster, nodeName)

			node := cm.getNodePricing(nodeMap, thisNodeKey)
			alloc.Properties.ProviderID = node.ProviderID
			alloc.CPUCost = alloc.CPUCoreHours * node.CostPerCPUHr
			alloc.RAMCost = (alloc.RAMByteHours / 1024 / 1024 / 1024) * node.CostPerRAMGiBHr
			alloc.GPUCost = alloc.GPUHours * node.CostPerGPUHr
		}
	}
}

// getCustomNodePricing converts the CostModel's configured custom pricing
// values into a nodePricing instance.
func (cm *CostModel) getCustomNodePricing(spot bool) *nodePricing {
	customPricingConfig, err := cm.Provider.GetConfig()
	if err != nil {
		return nil
	}

	cpuCostStr := customPricingConfig.CPU
	gpuCostStr := customPricingConfig.GPU
	ramCostStr := customPricingConfig.RAM
	if spot {
		cpuCostStr = customPricingConfig.SpotCPU
		gpuCostStr = customPricingConfig.SpotGPU
		ramCostStr = customPricingConfig.SpotRAM
	}

	node := &nodePricing{Source: "custom"}

	costPerCPUHr, err := strconv.ParseFloat(cpuCostStr, 64)
	if err != nil {
		log.Warnf("CostModel: custom pricing has illegal CPU cost: %s", cpuCostStr)
	}
	node.CostPerCPUHr = costPerCPUHr

	costPerGPUHr, err := strconv.ParseFloat(gpuCostStr, 64)
	if err != nil {
		log.Warnf("CostModel: custom pricing has illegal GPU cost: %s", gpuCostStr)
	}
	node.CostPerGPUHr = costPerGPUHr

	costPerRAMHr, err := strconv.ParseFloat(ramCostStr, 64)
	if err != nil {
		log.Warnf("CostModel: custom pricing has illegal RAM cost: %s", ramCostStr)
	}
	node.CostPerRAMGiBHr = costPerRAMHr

	return node
}

// getNodePricing determines node pricing, given a key and a mapping from keys
// to their nodePricing instances, as well as the custom pricing configuration
// inherent to the CostModel instance. If custom pricing is set, use that. If
// not, use the pricing defined by the given key. If that doesn't exist, fall
// back on custom pricing as a default.
func (cm *CostModel) getNodePricing(nodeMap map[nodeKey]*nodePricing, nodeKey nodeKey) *nodePricing {
	// Find the relevant nodePricing, if it exists. If not, substitute the
	// custom nodePricing as a default.
	node, ok := nodeMap[nodeKey]
	if !ok || node == nil {
		if nodeKey.Node != "" {
			log.DedupedWarningf(5, "CostModel: failed to find node for %s", nodeKey)
		}
		return cm.getCustomNodePricing(false)
	}

	// If custom pricing is enabled and can be retrieved, override detected
	// node pricing with the custom values.
	customPricingConfig, err := cm.Provider.GetConfig()
	if err != nil {
		log.Warnf("CostModel: failed to load custom pricing: %s", err)
	}
	if cloud.CustomPricesEnabled(cm.Provider) && customPricingConfig != nil {
		return cm.getCustomNodePricing(node.Preemptible)
	}

	node.Source = "prometheus"

	// If any of the values are NaN or zero, replace them with the custom
	// values as default.
	// TODO:CLEANUP can't we parse these custom prices once? why do we store
	// them as strings like this?

	if node.CostPerCPUHr == 0 || math.IsNaN(node.CostPerCPUHr) {
		log.Warnf("CostModel: node pricing has illegal CostPerCPUHr; replacing with custom pricing: %s", nodeKey)
		cpuCostStr := customPricingConfig.CPU
		if node.Preemptible {
			cpuCostStr = customPricingConfig.SpotCPU
		}
		costPerCPUHr, err := strconv.ParseFloat(cpuCostStr, 64)
		if err != nil {
			log.Warnf("CostModel: custom pricing has illegal CPU cost: %s", cpuCostStr)
		}
		node.CostPerCPUHr = costPerCPUHr
		node.Source += "/customCPU"
	}

	if math.IsNaN(node.CostPerGPUHr) {
		log.Warnf("CostModel: node pricing has illegal CostPerGPUHr; replacing with custom pricing: %s", nodeKey)
		gpuCostStr := customPricingConfig.GPU
		if node.Preemptible {
			gpuCostStr = customPricingConfig.SpotGPU
		}
		costPerGPUHr, err := strconv.ParseFloat(gpuCostStr, 64)
		if err != nil {
			log.Warnf("CostModel: custom pricing has illegal GPU cost: %s", gpuCostStr)
		}
		node.CostPerGPUHr = costPerGPUHr
		node.Source += "/customGPU"
	}

	if node.CostPerRAMGiBHr == 0 || math.IsNaN(node.CostPerRAMGiBHr) {
		log.Warnf("CostModel: node pricing has illegal CostPerRAMHr; replacing with custom pricing: %s", nodeKey)
		ramCostStr := customPricingConfig.RAM
		if node.Preemptible {
			ramCostStr = customPricingConfig.SpotRAM
		}
		costPerRAMHr, err := strconv.ParseFloat(ramCostStr, 64)
		if err != nil {
			log.Warnf("CostModel: custom pricing has illegal RAM cost: %s", ramCostStr)
		}
		node.CostPerRAMGiBHr = costPerRAMHr
		node.Source += "/customRAM"
	}

	return node
}

/* PV/PVC Helpers */

func buildPVMap(resolution time.Duration, pvMap map[pvKey]*pv, resPVCostPerGiBHour, resPVActiveMins []*prom.QueryResult) {
	for _, result := range resPVActiveMins {
		key, err := resultPVKey(result, env.GetPromClusterLabel(), "persistentvolume")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: pv bytes query result missing field: %s", err)
			continue
		}

		pvStart, pvEnd := calculateStartAndEnd(result, resolution)
		if pvStart.IsZero() || pvEnd.IsZero() {
			log.Warnf("CostModel.ComputeAllocation: pv %s has no running time", key)
		}

		pvMap[key] = &pv{
			Cluster: key.Cluster,
			Name:    key.PersistentVolume,
			Start:   pvStart,
			End:     pvEnd,
		}
	}

	for _, result := range resPVCostPerGiBHour {
		key, err := resultPVKey(result, env.GetPromClusterLabel(), "volumename")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: thisPV bytes query result missing field: %s", err)
			continue
		}

		if _, ok := pvMap[key]; !ok {
			pvMap[key] = &pv{
				Cluster: key.Cluster,
				Name:    key.PersistentVolume,
			}
		}
		pvMap[key].CostPerGiBHour = result.Values[0].Value

	}
}

func applyPVBytes(pvMap map[pvKey]*pv, resPVBytes []*prom.QueryResult) {
	for _, res := range resPVBytes {
		key, err := resultPVKey(res, env.GetPromClusterLabel(), "persistentvolume")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: pv bytes query result missing field: %s", err)
			continue
		}

		if _, ok := pvMap[key]; !ok {
			log.Warnf("CostModel.ComputeAllocation: pv bytes result for missing pv: %s", err)
			continue
		}

		pvMap[key].Bytes = res.Values[0].Value
	}
}

func buildPVCMap(resolution time.Duration, pvcMap map[pvcKey]*pvc, pvMap map[pvKey]*pv, resPVCInfo []*prom.QueryResult) {
	for _, res := range resPVCInfo {
		cluster, err := res.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		values, err := res.GetStrings("persistentvolumeclaim", "storageclass", "volumename", "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: pvc info query result missing field: %s", err)
			continue
		}

		namespace := values["namespace"]
		name := values["persistentvolumeclaim"]
		volume := values["volumename"]
		storageClass := values["storageclass"]

		pvKey := newPVKey(cluster, volume)
		pvcKey := newPVCKey(cluster, namespace, name)

		pvcStart, pvcEnd := calculateStartAndEnd(res, resolution)
		if pvcStart.IsZero() || pvcEnd.IsZero() {
			log.Warnf("CostModel.ComputeAllocation: pvc %s has no running time", pvcKey)
		}

		if _, ok := pvMap[pvKey]; !ok {
			continue
		}

		pvMap[pvKey].StorageClass = storageClass

		if _, ok := pvcMap[pvcKey]; !ok {
			pvcMap[pvcKey] = &pvc{}
		}

		pvcMap[pvcKey].Name = name
		pvcMap[pvcKey].Namespace = namespace
		pvcMap[pvcKey].Cluster = cluster
		pvcMap[pvcKey].Volume = pvMap[pvKey]
		pvcMap[pvcKey].Start = pvcStart
		pvcMap[pvcKey].End = pvcEnd
	}
}

func applyPVCBytesRequested(pvcMap map[pvcKey]*pvc, resPVCBytesRequested []*prom.QueryResult) {
	for _, res := range resPVCBytesRequested {
		key, err := resultPVCKey(res, env.GetPromClusterLabel(), "namespace", "persistentvolumeclaim")
		if err != nil {
			continue
		}

		if _, ok := pvcMap[key]; !ok {
			continue
		}

		pvcMap[key].Bytes = res.Values[0].Value
	}
}

func buildPodPVCMap(podPVCMap map[podKey][]*pvc, pvMap map[pvKey]*pv, pvcMap map[pvcKey]*pvc, podMap map[podKey]*pod, resPodPVCAllocation []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey, ingestPodUID bool) {
	for _, res := range resPodPVCAllocation {
		cluster, err := res.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		values, err := res.GetStrings("persistentvolume", "persistentvolumeclaim", "pod", "namespace")
		if err != nil {
			log.DedupedWarningf(5, "CostModel.ComputeAllocation: pvc allocation query result missing field: %s", err)
			continue
		}

		namespace := values["namespace"]
		pod := values["pod"]
		name := values["persistentvolumeclaim"]
		volume := values["persistentvolume"]

		key := newPodKey(cluster, namespace, pod)
		pvKey := newPVKey(cluster, volume)
		pvcKey := newPVCKey(cluster, namespace, name)

		var keys []podKey

		if ingestPodUID {
			if uidKeys, ok := podUIDKeyMap[key]; ok {

				keys = append(keys, uidKeys...)

			}
		} else {
			keys = []podKey{key}
		}

		for _, key := range keys {

			if _, ok := pvMap[pvKey]; !ok {
				log.DedupedWarningf(5, "CostModel.ComputeAllocation: pv missing for pvc allocation query result: %s", pvKey)
				continue
			}

			if _, ok := podPVCMap[key]; !ok {
				podPVCMap[key] = []*pvc{}
			}

			pvc, ok := pvcMap[pvcKey]
			if !ok {
				log.DedupedWarningf(5, "CostModel.ComputeAllocation: pvc missing for pvc allocation query: %s", pvcKey)
				continue
			}

			if pod, ok := podMap[key]; !ok || len(pod.Allocations) <= 0 {
				log.DedupedWarningf(10, "CostModel.ComputeAllocation: pvc %s for missing pod %s", pvcKey, key)
				continue
			}

			pvc.Mounted = true

			podPVCMap[key] = append(podPVCMap[key], pvc)
		}
	}
}
func applyPVCsToPods(window kubecost.Window, podMap map[podKey]*pod, podPVCMap map[podKey][]*pvc, pvcMap map[pvcKey]*pvc) {
	// Because PVCs can be shared among pods, the respective pv cost
	// needs to be evenly distributed to those pods based on time
	// running, as well as the amount of time the pvc was shared.

	// Build a relation between every pvc to the pods that mount it
	// and a window representing the interval during which they
	// were associated.
	pvcPodWindowMap := make(map[pvcKey]map[podKey]kubecost.Window)

	for thisPodKey, thisPod := range podMap {
		if pvcs, ok := podPVCMap[thisPodKey]; ok {
			for _, thisPVC := range pvcs {

				// Determine the (start, end) of the relationship between the
				// given pvc and the associated Allocation so that a precise
				// number of hours can be used to compute cumulative cost.
				s, e := thisPod.Start, thisPod.End
				if thisPVC.Start.After(thisPod.Start) {
					s = thisPVC.Start
				}
				if thisPVC.End.Before(thisPod.End) {
					e = thisPVC.End
				}

				thisPVCKey := thisPVC.key()
				if pvcPodWindowMap[thisPVCKey] == nil {
					pvcPodWindowMap[thisPVCKey] = make(map[podKey]kubecost.Window)
				}

				pvcPodWindowMap[thisPVCKey][thisPodKey] = kubecost.NewWindow(&s, &e)
			}
		}
	}

	for thisPVCKey, podWindowMap := range pvcPodWindowMap {
		// Build out a pv price coefficient for each pod with a pvc. Each
		// pvc-pod relation needs a coefficient which modifies the pv cost
		// such that pv costs can be shared between all pods using that pvc.

		// Get single-point intervals from alloc-pvc relation windows.
		intervals := getIntervalPointsFromWindows(podWindowMap)

		pvc, ok := pvcMap[thisPVCKey]
		if !ok {
			log.DedupedWarningf(5, "Missing pvc with key %s", thisPVCKey)
			continue
		}

		// Determine coefficients for each pvc-pod relation.
		sharedPVCCostCoefficients := getPVCCostCoefficients(intervals, pvc)

		// Distribute pvc costs to Allocations
		for thisPodKey, coeffComponents := range sharedPVCCostCoefficients {
			pod, ok2 := podMap[thisPodKey]
			// If pod does not exist or the pod does not have any allocations
			// get unmounted pod for cluster
			if !ok2 || len(pod.Allocations) == 0 {
				// Get namespace unmounted pod, as pvc will have a namespace
				pod = getUnmountedPodForNamespace(window, podMap, pvc.Cluster, pvc.Namespace)
			}
			for _, alloc := range pod.Allocations {
				s, e := pod.Start, pod.End

				minutes := e.Sub(s).Minutes()
				hrs := minutes / 60.0

				gib := pvc.Bytes / 1024 / 1024 / 1024
				cost := pvc.Volume.CostPerGiBHour * gib * hrs
				byteHours := pvc.Bytes * hrs
				coef := getCoefficientFromComponents(coeffComponents)

				// Apply the size and cost of the pv to the allocation, each
				// weighted by count (i.e. the number of containers in the pod)
				// record the amount of total PVBytes Hours attributable to a given pv
				if alloc.PVs == nil {
					alloc.PVs = kubecost.PVAllocations{}
				}
				pvKey := kubecost.PVKey{
					Cluster: pvc.Volume.Cluster,
					Name:    pvc.Volume.Name,
				}
				// Both Cost and byteHours should be multiplied by the coef and divided by count
				// so that you if all allocations with a given pv key are summed the result of those
				// would be equal to the values of the original pv
				count := float64(len(pod.Allocations))
				alloc.PVs[pvKey] = &kubecost.PVAllocation{
					ByteHours: byteHours * coef / count,
					Cost:      cost * coef / count,
				}
			}
		}
	}
}

func applyUnmountedPVs(window kubecost.Window, podMap map[podKey]*pod, pvMap map[pvKey]*pv, pvcMap map[pvcKey]*pvc) {
	for _, pv := range pvMap {
		mounted := false
		for _, pvc := range pvcMap {
			if pvc.Volume == nil {
				continue
			}
			if pvc.Volume == pv {
				mounted = true
				break
			}
		}

		if !mounted {

			// a pv without a pvc will not have a namespace, so get the cluster unmounted pod
			pod := getUnmountedPodForCluster(window, podMap, pv.Cluster)

			// Calculate pv Cost

			// Unmounted pv should have correct keyso it can still reconcile
			thisPVKey := kubecost.PVKey{
				Cluster: pv.Cluster,
				Name:    pv.Name,
			}
			gib := pv.Bytes / 1024 / 1024 / 1024
			hrs := pv.minutes() / 60.0
			cost := pv.CostPerGiBHour * gib * hrs
			unmountedPVs := kubecost.PVAllocations{
				thisPVKey: {
					ByteHours: pv.Bytes * hrs,
					Cost:      cost,
				},
			}
			pod.Allocations[kubecost.UnmountedSuffix].PVs = pod.Allocations[kubecost.UnmountedSuffix].PVs.Add(unmountedPVs)
		}
	}
}

func applyUnmountedPVCs(window kubecost.Window, podMap map[podKey]*pod, pvcMap map[pvcKey]*pvc) {
	for _, pvc := range pvcMap {
		if !pvc.Mounted && pvc.Volume != nil {

			// Get namespace unmounted pod, as pvc will have a namespace
			pod := getUnmountedPodForNamespace(window, podMap, pvc.Cluster, pvc.Namespace)

			// Calculate pv Cost

			// Unmounted pv should have correct key so it can still reconcile
			thisPVKey := kubecost.PVKey{
				Cluster: pvc.Volume.Cluster,
				Name:    pvc.Volume.Name,
			}

			// Use the Volume Bytes here because pvc bytes could be different,
			// however the pv bytes are what are going to determine cost
			gib := pvc.Volume.Bytes / 1024 / 1024 / 1024
			hrs := pvc.Volume.minutes() / 60.0
			cost := pvc.Volume.CostPerGiBHour * gib * hrs
			unmountedPVs := kubecost.PVAllocations{
				thisPVKey: {
					ByteHours: pvc.Volume.Bytes * hrs,
					Cost:      cost,
				},
			}
			pod.Allocations[kubecost.UnmountedSuffix].PVs = pod.Allocations[kubecost.UnmountedSuffix].PVs.Add(unmountedPVs)
		}
	}
}

/* Helper Helpers */

// getUnmountedPodForCluster retrieve the unmounted pod for a cluster and create it if it does not exist
func getUnmountedPodForCluster(window kubecost.Window, podMap map[podKey]*pod, cluster string) *pod {
	container := kubecost.UnmountedSuffix
	podName := kubecost.UnmountedSuffix
	namespace := kubecost.UnmountedSuffix
	node := ""

	thisPodKey := getUnmountedPodKey(cluster)
	// Initialize pod and container if they do not already exist
	thisPod, ok := podMap[thisPodKey]
	if !ok {
		thisPod = &pod{
			Window:      window.Clone(),
			Start:       *window.Start(),
			End:         *window.End(),
			Key:         thisPodKey,
			Allocations: map[string]*kubecost.Allocation{},
		}

		thisPod.appendContainer(container)
		thisPod.Allocations[container].Properties.Cluster = cluster
		thisPod.Allocations[container].Properties.Node = node
		thisPod.Allocations[container].Properties.Namespace = namespace
		thisPod.Allocations[container].Properties.Pod = podName
		thisPod.Allocations[container].Properties.Container = container

		thisPod.Node = node

		podMap[thisPodKey] = thisPod
	}
	return thisPod
}

// getUnmountedPodForNamespace is as getUnmountedPodForCluster, but keys allocation property pod/namespace field off namespace
// This creates or adds allocations to an unmounted pod in the specified namespace, rather than in __unmounted__
func getUnmountedPodForNamespace(window kubecost.Window, podMap map[podKey]*pod, cluster string, namespace string) *pod {
	container := kubecost.UnmountedSuffix
	podName := fmt.Sprintf("%s-unmounted-pvcs", namespace)
	node := ""

	thisPodKey := newPodKey(cluster, namespace, podName)
	// Initialize pod and container if they do not already exist
	thisPod, ok := podMap[thisPodKey]
	if !ok {
		thisPod = &pod{
			Window:      window.Clone(),
			Start:       *window.Start(),
			End:         *window.End(),
			Key:         thisPodKey,
			Allocations: map[string]*kubecost.Allocation{},
		}

		thisPod.appendContainer(container)
		thisPod.Allocations[container].Properties.Cluster = cluster
		thisPod.Allocations[container].Properties.Node = node
		thisPod.Allocations[container].Properties.Namespace = namespace
		thisPod.Allocations[container].Properties.Pod = podName
		thisPod.Allocations[container].Properties.Container = container

		thisPod.Node = node

		podMap[thisPodKey] = thisPod
	}
	return thisPod
}

func calculateStartAndEnd(result *prom.QueryResult, resolution time.Duration) (time.Time, time.Time) {
	s := time.Unix(int64(result.Values[0].Timestamp), 0).UTC()
	// subtract resolution from start time to cover full time period
	s = s.Add(-resolution)
	e := time.Unix(int64(result.Values[len(result.Values)-1].Timestamp), 0).UTC()
	return s, e
}

// calculateStartEndFromIsRunning Calculates the start and end of a prom result when the values of the datum are 0 for not running and 1 for running
// the coeffs are used to adjust the start and end when the value is not equal to 1 or 0, which means that pod came up or went down in that window.
func calculateStartEndFromIsRunning(result *prom.QueryResult, resolution time.Duration, window kubecost.Window) (time.Time, time.Time) {
	// start and end are the timestamps of the first and last
	// minutes the pod was running, respectively. We subtract one resolution
	// from start because this point will actually represent the end
	// of the first minute. We don't subtract from end because it
	// already represents the end of the last minute.
	var start, end time.Time
	startAdjustmentCoeff, endAdjustmentCoeff := 1.0, 1.0
	for _, datum := range result.Values {
		t := time.Unix(int64(datum.Timestamp), 0)

		if start.IsZero() && datum.Value > 0 && window.Contains(t) {
			// Set the start timestamp to the earliest non-zero timestamp
			start = t

			// Record adjustment coefficient, i.e. the portion of the start
			// timestamp to "ignore". That is, sometimes the value will be
			// 0.5, meaning that we should discount the time running by
			// half of the resolution the timestamp stands for.
			startAdjustmentCoeff = (1.0 - datum.Value)
		}

		if datum.Value > 0 && window.Contains(t) {
			// Set the end timestamp to the latest non-zero timestamp
			end = t

			// Record adjustment coefficient, i.e. the portion of the end
			// timestamp to "ignore". (See explanation above for start.)
			endAdjustmentCoeff = (1.0 - datum.Value)
		}
	}

	// Do not attempt to adjust start if it is zero
	if !start.IsZero() {
		// Adjust timestamps according to the resolution and the adjustment
		// coefficients, as described above. That is, count the start timestamp
		// from the beginning of the resolution, not the end. Then "reduce" the
		// start and end by the correct amount, in the case that the "running"
		// value of the first or last timestamp was not a full 1.0.
		start = start.Add(-resolution)
		// Note: the *100 and /100 are necessary because Duration is an int, so
		// 0.5, for instance, will be truncated, resulting in no adjustment.
		start = start.Add(time.Duration(startAdjustmentCoeff*100) * resolution / time.Duration(100))
		end = end.Add(-time.Duration(endAdjustmentCoeff*100) * resolution / time.Duration(100))

		// Ensure that the start is always within the window, adjusting
		// for the occasions where start falls 1m before the query window.
		// NOTE: window here will always be closed (so no need to nil check
		// "start").
		// TODO:CLEANUP revisit query methodology to figure out why this is
		// happening on occasion
		if start.Before(*window.Start()) {
			start = *window.Start()
		}
	}

	// do not attempt to adjust end if it is zero
	if !end.IsZero() {
		// If there is only one point with a value <= 0.5 that the start and
		// end timestamps both share, then we will enter this case because at
		// least half of a resolution will be subtracted from both the start
		// and the end. If that is the case, then add back half of each side
		// so that the pod is said to run for half a resolution total.
		// e.g. For resolution 1m and a value of 0.5 at one timestamp, we'll
		//      end up with end == start and each coeff == 0.5. In
		//      that case, add 0.25m to each side, resulting in 0.5m duration.
		if !end.After(start) {
			start = start.Add(-time.Duration(50*startAdjustmentCoeff) * resolution / time.Duration(100))
			end = end.Add(time.Duration(50*endAdjustmentCoeff) * resolution / time.Duration(100))
		}

		// Ensure that the allocEnf is always within the window, adjusting
		// for the occasions where end falls 1m after the query window. This
		// has not ever happened, but is symmetrical with the start check
		// above.
		// NOTE: window here will always be closed (so no need to nil check
		// "end").
		// TODO:CLEANUP revisit query methodology to figure out why this is
		// happening on occasion
		if end.After(*window.End()) {
			end = *window.End()
		}
	}
	return start, end
}
