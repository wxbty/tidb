// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package core

import (
	"fmt"
	"math"
	"strings"

	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/expression/aggregation"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/planner/property"
	"github.com/pingcap/tidb/planner/util"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tipb/go-tipb"
)

func getPlanCost(p PhysicalPlan, taskType property.TaskType, option *PlanCostOption) (float64, error) {
	if p.SCtx().GetSessionVars().CostModelVersion == modelVer2 {
		planCost, err := p.getPlanCostVer2(taskType, option)
		return planCost.cost, err
	}
	return p.getPlanCostVer1(taskType, option)
}

// getPlanCostVer2 calculates the cost of the plan if it has not been calculated yet and returns the cost.
func (p *basePhysicalPlan) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}
	childCosts := make([]costVer2, 0, len(p.children))
	for _, child := range p.children {
		childCost, err := child.getPlanCostVer2(taskType, option)
		if err != nil {
			return zeroCostVer2, err
		}
		childCosts = append(childCosts, childCost)
	}
	if len(childCosts) == 0 {
		p.planCostVer2 = newZeroCostVer2(traceCost(option))
	} else {
		p.planCostVer2 = sumCostVer2(childCosts...)
	}
	p.planCostInit = true
	return p.planCostVer2.label(p), nil
}

// getPlanCostVer2 returns the plan-cost of this sub-plan, which is:
// plan-cost = child-cost + filter-cost
func (p *PhysicalSelection) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}

	inputRows := getCardinality(p.children[0], option.CostFlag)
	cpuFactor := getTaskCPUFactorVer2(p, taskType)

	filterCost := filterCostVer2(option, inputRows, p.Conditions, cpuFactor)

	childCost, err := p.children[0].getPlanCostVer2(taskType, option)
	if err != nil {
		return zeroCostVer2, err
	}

	p.planCostVer2 = sumCostVer2(filterCost, childCost)
	p.planCostInit = true
	return p.planCostVer2.label(p), nil
}

// getPlanCostVer2 returns the plan-cost of this sub-plan, which is:
// plan-cost = child-cost + proj-cost / concurrency
// proj-cost = input-rows * len(expressions) * cpu-factor
func (p *PhysicalProjection) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}

	inputRows := getCardinality(p.children[0], option.CostFlag)
	cpuFactor := getTaskCPUFactorVer2(p, taskType)
	concurrency := float64(p.ctx.GetSessionVars().ProjectionConcurrency())

	projCost := filterCostVer2(option, inputRows, p.Exprs, cpuFactor)

	childCost, err := p.children[0].getPlanCostVer2(taskType, option)
	if err != nil {
		return zeroCostVer2, err
	}

	p.planCostVer2 = sumCostVer2(childCost, divCostVer2(projCost, concurrency))
	p.planCostInit = true
	return p.planCostVer2.label(p), nil
}

// getPlanCostVer2 returns the plan-cost of this sub-plan, which is:
// plan-cost = rows * log2(row-size) * scan-factor
// log2(row-size) is from experiments.
func (p *PhysicalIndexScan) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}

	rows := getCardinality(p, option.CostFlag)
	rowSize := math.Max(p.getScanRowSize(), 2.0)
	scanFactor := getTaskScanFactorVer2(p, taskType)

	p.planCostVer2 = scanCostVer2(option, rows, rowSize, scanFactor)
	p.planCostInit = true
	return p.planCostVer2.label(p), nil
}

// getPlanCostVer2 returns the plan-cost of this sub-plan, which is:
// plan-cost = rows * log2(row-size) * scan-factor
// log2(row-size) is from experiments.
func (p *PhysicalTableScan) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}

	rows := getCardinality(p, option.CostFlag)
	rowSize := math.Max(p.getScanRowSize(), 2.0)
	scanFactor := getTaskScanFactorVer2(p, taskType)

	p.planCostVer2 = scanCostVer2(option, rows, rowSize, scanFactor)

	// give TiFlash a start-up cost to let the optimizer prefers to use TiKV to process small table scans.
	if p.StoreType == kv.TiFlash {
		p.planCostVer2 = sumCostVer2(p.planCostVer2, scanCostVer2(option, 10000, rowSize, scanFactor))
	}

	p.planCostInit = true
	return p.planCostVer2.label(p), nil
}

// getPlanCostVer2 returns the plan-cost of this sub-plan, which is:
// plan-cost = (child-cost + net-cost) / concurrency
// net-cost = rows * row-size * net-factor
func (p *PhysicalIndexReader) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}

	rows := getCardinality(p.indexPlan, option.CostFlag)
	rowSize := getAvgRowSize(p.indexPlan.Stats(), p.indexPlan.Schema())
	netFactor := getTaskNetFactorVer2(p, taskType)
	concurrency := float64(p.ctx.GetSessionVars().DistSQLScanConcurrency())

	netCost := netCostVer2(option, rows, rowSize, netFactor)

	childCost, err := p.indexPlan.getPlanCostVer2(property.CopSingleReadTaskType, option)
	if err != nil {
		return zeroCostVer2, err
	}

	p.planCostVer2 = divCostVer2(sumCostVer2(childCost, netCost), concurrency)
	p.planCostInit = true
	return p.planCostVer2.label(p), nil
}

