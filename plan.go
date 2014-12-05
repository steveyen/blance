//  Copyright (c) 2014 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the
//  License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing,
//  software distributed under the License is distributed on an "AS
//  IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
//  express or implied. See the License for the specific language
//  governing permissions and limitations under the License.

package blance

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
)

var MaxIterationsPerPlan = 10

func planNextMap(
	prevMap PartitionMap,
	nodesAll []string,
	nodesToRemove []string,
	nodesToAdd []string,
	model PartitionModel,
	modelStateConstraints map[string]int, // Keyed by stateName.
	partitionWeights map[string]int, // Keyed by partitionName.
	stateStickiness map[string]int, // Keyed by stateName.
	nodeWeights map[string]int, // Keyed by node.
	nodeHierarchy map[string]string, // Keyed by node, value is node's parent.
	hierarchyRules HierarchyRules,
) (nextMap PartitionMap, warnings []string) {
	for i := 0; i < MaxIterationsPerPlan; i++ { // Loop for convergence.
		nextMap, warnings = planNextMapInner(prevMap,
			nodesAll, nodesToRemove, nodesToAdd,
			model, modelStateConstraints,
			partitionWeights, stateStickiness, nodeWeights,
			nodeHierarchy, hierarchyRules)
		if reflect.DeepEqual(nextMap, prevMap) {
			break
		}
		prevMap = nextMap
		nodesAll = StringsRemoveStrings(nodesAll, nodesToRemove)
		nodesToRemove = []string{}
		nodesToAdd = []string{}
	}
	return nextMap, warnings
}