// getPlanCostVer2 returns the plan-cost of this sub-plan, which is:
// plan-cost = (child-cost + net-cost) / concurrency
// net-cost = rows * row-size * net-factor
func (p *PhysicalTableReader) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}

	rows := getCardinality(p.tablePlan, option.CostFlag)
	rowSize := getAvgRowSize(p.tablePlan.Stats(), p.tablePlan.Schema())
	netFactor := getTaskNetFactorVer2(p, taskType)
	concurrency := float64(p.ctx.GetSessionVars().DistSQLScanConcurrency())
	childType := property.CopSingleReadTaskType
	if p.StoreType == kv.TiFlash { // mpp protocol
		childType = property.MppTaskType
	}

	netCost := netCostVer2(option, rows, rowSize, netFactor)

	childCost, err := p.tablePlan.getPlanCostVer2(childType, option)
	if err != nil {
		return zeroCostVer2, err
	}

	p.planCostVer2 = divCostVer2(sumCostVer2(childCost, netCost), concurrency)
	p.planCostInit = true

	// consider tidb_enforce_mpp
	_, isMPP := p.tablePlan.(*PhysicalExchangeSender)
	if isMPP && p.ctx.GetSessionVars().IsMPPEnforced() &&
		!hasCostFlag(option.CostFlag, CostFlagRecalculate) { // show the real cost in explain-statements
		p.planCostVer2 = divCostVer2(p.planCostVer2, 1000000000)
	}
	return p.planCostVer2.label(p), nil
}

// getPlanCostVer2 returns the plan-cost of this sub-plan, which is:
// plan-cost = index-side-cost + (table-side-cost + double-read-cost) / double-read-concurrency
// index-side-cost = (index-child-cost + index-net-cost) / dist-concurrency # same with IndexReader
// table-side-cost = (table-child-cost + table-net-cost) / dist-concurrency # same with TableReader
// double-read-cost = double-read-request-cost + double-read-cpu-cost
// double-read-request-cost = double-read-tasks * request-factor
// double-read-cpu-cost = index-rows * cpu-factor
// double-read-tasks = index-rows / batch-size * task-per-batch # task-per-batch is a magic number now
func (p *PhysicalIndexLookUpReader) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}

	indexRows := getCardinality(p.indexPlan, option.CostFlag)
	tableRows := getCardinality(p.indexPlan, option.CostFlag)
	indexRowSize := getTblStats(p.indexPlan).GetAvgRowSize(p.ctx, p.indexPlan.Schema().Columns, true, false)
	tableRowSize := getTblStats(p.tablePlan).GetAvgRowSize(p.ctx, p.tablePlan.Schema().Columns, false, false)
	cpuFactor := getTaskCPUFactorVer2(p, taskType)
	netFactor := getTaskNetFactorVer2(p, taskType)
	requestFactor := getTaskRequestFactorVer2(p, taskType)
	distConcurrency := float64(p.ctx.GetSessionVars().DistSQLScanConcurrency())
	doubleReadConcurrency := float64(p.ctx.GetSessionVars().IndexLookupConcurrency())

	// index-side
	indexNetCost := netCostVer2(option, indexRows, indexRowSize, netFactor)
	indexChildCost, err := p.indexPlan.getPlanCostVer2(property.CopDoubleReadTaskType, option)
	if err != nil {
		return zeroCostVer2, err
	}
	indexSideCost := divCostVer2(sumCostVer2(indexNetCost, indexChildCost), distConcurrency)

	// table-side
	tableNetCost := netCostVer2(option, tableRows, tableRowSize, netFactor)
	tableChildCost, err := p.tablePlan.getPlanCostVer2(property.CopDoubleReadTaskType, option)
	if err != nil {
		return zeroCostVer2, err
	}
	tableSideCost := divCostVer2(sumCostVer2(tableNetCost, tableChildCost), distConcurrency)

	// double-read
	doubleReadCPUCost := newCostVer2(option, cpuFactor,
		indexRows*cpuFactor.Value,
		"double-read-cpu(%v*%v)", indexRows, cpuFactor)
	batchSize := float64(p.ctx.GetSessionVars().IndexLookupSize)
	taskPerBatch := 32.0 // TODO: remove this magic number
	doubleReadTasks := indexRows / batchSize * taskPerBatch
	doubleReadRequestCost := doubleReadCostVer2(option, doubleReadTasks, requestFactor)
	doubleReadCost := sumCostVer2(doubleReadCPUCost, doubleReadRequestCost)

	p.planCostVer2 = sumCostVer2(indexSideCost, divCostVer2(sumCostVer2(tableSideCost, doubleReadCost), doubleReadConcurrency))
	p.planCostInit = true
	return p.planCostVer2.label(p), nil
}