func planNextMapInner(
	prevMap PartitionMap,
	nodesAll []string,
	nodesToRemove []string,
	nodesToAdd []string,
	model PartitionModel,
	modelStateConstraints map[string]int, // Keyed by stateName.
	partitionWeights map[string]int, // Keyed by partitionName.
	stateStickiness map[string]int, // Keyed by stateName.
	nodeWeights map[string]int, // Keyed by node.
	nodeHierarchy map[string]string, // Keyed by node, value is node's parent.
	hierarchyRules HierarchyRules,
) (PartitionMap, []string) {
	warnings := []string{}

	nodesNext := StringsRemoveStrings(nodesAll, nodesToRemove)

	hierarchyChildren := mapParentsToMapChildren(nodeHierarchy)

	// Start by filling out nextPartitions as a deep clone of
	// prevMap.Partitions, but filter out the to-be-removed nodes.
	nextPartitions := prevMap.toArrayCopy()
	for _, partition := range nextPartitions {
		partition.NodesByState =
			removeNodesFromNodesByState(partition.NodesByState, nodesToRemove, nil)
	}
	sort.Sort(&partitionSorter{a: nextPartitions})

	// Key is stateName, value is {node: count}.
	var stateNodeCounts map[string]map[string]int

	stateNodeCounts = countStateNodes(prevMap, partitionWeights)

	// Helper function that returns an ordered array of candidates
	// nodes to assign to a partition, ordered by best heuristic fit.
	findBestNodes := func(
		partition *Partition,
		stateName string,
		constraints int,
		nodeToNodeCounts map[string]map[string]int,
	) []string {
		stickiness := 1.5
		if partitionWeights != nil {
			w, exists := partitionWeights[partition.Name]
			if exists {
				stickiness = float64(w)
			} else if stateStickiness != nil {
				s, exists := stateStickiness[stateName]
				if exists {
					stickiness = float64(s)
				}
			}
		}

		// Keyed by node, value is sum of partitions on that node.
		nodePartitionCounts := make(map[string]int)
		for _, nodeCounts := range stateNodeCounts {
			for node, nodeCount := range nodeCounts {
				nodePartitionCounts[node] = nodePartitionCounts[node] + nodeCount
			}
		}

		topPriorityStateName := ""
		for stateName, state := range model {
			if topPriorityStateName == "" ||
				state.Priority < model[topPriorityStateName].Priority {
				topPriorityStateName = stateName
			}
		}

		topPriorityNode := ""
		topPriorityStateNodes := partition.NodesByState[topPriorityStateName]
		if len(topPriorityStateNodes) > 0 {
			topPriorityNode = topPriorityStateNodes[0]
		}

		statePriority := model[stateName].Priority

		candidateNodes := append([]string(nil), nodesNext...)

		// Filter out nodes of a higher priority state; e.g., if we're
		// assigning slaves, leave the masters untouched.
		excludeHigherPriorityNodes := func(remainingNodes []string) []string {
			for stateName, stateNodes := range partition.NodesByState {
				if model[stateName].Priority < statePriority {
					remainingNodes = StringsRemoveStrings(remainingNodes, stateNodes)
				}
			}
			return remainingNodes
		}

		candidateNodes = excludeHigherPriorityNodes(candidateNodes)

		sort.Sort(&nodeSorter{
			stateName:           stateName,
			partition:           partition,
			numPartitions:       len(prevMap),
			topPriorityNode:     topPriorityNode,
			stateNodeCounts:     stateNodeCounts,
			nodeToNodeCounts:    nodeToNodeCounts,
			nodePartitionCounts: nodePartitionCounts,
			nodeWeights:         nodeWeights,
			stickiness:          stickiness,
			a:                   candidateNodes,
		})

		if hierarchyRules != nil {
			hierarchyNodes := []string{}

			for _, hierarchyRule := range hierarchyRules[stateName] {
				h := topPriorityNode
				if h == "" && len(hierarchyNodes) > 0 {
					h = hierarchyNodes[0]
				}

				hierarchyCandidates := includeExcludeNodes(h,
					hierarchyRule.IncludeLevel,
					hierarchyRule.ExcludeLevel,
					nodeHierarchy, hierarchyChildren)
				hierarchyCandidates =
					StringsIntersectStrings(hierarchyCandidates, nodesNext)
				hierarchyCandidates =
					excludeHigherPriorityNodes(hierarchyCandidates)

				sort.Sort(&nodeSorter{
					stateName:           stateName,
					partition:           partition,
					numPartitions:       len(prevMap),
					topPriorityNode:     topPriorityNode,
					stateNodeCounts:     stateNodeCounts,
					nodeToNodeCounts:    nodeToNodeCounts,
					nodePartitionCounts: nodePartitionCounts,
					nodeWeights:         nodeWeights,
					stickiness:          stickiness,
					a:                   hierarchyCandidates,
				})

				if len(hierarchyCandidates) > 0 {
					hierarchyNodes = append(hierarchyNodes,
						hierarchyCandidates[0])
				} else if len(candidateNodes) > 0 {
					hierarchyNodes = append(hierarchyNodes,
						candidateNodes[0])
				}
			}

			candidateNodes = append(hierarchyNodes, candidateNodes...)
		}

		if len(candidateNodes) >= constraints {
			candidateNodes = candidateNodes[0:constraints]
		} else {
			warnings = append(warnings,
				fmt.Sprintf("could not meet contraints: %d,"+
					" stateName: %s, partitionName: %s",
					constraints, stateName, partition.Name))
		}

		// Keep nodeToNodeCounts updated.
		for _, candidateNode := range candidateNodes {
			m, exists := nodeToNodeCounts[topPriorityNode]
			if !exists {
				m = make(map[string]int)
				nodeToNodeCounts[topPriorityNode] = m
			}
			m[candidateNode] = m[candidateNode] + 1
		}

		return candidateNodes
	}

	// Helper function that given a PartitionModel state name and its
	// constraints, for every partition, assign nodes by mutating
	// nextPartitions.
	assignStateToPartitions := func(stateName string, constraints int) {
		// Sort the partitions to help reach a better assignment.
		p := &partitionSorter{
			stateName:        stateName,
			prevMap:          prevMap,
			nodesToRemove:    nodesToRemove,
			nodesToAdd:       nodesToAdd,
			partitionWeights: partitionWeights,
			a:                append([]*Partition(nil), nextPartitions...),
		}
		sort.Sort(p)

		// Key is higherPriorityNode, value is {lowerPriorityNode: count}.
		nodeToNodeCounts := make(map[string]map[string]int)

		for _, partition := range p.a {
			partitionWeight := 1
			if partitionWeights != nil {
				w, exists := partitionWeights[partition.Name]
				if exists {
					partitionWeight = w
				}
			}

			incStateNodeCounts := func(stateName string, nodes []string) {
				adjustStateNodeCounts(stateNodeCounts, stateName, nodes,
					partitionWeight)
			}
			decStateNodeCounts := func(stateName string, nodes []string) {
				adjustStateNodeCounts(stateNodeCounts, stateName, nodes,
					-partitionWeight)
			}

			nodesToAssign :=
				findBestNodes(partition, stateName, constraints, nodeToNodeCounts)

			partition.NodesByState =
				removeNodesFromNodesByState(partition.NodesByState,
					partition.NodesByState[stateName],
					decStateNodeCounts)
			partition.NodesByState =
				removeNodesFromNodesByState(partition.NodesByState,
					nodesToAssign,
					decStateNodeCounts)

			partition.NodesByState[stateName] = nodesToAssign

			incStateNodeCounts(stateName, nodesToAssign)
		}
	}

	// Run through the sorted partition states (master, slave, etc)
	// that have constraints and invoke assignStateToPartitions().
	pms := &stateNameSorter{
		m: model,
		s: make([]string, 0, len(model)),
	}
	for stateName := range model {
		pms.s = append(pms.s, stateName)
	}
	sort.Sort(pms)

	for _, stateName := range pms.s {
		constraints, exists := modelStateConstraints[stateName]
		if !exists {
			modelState, exists := model[stateName]
			if exists && modelState != nil {
				constraints = modelState.Constraints
			}
		}

		if constraints > 0 {
			assignStateToPartitions(stateName, constraints)
		}
	}

	rv := PartitionMap{}
	for _, partition := range nextPartitions {
		rv[partition.Name] = partition
	}
	return rv, warnings
}

// Makes a deep copy of the PartitionMap as an array.
func (m PartitionMap) toArrayCopy() []*Partition {
	rv := make([]*Partition, 0, len(m))
	for _, partition := range m {
		rv = append(rv, &Partition{
			Name:         partition.Name,
			NodesByState: copyNodesByState(partition.NodesByState),
		})
	}
	return rv
}

func copyNodesByState(nbs map[string][]string) map[string][]string {
	rv := make(map[string][]string)
	for stateName, nodes := range nbs {
		rv[stateName] = append([]string(nil), nodes...)
	}
	return rv
}

func adjustStateNodeCounts(stateNodeCounts map[string]map[string]int,
	stateName string, nodes []string, amt int) {
	for _, node := range nodes {
		s, exists := stateNodeCounts[stateName]
		if !exists || s == nil {
			s = make(map[string]int)
			stateNodeCounts[stateName] = s
		}
		s[node] = s[node] + amt
	}
}

// Example, with input partitionMap of...
//   { "0": { NodesByState: {"master": ["a"], "slave": ["b", "c"]} },
//     "1": { NodesByState: {"master": ["b"], "slave": ["c"]} } }
// then return value will be...
//   { "master": { "a": 1, "b": 1 },
//     "slave": { "b": 1, "c": 2 } }
func countStateNodes(
	partitionMap PartitionMap,
	partitionWeights map[string]int,
) map[string]map[string]int {
	rv := make(map[string]map[string]int)
	for partitionName, partition := range partitionMap {
		for stateName, nodes := range partition.NodesByState {
			s := rv[stateName]
			if s == nil {
				s = make(map[string]int)
				rv[stateName] = s
			}
			for _, node := range nodes {
				partitionWeight := 1
				if partitionWeights != nil {
					w, exists := partitionWeights[partitionName]
					if exists {
						partitionWeight = w
					}
				}
				s[node] = s[node] + partitionWeight
			}
		}
	}
	return rv
}

// --------------------------------------------------------