// getPlanCostVer2 returns the plan-cost of this sub-plan, which is:
// plan-cost = table-side-cost + sum(index-side-cost)
// index-side-cost = (index-child-cost + index-net-cost) / dist-concurrency # same with IndexReader
// table-side-cost = (table-child-cost + table-net-cost) / dist-concurrency # same with TableReader
func (p *PhysicalIndexMergeReader) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}

	netFactor := getTaskNetFactorVer2(p, taskType)
	distConcurrency := float64(p.ctx.GetSessionVars().DistSQLScanConcurrency())

	var tableSideCost costVer2
	if tablePath := p.tablePlan; tablePath != nil {
		rows := getCardinality(tablePath, option.CostFlag)
		rowSize := getAvgRowSize(tablePath.Stats(), tablePath.Schema())

		tableNetCost := netCostVer2(option, rows, rowSize, netFactor)
		tableChildCost, err := tablePath.getPlanCostVer2(taskType, option)
		if err != nil {
			return zeroCostVer2, err
		}
		tableSideCost = divCostVer2(sumCostVer2(tableNetCost, tableChildCost), distConcurrency)
	}

	indexSideCost := make([]costVer2, 0, len(p.partialPlans))
	for _, indexPath := range p.partialPlans {
		rows := getCardinality(indexPath, option.CostFlag)
		rowSize := getAvgRowSize(indexPath.Stats(), indexPath.Schema())

		indexNetCost := netCostVer2(option, rows, rowSize, netFactor)
		indexChildCost, err := indexPath.getPlanCostVer2(taskType, option)
		if err != nil {
			return zeroCostVer2, err
		}
		indexSideCost = append(indexSideCost,
			divCostVer2(sumCostVer2(indexNetCost, indexChildCost), distConcurrency))
	}
	sumIndexSideCost := sumCostVer2(indexSideCost...)

	p.planCostVer2 = sumCostVer2(tableSideCost, sumIndexSideCost)
	p.planCostInit = true
	return p.planCostVer2.label(p), nil
}

// getPlanCostVer2 returns the plan-cost of this sub-plan, which is:
// plan-cost = child-cost + sort-cpu-cost + sort-mem-cost + sort-disk-cost
// sort-cpu-cost = rows * log2(rows) * len(sort-items) * cpu-factor
// if no spill:
// 1. sort-mem-cost = rows * row-size * mem-factor
// 2. sort-disk-cost = 0
// else if spill:
// 1. sort-mem-cost = mem-quota * mem-factor
// 2. sort-disk-cost = rows * row-size * disk-factor
func (p *PhysicalSort) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}

	rows := math.Max(getCardinality(p.children[0], option.CostFlag), 1)
	rowSize := getAvgRowSize(p.statsInfo(), p.Schema())
	cpuFactor := getTaskCPUFactorVer2(p, taskType)
	memFactor := getTaskMemFactorVer2(p, taskType)
	diskFactor := defaultVer2Factors.TiDBDisk
	oomUseTmpStorage := variable.EnableTmpStorageOnOOM.Load()
	memQuota := p.ctx.GetSessionVars().StmtCtx.MemTracker.GetBytesLimit()
	spill := taskType == property.RootTaskType && // only TiDB can spill
		oomUseTmpStorage && // spill is enabled
		memQuota > 0 && // mem-quota is set
		rowSize*rows > float64(memQuota) // exceed the mem-quota

	sortCPUCost := orderCostVer2(option, rows, rows, p.ByItems, cpuFactor)

	var sortMemCost, sortDiskCost costVer2
	if !spill {
		sortMemCost = newCostVer2(option, memFactor,
			rows*rowSize*memFactor.Value,
			"sortMem(%v*%v*%v)", rows, rowSize, memFactor)
		sortDiskCost = zeroCostVer2
	} else {
		sortMemCost = newCostVer2(option, memFactor,
			float64(memQuota)*memFactor.Value,
			"sortMem(%v*%v)", memQuota, memFactor)
		sortDiskCost = newCostVer2(option, diskFactor,
			rows*rowSize*diskFactor.Value,
			"sortDisk(%v*%v*%v)", rows, rowSize, diskFactor)
	}

	childCost, err := p.children[0].getPlanCostVer2(taskType, option)
	if err != nil {
		return zeroCostVer2, err
	}

	p.planCostVer2 = sumCostVer2(childCost, sortCPUCost, sortMemCost, sortDiskCost)
	p.planCostInit = true
	return p.planCostVer2.label(p), nil
}

// getPlanCostVer2 returns the plan-cost of this sub-plan, which is:
// plan-cost = child-cost + topn-cpu-cost + topn-mem-cost
// topn-cpu-cost = rows * log2(N) * len(sort-items) * cpu-factor
// topn-mem-cost = N * row-size * mem-factor
func (p *PhysicalTopN) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}

	rows := getCardinality(p.children[0], option.CostFlag)
	N := math.Max(1, float64(p.Count+p.Offset))
	rowSize := getAvgRowSize(p.statsInfo(), p.Schema())
	cpuFactor := getTaskCPUFactorVer2(p, taskType)
	memFactor := getTaskMemFactorVer2(p, taskType)

	topNCPUCost := orderCostVer2(option, rows, N, p.ByItems, cpuFactor)
	topNMemCost := newCostVer2(option, memFactor,
		N*rowSize*memFactor.Value,
		"topMem(%v*%v*%v)", N, rowSize, memFactor)

	childCost, err := p.children[0].getPlanCostVer2(taskType, option)
	if err != nil {
		return zeroCostVer2, err
	}

	p.planCostVer2 = sumCostVer2(childCost, topNCPUCost, topNMemCost)
	p.planCostInit = true
	return p.planCostVer2.label(p), nil
}

// getPlanCostVer2 returns the plan-cost of this sub-plan, which is:
// plan-cost = child-cost + agg-cost + group-cost
func (p *PhysicalStreamAgg) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}

	rows := getCardinality(p.children[0], option.CostFlag)
	cpuFactor := getTaskCPUFactorVer2(p, taskType)

	aggCost := aggCostVer2(option, rows, p.AggFuncs, cpuFactor)
	groupCost := groupCostVer2(option, rows, p.GroupByItems, cpuFactor)

	childCost, err := p.children[0].getPlanCostVer2(taskType, option)
	if err != nil {
		return zeroCostVer2, err
	}

	p.planCostVer2 = sumCostVer2(childCost, aggCost, groupCost)
	p.planCostInit = true
	return p.planCostVer2.label(p), nil
}

// getPlanCostVer2 returns the plan-cost of this sub-plan, which is:
// plan-cost = child-cost + (agg-cost + group-cost + hash-build-cost + hash-probe-cost) / concurrency
func (p *PhysicalHashAgg) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}

	inputRows := getCardinality(p.children[0], option.CostFlag)
	outputRows := getCardinality(p, option.CostFlag)
	outputRowSize := getAvgRowSize(p.Stats(), p.Schema())
	cpuFactor := getTaskCPUFactorVer2(p, taskType)
	memFactor := getTaskMemFactorVer2(p, taskType)
	concurrency := float64(p.ctx.GetSessionVars().HashAggFinalConcurrency())

	aggCost := aggCostVer2(option, inputRows, p.AggFuncs, cpuFactor)
	groupCost := groupCostVer2(option, inputRows, p.GroupByItems, cpuFactor)
	hashBuildCost := hashBuildCostVer2(option, outputRows, outputRowSize, p.GroupByItems, cpuFactor, memFactor)
	hashProbeCost := hashProbeCostVer2(option, inputRows, p.GroupByItems, cpuFactor)

	childCost, err := p.children[0].getPlanCostVer2(taskType, option)
	if err != nil {
		return zeroCostVer2, err
	}

	p.planCostVer2 = sumCostVer2(childCost, divCostVer2(sumCostVer2(aggCost, groupCost, hashBuildCost, hashProbeCost), concurrency))
	p.planCostInit = true
	return p.planCostVer2.label(p), nil
}

// getPlanCostVer2 returns the plan-cost of this sub-plan, which is:
// plan-cost = left-child-cost + right-child-cost + filter-cost + group-cost
func (p *PhysicalMergeJoin) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}

	leftRows := getCardinality(p.children[0], option.CostFlag)
	rightRows := getCardinality(p.children[1], option.CostFlag)
	cpuFactor := getTaskCPUFactorVer2(p, taskType)

	filterCost := sumCostVer2(filterCostVer2(option, leftRows, p.LeftConditions, cpuFactor),
		filterCostVer2(option, rightRows, p.RightConditions, cpuFactor))
	groupCost := sumCostVer2(groupCostVer2(option, leftRows, cols2Exprs(p.LeftJoinKeys), cpuFactor),
		groupCostVer2(option, rightRows, cols2Exprs(p.LeftJoinKeys), cpuFactor))

	leftChildCost, err := p.children[0].getPlanCostVer2(taskType, option)
	if err != nil {
		return zeroCostVer2, err
	}
	rightChildCost, err := p.children[1].getPlanCostVer2(taskType, option)
	if err != nil {
		return zeroCostVer2, err
	}

	p.planCostVer2 = sumCostVer2(leftChildCost, rightChildCost, filterCost, groupCost)
	p.planCostInit = true
	return p.planCostVer2.label(p), nil
}

// getPlanCostVer2 returns the plan-cost of this sub-plan, which is:
// plan-cost = build-child-cost + probe-child-cost +
// build-hash-cost + build-filter-cost +
// (probe-filter-cost + probe-hash-cost) / concurrency
func (p *PhysicalHashJoin) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}

	build, probe := p.children[0], p.children[1]
	buildFilters, probeFilters := p.LeftConditions, p.RightConditions
	buildKeys, probeKeys := p.LeftJoinKeys, p.RightJoinKeys
	if (p.InnerChildIdx == 1 && !p.UseOuterToBuild) || (p.InnerChildIdx == 0 && p.UseOuterToBuild) {
		build, probe = probe, build
		buildFilters, probeFilters = probeFilters, buildFilters
	}
	buildRows := getCardinality(build, option.CostFlag)
	probeRows := getCardinality(probe, option.CostFlag)
	buildRowSize := getAvgRowSize(build.Stats(), build.Schema())
	tidbConcurrency := float64(p.Concurrency)
	mppConcurrency := float64(3) // TODO: remove this empirical value
	cpuFactor := getTaskCPUFactorVer2(p, taskType)
	memFactor := getTaskMemFactorVer2(p, taskType)

	buildFilterCost := filterCostVer2(option, buildRows, buildFilters, cpuFactor)
	buildHashCost := hashBuildCostVer2(option, buildRows, buildRowSize, cols2Exprs(buildKeys), cpuFactor, memFactor)

	probeFilterCost := filterCostVer2(option, probeRows, probeFilters, cpuFactor)
	probeHashCost := hashProbeCostVer2(option, probeRows, cols2Exprs(probeKeys), cpuFactor)

	buildChildCost, err := build.getPlanCostVer2(taskType, option)
	if err != nil {
		return zeroCostVer2, err
	}
	probeChildCost, err := probe.getPlanCostVer2(taskType, option)
	if err != nil {
		return zeroCostVer2, err
	}

	if taskType == property.MppTaskType { // BCast or Shuffle Join, use mppConcurrency
		p.planCostVer2 = sumCostVer2(buildChildCost, probeChildCost,
			divCostVer2(sumCostVer2(buildHashCost, buildFilterCost, probeHashCost, probeFilterCost), mppConcurrency))
	} else { // TiDB HashJoin
		p.planCostVer2 = sumCostVer2(buildChildCost, probeChildCost, buildHashCost, buildFilterCost,
			divCostVer2(sumCostVer2(probeFilterCost, probeHashCost), tidbConcurrency))
	}
	p.planCostInit = true
	return p.planCostVer2.label(p), nil
}