// Returns a copy of nodesByState but with nodes removed.  Example,
// when removeNodes == ["a"] and nodesByState == {"master": ["a"],
// "slave": ["b"]}, then result will be {"master": [], "slave":
// ["b"]}.  Optional callback is invoked with the nodes that will
// actually be removed.
func removeNodesFromNodesByState(
	nodesByState map[string][]string,
	removeNodes []string,
	cb func(stateName string, nodesToBeRemoved []string),
) map[string][]string {
	rv := make(map[string][]string)
	for stateName, nodes := range nodesByState {
		if cb != nil {
			cb(stateName, StringsIntersectStrings(nodes, removeNodes))
		}
		rv[stateName] = StringsRemoveStrings(nodes, removeNodes)
	}
	return rv
}

// Given a nodesByState, like {"master": ["a"], "slave": ["b", "c"]},
// this function might return something like ["b", "c", "a"].
func flattenNodesByState(nodesByState map[string][]string) []string {
	rv := make([]string, 0)
	for _, nodes := range nodesByState {
		rv = append(rv, nodes...)
	}
	return rv
}

// --------------------------------------------------------

// Does ORDER BY m.States[stateName].Priority ASC, stateName ASC".
type stateNameSorter struct {
	m PartitionModel
	s []string // This array is mutated during a sort.Sort()
}

func (pms *stateNameSorter) Len() int {
	return len(pms.s)
}

func (pms *stateNameSorter) Less(i, j int) bool {
	iname, jname := pms.s[i], pms.s[j]

	if pms.m != nil &&
		pms.m[iname] != nil &&
		pms.m[jname] != nil &&
		pms.m[iname].Priority < pms.m[jname].Priority {
		return true
	}

	return iname < jname
}

func (pms *stateNameSorter) Swap(i, j int) {
	pms.s[i], pms.s[j] = pms.s[j], pms.s[i]
}

// --------------------------------------------------------

// Does ORDER BY partitions-on-nodes-to-be-removed, then by
// partitions-who-haven't-been-assigned-anywhere-yet, then by
// partition-weight, then by partition-name.
type partitionSorter struct {
	stateName        string // When "", just sort by partition name.
	prevMap          PartitionMap
	nodesToRemove    []string
	nodesToAdd       []string
	partitionWeights map[string]int // Keyed by partition name.

	a []*Partition // This array is mutated during sort.Sort().
}

func (r *partitionSorter) Len() int {
	return len(r.a)
}

func (r *partitionSorter) Less(i, j int) bool {
	ei := r.Score(i)
	ej := r.Score(j)
	for x := 0; x < len(ei) && x < len(ej); x++ {
		if ei[x] < ej[x] {
			return true
		}
	}
	return len(ei) < len(ej)
}

func (r *partitionSorter) Swap(i, j int) {
	r.a[i], r.a[j] = r.a[j], r.a[i]
}

func (r *partitionSorter) Score(i int) []string {
	partitionName := r.a[i].Name
	partitionNameStr := partitionName

	// If the partitionName looks like a positive integer, then
	// zero-pad it for sortability.
	partitionN, err := strconv.Atoi(partitionName)
	if err != nil && partitionN >= 0 {
		partitionNameStr = fmt.Sprintf("%10d", partitionN)
	}

	// Calculate partition weight, and zero-pad it for sortability,
	// where the nine 9's magic number is to to allow heavier
	// partitions to come first.
	partitionWeight := 1
	if r.partitionWeights != nil {
		if w, exists := r.partitionWeights[partitionName]; exists {
			partitionWeight = w
		}
	}
	partitionWeightStr := fmt.Sprintf("%10d", 999999999-partitionWeight)

	// First, favor partitions on nodes that are to-be-removed.
	if r.prevMap != nil &&
		r.nodesToRemove != nil {
		lastPartition := r.prevMap[partitionName]
		lastPartitionNBS := lastPartition.NodesByState[r.stateName]
		if lastPartitionNBS != nil &&
			len(StringsIntersectStrings(lastPartitionNBS, r.nodesToRemove)) > 0 {
			return []string{"0", partitionWeightStr, partitionNameStr}
		}
	}

	// Then, favor partitions who haven't yet been assigned to any
	// newly added nodes yet for any state.
	if r.nodesToAdd != nil {
		fnbs := flattenNodesByState(r.a[i].NodesByState)
		if len(StringsIntersectStrings(fnbs, r.nodesToAdd)) <= 0 {
			return []string{"1", partitionWeightStr, partitionNameStr}
		}
	}

	return []string{"2", partitionWeightStr, partitionNameStr}
}