// getPlanCostVer2 returns the plan-cost of this sub-plan, which is:
// plan-cost = build-child-cost + build-filter-cost +
// (probe-cost + probe-filter-cost) / concurrency
// probe-cost = probe-child-cost * build-rows / batchRatio
func (p *PhysicalIndexJoin) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}

	build, probe := p.children[1-p.InnerChildIdx], p.children[p.InnerChildIdx]
	buildRows := getCardinality(build, option.CostFlag)
	probeRowsOne := getCardinality(probe, option.CostFlag)
	probeRowsTot := probeRowsOne * buildRows
	buildFilters, probeFilters := p.LeftConditions, p.RightConditions
	probeConcurrency := float64(p.ctx.GetSessionVars().IndexLookupJoinConcurrency())
	cpuFactor := getTaskCPUFactorVer2(p, taskType)
	requestFactor := getTaskRequestFactorVer2(p, taskType)

	buildFilterCost := filterCostVer2(option, buildRows, buildFilters, cpuFactor)
	buildChildCost, err := build.getPlanCostVer2(taskType, option)
	if err != nil {
		return zeroCostVer2, err
	}

	probeFilterCost := filterCostVer2(option, probeRowsTot, probeFilters, cpuFactor)
	probeChildCost, err := probe.getPlanCostVer2(taskType, option)
	if err != nil {
		return zeroCostVer2, err
	}
	// IndexJoin executes a batch of rows at a time, so the actual cost of this part should be
	//  `innerCostPerBatch * numberOfBatches` instead of `innerCostPerRow * numberOfOuterRow`.
	// Use an empirical value batchRatio to handle this now.
	// TODO: remove this empirical value.
	batchRatio := 1024.0
	probeCost := divCostVer2(mulCostVer2(probeChildCost, buildRows), batchRatio)
	doubleReadCost := doubleReadCostVer2(option, buildRows/batchRatio, requestFactor)

	p.planCostVer2 = sumCostVer2(buildChildCost, buildFilterCost, divCostVer2(sumCostVer2(probeCost, probeFilterCost, doubleReadCost), probeConcurrency))
	p.planCostInit = true
	return p.planCostVer2.label(p), nil
}

func (p *PhysicalIndexHashJoin) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	// TODO: distinguish IndexHashJoin with IndexJoin
	return p.PhysicalIndexJoin.getPlanCostVer2(taskType, option)
}

func (p *PhysicalIndexMergeJoin) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	// TODO: distinguish IndexMergeJoin with IndexJoin
	return p.PhysicalIndexJoin.getPlanCostVer2(taskType, option)
}

// getPlanCostVer2 returns the plan-cost of this sub-plan, which is:
// plan-cost = build-child-cost + build-filter-cost + probe-cost + probe-filter-cost
// probe-cost = probe-child-cost * build-rows
func (p *PhysicalApply) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}

	buildRows := getCardinality(p.children[0], option.CostFlag)
	probeRowsOne := getCardinality(p.children[1], option.CostFlag)
	probeRowsTot := buildRows * probeRowsOne
	cpuFactor := getTaskCPUFactorVer2(p, taskType)

	buildFilterCost := filterCostVer2(option, buildRows, p.LeftConditions, cpuFactor)
	buildChildCost, err := p.children[0].getPlanCostVer2(taskType, option)
	if err != nil {
		return zeroCostVer2, err
	}

	probeFilterCost := filterCostVer2(option, probeRowsTot, p.RightConditions, cpuFactor)
	probeChildCost, err := p.children[1].getPlanCostVer2(taskType, option)
	if err != nil {
		return zeroCostVer2, err
	}
	probeCost := mulCostVer2(probeChildCost, buildRows)

	p.planCostVer2 = sumCostVer2(buildChildCost, buildFilterCost, probeCost, probeFilterCost)
	p.planCostInit = true
	return p.planCostVer2.label(p), nil
}

// getPlanCostVer2 calculates the cost of the plan if it has not been calculated yet and returns the cost.
// plan-cost = sum(child-cost) / concurrency
func (p *PhysicalUnionAll) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}

	concurrency := float64(p.ctx.GetSessionVars().UnionConcurrency())
	childCosts := make([]costVer2, 0, len(p.children))
	for _, child := range p.children {
		childCost, err := child.getPlanCostVer2(taskType, option)
		if err != nil {
			return zeroCostVer2, err
		}
		childCosts = append(childCosts, childCost)
	}
	p.planCostVer2 = divCostVer2(sumCostVer2(childCosts...), concurrency)
	p.planCostInit = true
	return p.planCostVer2.label(p), nil
}

// getPlanCostVer2 returns the plan-cost of this sub-plan, which is:
// plan-cost = child-cost + net-cost
func (p *PhysicalExchangeReceiver) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}

	rows := getCardinality(p, option.CostFlag)
	rowSize := getAvgRowSize(p.stats, p.Schema())
	netFactor := getTaskNetFactorVer2(p, taskType)
	isBCast := false
	if sender, ok := p.children[0].(*PhysicalExchangeSender); ok {
		isBCast = sender.ExchangeType == tipb.ExchangeType_Broadcast
	}
	numNode := float64(3) // TODO: remove this empirical value

	netCost := netCostVer2(option, rows, rowSize, netFactor)
	if isBCast {
		netCost = mulCostVer2(netCost, numNode)
	}
	childCost, err := p.children[0].getPlanCostVer2(taskType, option)
	if err != nil {
		return zeroCostVer2, err
	}

	p.planCostVer2 = sumCostVer2(childCost, netCost)
	p.planCostInit = true
	return p.planCostVer2.label(p), nil
}

// getPlanCostVer2 returns the plan-cost of this sub-plan, which is:
func (p *PointGetPlan) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}

	if p.accessCols == nil { // from fast plan code path
		p.planCostVer2 = zeroCostVer2
		p.planCostInit = true
		return zeroCostVer2, nil
	}
	rowSize := getAvgRowSize(p.stats, p.schema)
	netFactor := getTaskNetFactorVer2(p, taskType)

	p.planCostVer2 = netCostVer2(option, 1, rowSize, netFactor)
	p.planCostInit = true
	return p.planCostVer2.label(p), nil
}

// getPlanCostVer2 returns the plan-cost of this sub-plan, which is:
func (p *BatchPointGetPlan) getPlanCostVer2(taskType property.TaskType, option *PlanCostOption) (costVer2, error) {
	if p.planCostInit && !hasCostFlag(option.CostFlag, CostFlagRecalculate) {
		return p.planCostVer2, nil
	}

	if p.accessCols == nil { // from fast plan code path
		p.planCostVer2 = zeroCostVer2
		p.planCostInit = true
		return zeroCostVer2, nil
	}
	rows := getCardinality(p, option.CostFlag)
	rowSize := getAvgRowSize(p.stats, p.schema)
	netFactor := getTaskNetFactorVer2(p, taskType)

	p.planCostVer2 = netCostVer2(option, rows, rowSize, netFactor)
	p.planCostInit = true
	return p.planCostVer2.label(p), nil
}

func scanCostVer2(option *PlanCostOption, rows, rowSize float64, scanFactor costVer2Factor) costVer2 {
	return newCostVer2(option, scanFactor,
		// rows * log(row-size) * scanFactor, log2 from experiments
		rows*math.Log2(math.Max(1, rowSize))*scanFactor.Value,
		"scan(%v*logrowsize(%v)*%v)", rows, rowSize, scanFactor)
}

func netCostVer2(option *PlanCostOption, rows, rowSize float64, netFactor costVer2Factor) costVer2 {
	return newCostVer2(option, netFactor,
		rows*rowSize*netFactor.Value,
		"net(%v*rowsize(%v)*%v)", rows, rowSize, netFactor)
}

func filterCostVer2(option *PlanCostOption, rows float64, filters []expression.Expression, cpuFactor costVer2Factor) costVer2 {
	numFuncs := numFunctions(filters)
	return newCostVer2(option, cpuFactor,
		rows*float64(numFuncs)*cpuFactor.Value,
		"cpu(%v*filters(%v)*%v)", rows, numFuncs, cpuFactor)
}

func aggCostVer2(option *PlanCostOption, rows float64, aggFuncs []*aggregation.AggFuncDesc, cpuFactor costVer2Factor) costVer2 {
	return newCostVer2(option, cpuFactor,
		// TODO: consider types of agg-funcs
		rows*float64(len(aggFuncs))*cpuFactor.Value,
		"agg(%v*aggs(%v)*%v)", rows, len(aggFuncs), cpuFactor)
}

func groupCostVer2(option *PlanCostOption, rows float64, groupItems []expression.Expression, cpuFactor costVer2Factor) costVer2 {
	numFuncs := numFunctions(groupItems)
	return newCostVer2(option, cpuFactor,
		rows*float64(numFuncs)*cpuFactor.Value,
		"group(%v*cols(%v)*%v)", rows, numFuncs, cpuFactor)
}

func numFunctions(exprs []expression.Expression) int {
	num := 0
	for _, e := range exprs {
		if _, ok := e.(*expression.ScalarFunction); ok {
			num++
		}
	}
	return num
}

func orderCostVer2(option *PlanCostOption, rows, N float64, byItems []*util.ByItems, cpuFactor costVer2Factor) costVer2 {
	numFuncs := 0
	for _, byItem := range byItems {
		if _, ok := byItem.Expr.(*expression.ScalarFunction); ok {
			numFuncs++
		}
	}
	exprCost := newCostVer2(option, cpuFactor,
		rows*float64(numFuncs)*cpuFactor.Value,
		"exprCPU(%v*%v*%v)", rows, numFuncs, cpuFactor)
	orderCost := newCostVer2(option, cpuFactor,
		rows*math.Log2(N)*cpuFactor.Value,
		"orderCPU(%v*log(%v)*%v)", rows, N, cpuFactor)
	return sumCostVer2(exprCost, orderCost)
}

func hashBuildCostVer2(option *PlanCostOption, buildRows, buildRowSize float64, keys []expression.Expression, cpuFactor, memFactor costVer2Factor) costVer2 {
	// TODO: 1) consider types of keys, 2) dedicated factor for build-probe hash table
	hashKeyCost := newCostVer2(option, cpuFactor,
		buildRows*float64(len(keys))*cpuFactor.Value,
		"hashkey(%v*%v*%v)", buildRows, len(keys), cpuFactor)
	hashMemCost := newCostVer2(option, memFactor,
		buildRows*buildRowSize*memFactor.Value,
		"hashmem(%v*%v*%v)", buildRows, buildRowSize, memFactor)
	hashBuildCost := newCostVer2(option, cpuFactor,
		buildRows*cpuFactor.Value,
		"hashbuild(%v*%v)", buildRows, cpuFactor)
	return sumCostVer2(hashKeyCost, hashMemCost, hashBuildCost)
}