// --------------------------------------------------------

type nodeSorter struct {
	stateName           string
	partition           *Partition
	numPartitions       int
	topPriorityNode     string
	stateNodeCounts     map[string]map[string]int
	nodeToNodeCounts    map[string]map[string]int
	nodePartitionCounts map[string]int
	nodeWeights         map[string]int
	stickiness          float64

	a []string // Entries are node names.
}

func (ns *nodeSorter) Len() int {
	return len(ns.a)
}

func (ns *nodeSorter) Less(i, j int) bool {
	return ns.Score(i) < ns.Score(j)
}

func (ns *nodeSorter) Swap(i, j int) {
	ns.a[i], ns.a[j] = ns.a[j], ns.a[i]
}

func (ns *nodeSorter) Score(i int) float64 {
	node := ns.a[i]

	lowerPriorityBalanceFactor := 0.0
	if ns.nodeToNodeCounts != nil && ns.numPartitions > 0 {
		m, exists := ns.nodeToNodeCounts[ns.topPriorityNode]
		if exists {
			lowerPriorityBalanceFactor =
				float64(m[node]) / float64(ns.numPartitions)
		}
	}

	filledFactor := 0.0
	if ns.nodePartitionCounts != nil && ns.numPartitions > 0 {
		c, exists := ns.nodePartitionCounts[node]
		if exists {
			filledFactor = (0.001 * float64(c)) / float64(ns.numPartitions)
		}
	}

	currentFactor := 0.0
	if ns.partition != nil {
		for _, stateNode := range ns.partition.NodesByState[ns.stateName] {
			if stateNode == node {
				currentFactor = ns.stickiness
			}
		}
	}

	r := 0.0
	if ns.stateNodeCounts != nil {
		nodeCounts, exists := ns.stateNodeCounts[ns.stateName]
		if exists && nodeCounts != nil {
			r = float64(nodeCounts[node])
		}
	}

	r = r + lowerPriorityBalanceFactor
	r = r + filledFactor

	if ns.nodeWeights != nil {
		w, exists := ns.nodeWeights[node]
		if exists && w > 0 {
			r = r / float64(w)
		}
	}

	r = r - currentFactor

	return r
}

// --------------------------------------------------------

// The mapParents is keyed by node, value is parent node.  Returns a
// map keeyed by node, value is array of child nodes.
func mapParentsToMapChildren(mapParents map[string]string) map[string][]string {
	nodes := make([]string, 0) // Sort for stability.
	for node, _ := range mapParents {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)

	rv := make(map[string][]string)
	for _, child := range nodes {
		parent := mapParents[child]
		rv[parent] = append(rv[parent], child)
	}
	return rv
}

// The includeLevel is tree ancestor inclusion level, and excludeLevel
// is tree ancestor exclusion level.  Example: includeLevel of 2 and
// excludeLevel of 1 means include nodes with the same grandparent
// (level 2), but exclude nodes with the same parent (level 1).
func includeExcludeNodes(node string,
	includeLevel int,
	excludeLevel int,
	mapParents map[string]string,
	mapChildren map[string][]string) []string {
	incNodes := findLeaves(findAncestor(node, mapParents, includeLevel), mapChildren)
	excNodes := findLeaves(findAncestor(node, mapParents, excludeLevel), mapChildren)
	return StringsRemoveStrings(incNodes, excNodes)
}

func findAncestor(node string, mapParents map[string]string, level int) string {
	for level > 0 {
		node = mapParents[node]
		level--
	}
	return node
}

func findLeaves(node string, mapChildren map[string][]string) []string {
	children := mapChildren[node]
	if len(children) <= 0 {
		return []string{node} // Node is a leaf.
	}
	rv := make([]string, 0)
	for _, c := range children {
		rv = append(rv, findLeaves(c, mapChildren)...)
	}
	return rv
}