func hashProbeCostVer2(option *PlanCostOption, probeRows float64, keys []expression.Expression, cpuFactor costVer2Factor) costVer2 {
	// TODO: 1) consider types of keys, 2) dedicated factor for build-probe hash table
	hashKeyCost := newCostVer2(option, cpuFactor,
		probeRows*float64(len(keys))*cpuFactor.Value,
		"hashkey(%v*%v*%v)", probeRows, len(keys), cpuFactor)
	hashProbeCost := newCostVer2(option, cpuFactor,
		probeRows*cpuFactor.Value,
		"hashprobe(%v*%v)", probeRows, cpuFactor)
	return sumCostVer2(hashKeyCost, hashProbeCost)
}

// For simplicity and robust, only operators that need double-read like IndexLookup and IndexJoin consider this cost.
func doubleReadCostVer2(option *PlanCostOption, numTasks float64, requestFactor costVer2Factor) costVer2 {
	return newCostVer2(option, requestFactor,
		numTasks*requestFactor.Value,
		"doubleRead(tasks(%v)*%v)", numTasks, requestFactor)
}

type costVer2Factor struct {
	Name  string
	Value float64
}

func (f costVer2Factor) String() string {
	return fmt.Sprintf("%s(%v)", f.Name, f.Value)
}

// In Cost Ver2, we hide cost factors from users and deprecate SQL variables like `tidb_opt_scan_factor`.
type costVer2Factors struct {
	TiDBTemp      costVer2Factor // operations on TiDB temporary table
	TiKVScan      costVer2Factor // per byte
	TiKVDescScan  costVer2Factor // per byte
	TiFlashScan   costVer2Factor // per byte
	TiDBCPU       costVer2Factor // per column or expression
	TiKVCPU       costVer2Factor // per column or expression
	TiFlashCPU    costVer2Factor // per column or expression
	TiDB2KVNet    costVer2Factor // per byte
	TiDB2FlashNet costVer2Factor // per byte
	TiFlashMPPNet costVer2Factor // per byte
	TiDBMem       costVer2Factor // per byte
	TiKVMem       costVer2Factor // per byte
	TiFlashMem    costVer2Factor // per byte
	TiDBDisk      costVer2Factor // per byte
	TiDBRequest   costVer2Factor // per net request
}

func (c costVer2Factors) tolist() (l []costVer2Factor) {
	return append(l, c.TiDBTemp, c.TiKVScan, c.TiKVDescScan, c.TiFlashScan, c.TiDBCPU, c.TiKVCPU, c.TiFlashCPU,
		c.TiDB2KVNet, c.TiDB2FlashNet, c.TiFlashMPPNet, c.TiDBMem, c.TiKVMem, c.TiFlashMem, c.TiDBDisk, c.TiDBRequest)
}

var defaultVer2Factors = costVer2Factors{
	TiDBTemp:      costVer2Factor{"tidb_temp_table_factor", 0},
	TiKVScan:      costVer2Factor{"tikv_scan_factor", 100},
	TiKVDescScan:  costVer2Factor{"tikv_desc_scan_factor", 150},
	TiFlashScan:   costVer2Factor{"tiflash_scan_factor", 5},
	TiDBCPU:       costVer2Factor{"tidb_cpu_factor", 30},
	TiKVCPU:       costVer2Factor{"tikv_cpu_factor", 30},
	TiFlashCPU:    costVer2Factor{"tiflash_cpu_factor", 5},
	TiDB2KVNet:    costVer2Factor{"tidb_kv_net_factor", 8},
	TiDB2FlashNet: costVer2Factor{"tidb_flash_net_factor", 4},
	TiFlashMPPNet: costVer2Factor{"tiflash_mpp_net_factor", 4},
	TiDBMem:       costVer2Factor{"tidb_mem_factor", 1},
	TiKVMem:       costVer2Factor{"tikv_mem_factor", 1},
	TiFlashMem:    costVer2Factor{"tiflash_mem_factor", 1},
	TiDBDisk:      costVer2Factor{"tidb_disk_factor", 1000},
	TiDBRequest:   costVer2Factor{"tidb_request_factor", 9500000},
}

func getTaskCPUFactorVer2(p PhysicalPlan, taskType property.TaskType) costVer2Factor {
	switch taskType {
	case property.RootTaskType: // TiDB
		return defaultVer2Factors.TiDBCPU
	case property.MppTaskType: // TiFlash
		return defaultVer2Factors.TiFlashCPU
	default: // TiKV
		return defaultVer2Factors.TiKVCPU
	}
}

func getTaskMemFactorVer2(p PhysicalPlan, taskType property.TaskType) costVer2Factor {
	switch taskType {
	case property.RootTaskType: // TiDB
		return defaultVer2Factors.TiDBMem
	case property.MppTaskType: // TiFlash
		return defaultVer2Factors.TiFlashMem
	default: // TiKV
		return defaultVer2Factors.TiKVMem
	}
}

func getTaskScanFactorVer2(p PhysicalPlan, taskType property.TaskType) costVer2Factor {
	if isTemporaryTable(getTableInfo(p)) {
		return defaultVer2Factors.TiDBTemp
	}
	switch taskType {
	case property.MppTaskType: // TiFlash
		return defaultVer2Factors.TiFlashScan
	default: // TiKV
		var desc bool
		if indexScan, ok := p.(*PhysicalIndexScan); ok {
			desc = indexScan.Desc
		}
		if tableScan, ok := p.(*PhysicalTableScan); ok {
			desc = tableScan.Desc
		}
		if desc {
			return defaultVer2Factors.TiKVDescScan
		}
		return defaultVer2Factors.TiKVScan
	}
}

func getTaskNetFactorVer2(p PhysicalPlan, _ property.TaskType) costVer2Factor {
	if isTemporaryTable(getTableInfo(p)) {
		return defaultVer2Factors.TiDBTemp
	}
	if _, ok := p.(*PhysicalExchangeReceiver); ok { // TiFlash MPP
		return defaultVer2Factors.TiFlashMPPNet
	}
	if tblReader, ok := p.(*PhysicalTableReader); ok {
		if _, isMPP := tblReader.tablePlan.(*PhysicalExchangeSender); isMPP { // TiDB to TiFlash with mpp protocol
			return defaultVer2Factors.TiDB2FlashNet
		}
	}
	return defaultVer2Factors.TiDB2KVNet
}

func getTaskRequestFactorVer2(p PhysicalPlan, _ property.TaskType) costVer2Factor {
	if isTemporaryTable(getTableInfo(p)) {
		return defaultVer2Factors.TiDBTemp
	}
	return defaultVer2Factors.TiDBRequest
}

func isTemporaryTable(tbl *model.TableInfo) bool {
	return tbl != nil && tbl.TempTableType != model.TempTableNone
}

func getTableInfo(p PhysicalPlan) *model.TableInfo {
	switch x := p.(type) {
	case *PhysicalIndexReader:
		return getTableInfo(x.indexPlan)
	case *PhysicalTableReader:
		return getTableInfo(x.tablePlan)
	case *PhysicalIndexLookUpReader:
		return getTableInfo(x.tablePlan)
	case *PhysicalIndexMergeReader:
		if x.tablePlan != nil {
			return getTableInfo(x.tablePlan)
		}
		return getTableInfo(x.partialPlans[0])
	case *PhysicalTableScan:
		return x.Table
	case *PhysicalIndexScan:
		return x.Table
	default:
		if len(x.Children()) == 0 {
			return nil
		}
		return getTableInfo(x.Children()[0])
	}
}

func cols2Exprs(cols []*expression.Column) []expression.Expression {
	exprs := make([]expression.Expression, 0, len(cols))
	for _, c := range cols {
		exprs = append(exprs, c)
	}
	return exprs
}

type costVer2 struct {
	cost        float64
	trace       bool               // Whether to trace the cost calculation.
	factorCosts map[string]float64 // map[factorName]cost, used to calibrate the cost model
	formula     string             // It used to trace the cost calculation.
}

func (c costVer2) label(p PhysicalPlan) costVer2 {
	if !c.trace {
		return c
	}
	c.formula = p.ExplainID().String()
	return c
}

func traceCost(option *PlanCostOption) bool {
	if option != nil && hasCostFlag(option.CostFlag, CostFlagTrace) {
		return true
	}
	return false
}

func newZeroCostVer2(trace bool) (ret costVer2) {
	if trace {
		ret.trace = true
		ret.factorCosts = make(map[string]float64)
		ret.formula = "0"
	}
	return
}

func newCostVer2(option *PlanCostOption, factor costVer2Factor, cost float64,
	formulaFormat string, formulaArgs ...any) costVer2 {
	ret := newZeroCostVer2(traceCost(option))
	ret.cost = cost
	if ret.trace {
		ret.factorCosts[factor.Name] = cost
		ret.formula = fmt.Sprintf(formulaFormat, formulaArgs...)
	}
	return ret
}

func sumCostVer2(costs ...costVer2) costVer2 {
	if len(costs) == 0 {
		return newZeroCostVer2(false)
	}
	ret := newZeroCostVer2(costs[0].trace)
	var subFormulas []string
	for _, c := range costs {
		ret.cost += c.cost
		if ret.trace {
			for factor, factorCost := range c.factorCosts {
				ret.factorCosts[factor] += factorCost
			}
			subFormulas = append(subFormulas, fmt.Sprintf("(%v)", c.formula))
		}
	}
	if ret.trace {
		ret.formula = strings.Join(subFormulas, " + ")
	}
	return ret
}

func divCostVer2(cost costVer2, denominator float64) costVer2 {
	ret := newZeroCostVer2(cost.trace)
	ret.cost = cost.cost / denominator
	if ret.trace {
		for f, c := range cost.factorCosts {
			ret.factorCosts[f] = c / denominator
		}
		ret.formula = fmt.Sprintf("(%v)/%v", cost.formula, denominator)
	}
	return ret
}

func mulCostVer2(cost costVer2, scale float64) costVer2 {
	ret := newZeroCostVer2(cost.trace)
	ret.cost = cost.cost * scale
	if ret.trace {
		for f, c := range cost.factorCosts {
			ret.factorCosts[f] = c * scale
		}
		ret.formula = fmt.Sprintf("(%v)*%v", cost.formula, scale)
	}
	return ret
}

var zeroCostVer2 = newZeroCostVer2(false)
