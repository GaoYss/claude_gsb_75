package repair_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"streetlight/internal/apperr"
	"streetlight/internal/modules/fault"
	"streetlight/internal/modules/lamp"
	"streetlight/internal/modules/repair"
)

// stateSnapshot 是一次失败操作前后的完整业务状态快照,
// 任何被拒绝的操作都必须让快照保持字节级一致。
type stateSnapshot struct {
	lampRunStatus   string
	faultStatus     string
	faultRepairCnt  int
	latestRepairID  *uint
	faultClosedAt   *time.Time
	repairsTotal    int64
	repairsOngoing  int64
	repairsFinished int64
	repairTotalCost float64
}

func (h *harness) snapshot(t *testing.T, lampID, faultID uint) stateSnapshot {
	t.Helper()
	ctx := context.Background()

	device, err := h.lamps.Get(ctx, lampID)
	require.NoError(t, err)
	entity, err := h.faults.GetByID(ctx, faultID)
	require.NoError(t, err)
	stats, err := h.repairs.Statistics(ctx)
	require.NoError(t, err)

	return stateSnapshot{
		lampRunStatus:   device.RunStatus,
		faultStatus:     entity.Status,
		faultRepairCnt:  entity.RepairCount,
		latestRepairID:  entity.LatestRepairID,
		faultClosedAt:   entity.ClosedAt,
		repairsTotal:    stats.Total,
		repairsOngoing:  stats.OngoingTotal,
		repairsFinished: stats.FinishedTotal,
		repairTotalCost: stats.TotalCost,
	}
}

// requireBadRequest 断言错误是 400 参数/业务校验失败。
func requireBadRequest(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	businessErr, ok := apperr.As(err)
	require.True(t, ok, "期望业务错误, 实际: %v", err)
	require.Equal(t, http.StatusBadRequest, businessErr.Status, "错误信息: %s", businessErr.Message)
}

// requireNotFound 断言错误是 404。
func requireNotFound(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	businessErr, ok := apperr.As(err)
	require.True(t, ok, "期望业务错误, 实际: %v", err)
	require.Equal(t, http.StatusNotFound, businessErr.Status, "错误信息: %s", businessErr.Message)
}

func fmtTime(at time.Time) string {
	return at.Format("2006-01-02 15:04:05")
}

// TestIllegalTransitionsRejected 覆盖状态机不允许的全部流转方向,
// 包括终态再操作、跨阶段跳转与非法参数。
func TestIllegalTransitionsRejected(t *testing.T) {
	ctx := context.Background()

	t.Run("待处理故障不能直接标记修复", func(t *testing.T) {
		h := newHarness(t)
		device := h.createLamp(t, "LD-R-001")
		entity := h.createFault(t, device.ID, "未开工直接修复")

		// 没有维修记录时直接通知"维修完成"是非法跳转。
		err := h.faults.OnRepairFinished(ctx, entity.ID, true)
		requireConflict(t, err)

		// 故障仍在待处理, 路灯仍是故障态。
		current, err := h.faults.GetByID(ctx, entity.ID)
		require.NoError(t, err)
		require.Equal(t, fault.StatusPending, current.Status)
		lampEntity, err := h.lamps.Get(ctx, device.ID)
		require.NoError(t, err)
		require.Equal(t, lamp.RunStatusFault, lampEntity.RunStatus)
	})

	t.Run("已修复故障不能重复完工推进", func(t *testing.T) {
		h := newHarness(t)
		device := h.createLamp(t, "LD-R-002")
		entity := h.createFault(t, device.ID, "修复后重复完工")
		record, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工甲"})
		require.NoError(t, err)
		_, err = h.repairs.Finish(ctx, record.ID, repair.FinishRequest{Result: repair.ResultFixed})
		require.NoError(t, err)

		// 故障已是已修复, 再次以"已修复"推进没有合法源状态。
		err = h.faults.OnRepairFinished(ctx, entity.ID, true)
		requireConflict(t, err)
	})

	t.Run("已关闭故障拒绝一切再操作", func(t *testing.T) {
		h := newHarness(t)
		device := h.createLamp(t, "LD-R-003")
		entity := h.createFault(t, device.ID, "误报关闭")
		_, err := h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "误报作废"})
		require.NoError(t, err)

		// 再次关闭 / 修改 / 删除(无维修记录可删但删除本身针对终态) / 开工维修 全部拒绝。
		_, err = h.faults.Close(ctx, entity.ID, fault.CloseRequest{})
		requireConflict(t, err)
		_, err = h.faults.Update(ctx, entity.ID, fault.UpdateRequest{Description: strPtr("想改描述")})
		requireConflict(t, err)
		_, err = h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工乙"})
		requireConflict(t, err)

		// 已关闭且无维修记录允许删除(业务上的作废清理), 但关闭后再开工绝不允许。
		current, err := h.faults.GetByID(ctx, entity.ID)
		require.NoError(t, err)
		require.Equal(t, fault.StatusClosed, current.Status)
		require.NotNil(t, current.ClosedAt)
	})

	t.Run("已完成维修记录拒绝修改与重复完工", func(t *testing.T) {
		h := newHarness(t)
		device := h.createLamp(t, "LD-R-004")
		entity := h.createFault(t, device.ID, "完工后再操作")
		record, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工丙"})
		require.NoError(t, err)
		_, err = h.repairs.Finish(ctx, record.ID, repair.FinishRequest{Result: repair.ResultFixed})
		require.NoError(t, err)

		_, err = h.repairs.Update(ctx, record.ID, repair.UpdateRequest{Content: strPtr("改内容")})
		requireConflict(t, err)
		_, err = h.repairs.Finish(ctx, record.ID, repair.FinishRequest{Result: repair.ResultPendingParts})
		requireConflict(t, err)
	})

	t.Run("非法维修结果与非法时间被拒", func(t *testing.T) {
		h := newHarness(t)
		device := h.createLamp(t, "LD-R-005")
		entity := h.createFault(t, device.ID, "参数校验")
		record, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工丁"})
		require.NoError(t, err)

		// 非法结果。
		_, err = h.repairs.Finish(ctx, record.ID, repair.FinishRequest{Result: "不存在"})
		requireBadRequest(t, err)

		// 完工早于开工。
		_, err = h.repairs.Finish(ctx, record.ID, repair.FinishRequest{
			Result:     repair.ResultFixed,
			FinishedAt: fmtTime(record.StartedAt.Add(-time.Hour)),
		})
		requireBadRequest(t, err)

		// 开工早于故障上报: 参数校验先于进行中冲突, 返回 400。
		_, err = h.repairs.Create(ctx, repair.CreateRequest{
			FaultID:   entity.ID,
			Repairman: "维修工戊",
			StartedAt: fmtTime(entity.ReportedAt.Add(-time.Hour)),
		})
		requireBadRequest(t, err)

		// 时间合法但已有进行中记录时才是 409。
		_, err = h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工戊"})
		requireConflict(t, err)
	})

	t.Run("删除不存在的资源返回404", func(t *testing.T) {
		h := newHarness(t)
		requireNotFound(t, h.repairs.Delete(ctx, 99999))
		_, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: 99999, Repairman: "维修工己"})
		requireNotFound(t, err)
	})
}

func strPtr(value string) *string { return &value }

// TestSameStatusDuplicateSubmissionRejected 验证停留在同一状态时的重复提交。
func TestSameStatusDuplicateSubmissionRejected(t *testing.T) {
	ctx := context.Background()

	t.Run("待处理重复登记故障", func(t *testing.T) {
		h := newHarness(t)
		device := h.createLamp(t, "LD-R-010")
		h.createFault(t, device.ID, "第一条")

		_, err := h.faults.Create(ctx, fault.CreateRequest{
			LampID: device.ID, FaultType: "灯不亮", Description: "重复登记",
		})
		requireConflict(t, err)

		// 连续第三次仍然被拒, 结论稳定。
		_, err = h.faults.Create(ctx, fault.CreateRequest{
			LampID: device.ID, FaultType: "灯不亮", Description: "再次重复登记",
		})
		requireConflict(t, err)

		list, err := h.faults.ListByLamp(ctx, device.ID)
		require.NoError(t, err)
		require.Len(t, list, 1)
	})

	t.Run("维修中重复开工", func(t *testing.T) {
		h := newHarness(t)
		device := h.createLamp(t, "LD-R-011")
		entity := h.createFault(t, device.ID, "重复开工")
		first, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工甲"})
		require.NoError(t, err)

		_, err = h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工乙"})
		requireConflict(t, err)

		current, err := h.faults.GetByID(ctx, entity.ID)
		require.NoError(t, err)
		require.Equal(t, 1, current.RepairCount)
		require.Equal(t, first.ID, *current.LatestRepairID)

		records, err := h.repairs.ListByFault(ctx, entity.ID)
		require.NoError(t, err)
		require.Len(t, records, 1)
	})

	t.Run("重复关闭与重复完工", func(t *testing.T) {
		h := newHarness(t)
		device := h.createLamp(t, "LD-R-012")
		entity := h.createFault(t, device.ID, "重复关闭")
		record, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工甲"})
		require.NoError(t, err)
		_, err = h.repairs.Finish(ctx, record.ID, repair.FinishRequest{Result: repair.ResultFixed})
		require.NoError(t, err)

		for i := 0; i < 3; i++ {
			_, err = h.repairs.Finish(ctx, record.ID, repair.FinishRequest{Result: repair.ResultFixed})
			requireConflict(t, err, "第 %d 次重复完工应被拒", i+1)
		}

		closed, err := h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "闭环"})
		require.NoError(t, err)
		closedAt := *closed.ClosedAt
		for i := 0; i < 3; i++ {
			_, err = h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "再次关闭"})
			requireConflict(t, err, "第 %d 次重复关闭应被拒", i+1)
		}

		// closed_at 不被重复提交覆盖(用 Equal 比较, 忽略 Location 表示差异)。
		current, err := h.faults.GetByID(ctx, entity.ID)
		require.NoError(t, err)
		require.True(t, closedAt.Equal(*current.ClosedAt))
		require.Equal(t, "闭环", current.CloseRemark)
	})
}

// TestRepairResultStateProgression 覆盖四种维修结果对故障状态的不同推进。
func TestRepairResultStateProgression(t *testing.T) {
	ctx := context.Background()

	// 非"已修复"的三种结果都应让故障保持维修中, 并允许后续返修。
	nonFixedResults := []struct {
		name   string
		result string
	}{
		{"待配件", repair.ResultPendingParts},
		{"观察中", repair.ResultObserving},
		{"无法修复", repair.ResultUnfixable},
	}
	for _, item := range nonFixedResults {
		t.Run("非修复结果保持维修中_"+item.name, func(t *testing.T) {
			h := newHarness(t)
			device := h.createLamp(t, "LD-R-020-"+item.result)
			entity := h.createFault(t, device.ID, item.name+"结果")

			first, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工甲"})
			require.NoError(t, err)
			_, err = h.repairs.Finish(ctx, first.ID, repair.FinishRequest{Result: item.result})
			require.NoError(t, err)

			after, err := h.faults.GetByID(ctx, entity.ID)
			require.NoError(t, err)
			require.Equal(t, fault.StatusProcessing, after.Status, "结果 %s 不应推进故障", item.result)
			deviceAfter, err := h.lamps.Get(ctx, device.ID)
			require.NoError(t, err)
			require.Equal(t, lamp.RunStatusMaintenance, deviceAfter.RunStatus)

			// 上一条已完工, 允许登记下一次维修(返修)。
			second, err := h.repairs.Create(ctx, repair.CreateRequest{
				FaultID: entity.ID, Repairman: "维修工乙", Content: "二次到场",
			})
			require.NoError(t, err)
			require.Equal(t, repair.StatusOngoing, second.Status)
			afterSecond, err := h.faults.GetByID(ctx, entity.ID)
			require.NoError(t, err)
			require.Equal(t, 2, afterSecond.RepairCount)
			require.Equal(t, second.ID, *afterSecond.LatestRepairID)
		})
	}

	t.Run("已修复推进故障并允许返修回退", func(t *testing.T) {
		h := newHarness(t)
		device := h.createLamp(t, "LD-R-030")
		entity := h.createFault(t, device.ID, "修复后返修")

		first, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工甲"})
		require.NoError(t, err)
		_, err = h.repairs.Finish(ctx, first.ID, repair.FinishRequest{Result: repair.ResultFixed})
		require.NoError(t, err)

		after, err := h.faults.GetByID(ctx, entity.ID)
		require.NoError(t, err)
		require.Equal(t, fault.StatusRepaired, after.Status)
		deviceAfter, err := h.lamps.Get(ctx, device.ID)
		require.NoError(t, err)
		require.Equal(t, lamp.RunStatusNormal, deviceAfter.RunStatus, "已修复后路灯恢复正常")

		// 已修复 -> 维修中 的返修回退是状态机明确允许的路径。
		second, err := h.repairs.Create(ctx, repair.CreateRequest{
			FaultID: entity.ID, Repairman: "维修工乙", Content: "返修: 问题复现",
		})
		require.NoError(t, err)
		afterSecond, err := h.faults.GetByID(ctx, entity.ID)
		require.NoError(t, err)
		require.Equal(t, fault.StatusProcessing, afterSecond.Status)
		require.Equal(t, 2, afterSecond.RepairCount)
		require.Equal(t, second.ID, *afterSecond.LatestRepairID)
		deviceAfterSecond, err := h.lamps.Get(ctx, device.ID)
		require.NoError(t, err)
		require.Equal(t, lamp.RunStatusMaintenance, deviceAfterSecond.RunStatus)

		// 二次维修修复后再次进入已修复, 然后闭环。
		_, err = h.repairs.Finish(ctx, second.ID, repair.FinishRequest{Result: repair.ResultFixed})
		require.NoError(t, err)
		closed, err := h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "复核通过"})
		require.NoError(t, err)
		require.Equal(t, fault.StatusClosed, closed.Status)

		// 闭环后同一路灯可以登记新故障。
		newFault, err := h.faults.Create(ctx, fault.CreateRequest{
			LampID: device.ID, FaultType: "灯不亮", Description: "新的问题",
		})
		require.NoError(t, err)
		require.Equal(t, fault.StatusPending, newFault.Status)
	})

	t.Run("无法修复可作废关闭", func(t *testing.T) {
		h := newHarness(t)
		device := h.createLamp(t, "LD-R-031")
		entity := h.createFault(t, device.ID, "无法修复后关闭")
		record, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工甲"})
		require.NoError(t, err)
		_, err = h.repairs.Finish(ctx, record.ID, repair.FinishRequest{Result: repair.ResultUnfixable})
		require.NoError(t, err)

		// 维修中 -> 已关闭 合法(作废处理)。
		_, err = h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "无维修价值, 作废"})
		require.NoError(t, err)
	})
}

// TestDeleteRepairRollback 验证删除维修记录后次数、最近维修与路灯状态的回落。
func TestDeleteRepairRollback(t *testing.T) {
	ctx := context.Background()

	t.Run("删除进行中记录回退到待处理", func(t *testing.T) {
		h := newHarness(t)
		device := h.createLamp(t, "LD-R-040")
		entity := h.createFault(t, device.ID, "删除进行中")
		record, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工甲"})
		require.NoError(t, err)

		before := h.snapshot(t, device.ID, entity.ID)
		require.Equal(t, fault.StatusProcessing, before.faultStatus)

		require.NoError(t, h.repairs.Delete(ctx, record.ID))

		after, err := h.faults.GetByID(ctx, entity.ID)
		require.NoError(t, err)
		require.Equal(t, fault.StatusPending, after.Status, "次数归零后故障回退待处理")
		require.Equal(t, 0, after.RepairCount)
		require.Nil(t, after.LatestRepairID)
		deviceAfter, err := h.lamps.Get(ctx, device.ID)
		require.NoError(t, err)
		require.Equal(t, lamp.RunStatusFault, deviceAfter.RunStatus, "回退后路灯为故障态")

		records, err := h.repairs.ListByFault(ctx, entity.ID)
		require.NoError(t, err)
		require.Empty(t, records)
	})

	t.Run("删除已修复记录同样回退", func(t *testing.T) {
		h := newHarness(t)
		device := h.createLamp(t, "LD-R-041")
		entity := h.createFault(t, device.ID, "删除已完工")
		record, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工甲"})
		require.NoError(t, err)
		cost := 300.0
		_, err = h.repairs.Finish(ctx, record.ID, repair.FinishRequest{Result: repair.ResultFixed, Cost: &cost})
		require.NoError(t, err)
		require.Equal(t, fault.StatusRepaired, h.snapshot(t, device.ID, entity.ID).faultStatus)

		require.NoError(t, h.repairs.Delete(ctx, record.ID))

		after, err := h.faults.GetByID(ctx, entity.ID)
		require.NoError(t, err)
		require.Equal(t, fault.StatusPending, after.Status, "已修复状态在记录删除后也回退待处理")
		require.Equal(t, 0, after.RepairCount)
		require.Nil(t, after.LatestRepairID)
		deviceAfter, err := h.lamps.Get(ctx, device.ID)
		require.NoError(t, err)
		require.Equal(t, lamp.RunStatusFault, deviceAfter.RunStatus)
	})

	t.Run("删除最新记录后最近维修回指上一条", func(t *testing.T) {
		h := newHarness(t)
		device := h.createLamp(t, "LD-R-042")
		entity := h.createFault(t, device.ID, "两次维修删最新")

		first, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工甲"})
		require.NoError(t, err)
		_, err = h.repairs.Finish(ctx, first.ID, repair.FinishRequest{Result: repair.ResultPendingParts})
		require.NoError(t, err)
		second, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工乙"})
		require.NoError(t, err)

		require.NoError(t, h.repairs.Delete(ctx, second.ID))

		after, err := h.faults.GetByID(ctx, entity.ID)
		require.NoError(t, err)
		require.Equal(t, 1, after.RepairCount, "次数回落到 1")
		require.Equal(t, first.ID, *after.LatestRepairID, "最近维修回指第一条")
		require.Equal(t, fault.StatusProcessing, after.Status, "仍有维修记录, 保持维修中")
		deviceAfter, err := h.lamps.Get(ctx, device.ID)
		require.NoError(t, err)
		require.Equal(t, lamp.RunStatusMaintenance, deviceAfter.RunStatus)
	})

	t.Run("已关闭故障的维修记录禁止删除", func(t *testing.T) {
		h := newHarness(t)
		device := h.createLamp(t, "LD-R-043")
		entity := h.createFault(t, device.ID, "闭环后删除")
		record, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工甲"})
		require.NoError(t, err)
		_, err = h.repairs.Finish(ctx, record.ID, repair.FinishRequest{Result: repair.ResultFixed})
		require.NoError(t, err)
		_, err = h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "闭环"})
		require.NoError(t, err)

		before := h.snapshot(t, device.ID, entity.ID)
		err = h.repairs.Delete(ctx, record.ID)
		requireConflict(t, err)
		require.Equal(t, before, h.snapshot(t, device.ID, entity.ID), "删除被拒后状态无变化")

		// 有维修记录的闭环故障同样不可删除。
		requireConflict(t, h.faults.Delete(ctx, entity.ID))
	})
}

// TestBackdateRepairKeepsLatestAndTimeline 补录更早的维修记录后,
// 最近维修必须指向开工时间最晚的记录, 次数同步累加。
func TestBackdateRepairKeepsLatestAndTimeline(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-R-050")
	now := time.Now()

	entity, err := h.faults.Create(ctx, fault.CreateRequest{
		LampID:      device.ID,
		FaultType:   "线路故障",
		Description: "补录验证",
		ReportedAt:  fmtTime(now.Add(-72 * time.Hour)),
	})
	require.NoError(t, err)

	// 先登记一条"最近"的维修(10 小时前开工, 9 小时前完工)。
	late, err := h.repairs.Create(ctx, repair.CreateRequest{
		FaultID: entity.ID, Repairman: "维修工甲",
		StartedAt: fmtTime(now.Add(-10 * time.Hour)),
	})
	require.NoError(t, err)
	_, err = h.repairs.Finish(ctx, late.ID, repair.FinishRequest{
		Result:     repair.ResultFixed,
		FinishedAt: fmtTime(now.Add(-9 * time.Hour)),
	})
	require.NoError(t, err)

	// 补录一条更早的维修(24 小时前开工, 20 小时前完工)。
	early, err := h.repairs.Create(ctx, repair.CreateRequest{
		FaultID: entity.ID, Repairman: "维修工乙", Content: "补录的早期抢修",
		StartedAt: fmtTime(now.Add(-24 * time.Hour)),
	})
	require.NoError(t, err)
	// 补录时故障从已修复回退维修中, 再完工修复。
	require.Equal(t, fault.StatusProcessing, h.snapshot(t, device.ID, entity.ID).faultStatus)
	_, err = h.repairs.Finish(ctx, early.ID, repair.FinishRequest{
		Result:     repair.ResultPendingParts,
		FinishedAt: fmtTime(now.Add(-20 * time.Hour)),
	})
	require.NoError(t, err)

	after, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, 2, after.RepairCount, "补录后次数为 2")
	require.Equal(t, late.ID, *after.LatestRepairID, "最近维修仍指向开工最晚的记录, 而非新补录的旧记录")

	records, err := h.repairs.ListByFault(ctx, entity.ID)
	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Equal(t, early.ID, records[0].ID, "ListByFault 按开工时间正序, 补录的在前面")
	require.Equal(t, late.ID, records[1].ID)

	// 删除"最晚"的记录后, 最近维修应回退为补录的那条。
	require.NoError(t, h.repairs.Delete(ctx, late.ID))
	afterDelete, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, 1, afterDelete.RepairCount)
	require.Equal(t, early.ID, *afterDelete.LatestRepairID)
}

// TestFailedOperationsRestoreIdenticalState 对每一种失败操作,
// 断言路灯运行状态、维修次数与统计数字都回到操作前的同一状态。连续多轮运行。
func TestFailedOperationsRestoreIdenticalState(t *testing.T) {
	const rounds = 3
	for round := 0; round < rounds; round++ {
		t.Run(fmt.Sprintf("第%d轮/未闭环阶段", round+1), func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t)
			device := h.createLamp(t, fmt.Sprintf("LD-R-060-%d", round))
			entity := h.createFault(t, device.ID, "失败操作回滚验证")

			// 构造两次维修的中间态: 第一次观察中完工, 第二次进行中。
			first, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工甲"})
			require.NoError(t, err)
			cost := 120.5
			_, err = h.repairs.Finish(ctx, first.ID, repair.FinishRequest{
				Result: repair.ResultObserving, Cost: &cost, Content: "继续观察",
			})
			require.NoError(t, err)
			second, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工乙"})
			require.NoError(t, err)
			baseline := h.snapshot(t, device.ID, entity.ID)
			require.Equal(t, fault.StatusProcessing, baseline.faultStatus)

			operations := []struct {
				name   string
				run    func() error
				expect int // 期望 HTTP 状态码
			}{
				{"重复登记故障", func() error {
					_, err := h.faults.Create(ctx, fault.CreateRequest{
						LampID: device.ID, FaultType: "灯不亮", Description: "重复",
					})
					return err
				}, http.StatusConflict},
				{"对不存在路灯登记故障", func() error {
					_, err := h.faults.Create(ctx, fault.CreateRequest{
						LampID: device.ID + 9999, FaultType: "灯不亮", Description: "x",
					})
					return err
				}, http.StatusNotFound},
				{"非法故障类型", func() error {
					_, err := h.faults.Create(ctx, fault.CreateRequest{
						LampID: device.ID, FaultType: "不存在", Description: "x",
					})
					return err
				}, http.StatusBadRequest},
				{"空维修人开工", func() error {
					_, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID})
					return err
				}, http.StatusBadRequest},
				{"重复开工", func() error {
					_, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工丙"})
					return err
				}, http.StatusConflict},
				{"对不存在故障开工", func() error {
					_, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: 99999, Repairman: "维修工丙"})
					return err
				}, http.StatusNotFound},
				{"已完工记录重复完工", func() error {
					_, err := h.repairs.Finish(ctx, first.ID, repair.FinishRequest{Result: repair.ResultFixed})
					return err
				}, http.StatusConflict},
				{"进行中记录非法结果完工", func() error {
					_, err := h.repairs.Finish(ctx, second.ID, repair.FinishRequest{Result: "无效结果"})
					return err
				}, http.StatusBadRequest},
				{"完工时间早于开工", func() error {
					_, err := h.repairs.Finish(ctx, second.ID, repair.FinishRequest{
						Result: repair.ResultFixed, FinishedAt: fmtTime(second.StartedAt.Add(-time.Minute)),
					})
					return err
				}, http.StatusBadRequest},
				{"已完工记录修改", func() error {
					_, err := h.repairs.Update(ctx, first.ID, repair.UpdateRequest{Content: strPtr("篡改")})
					return err
				}, http.StatusConflict},
				{"删除不存在的维修记录", func() error {
					return h.repairs.Delete(ctx, 99999)
				}, http.StatusNotFound},
			}
			for _, op := range operations {
				t.Run(op.name, func(t *testing.T) {
					businessErr, ok := apperr.As(op.run())
					require.True(t, ok, "期望业务错误")
					require.Equal(t, op.expect, businessErr.Status, businessErr.Message)
					require.Equal(t, baseline, h.snapshot(t, device.ID, entity.ID),
						"失败操作 %s 后状态必须与操作前完全一致", op.name)
				})
			}
		})

		t.Run(fmt.Sprintf("第%d轮/终态阶段", round+1), func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t)
			device := h.createLamp(t, fmt.Sprintf("LD-R-061-%d", round))
			entity := h.createFault(t, device.ID, "终态回滚验证")
			first, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工甲"})
			require.NoError(t, err)
			_, err = h.repairs.Finish(ctx, first.ID, repair.FinishRequest{Result: repair.ResultFixed})
			require.NoError(t, err)
			closed, err := h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "闭环"})
			require.NoError(t, err)
			require.Equal(t, fault.StatusClosed, closed.Status)
			baseline := h.snapshot(t, device.ID, entity.ID)

			operations := []struct {
				name string
				run  func() error
			}{
				{"终态故障重复关闭", func() error {
					_, err := h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "再关"})
					return err
				}},
				{"终态故障修改", func() error {
					_, err := h.faults.Update(ctx, entity.ID, fault.UpdateRequest{Description: strPtr("篡改")})
					return err
				}},
				{"终态故障开工维修", func() error {
					_, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工丁"})
					return err
				}},
				{"终态故障删除维修记录", func() error {
					return h.repairs.Delete(ctx, first.ID)
				}},
				{"带维修记录的闭环故障不可删除", func() error {
					return h.faults.Delete(ctx, entity.ID)
				}},
			}
			for _, op := range operations {
				t.Run(op.name, func(t *testing.T) {
					requireConflict(t, op.run())
					require.Equal(t, baseline, h.snapshot(t, device.ID, entity.ID),
						"失败操作 %s 后状态必须与操作前完全一致", op.name)
				})
			}
		})
	}
}

// TestConcurrentDuplicateSubmissions 并发重复提交: 同一时刻多个请求做同一件事,
// 必须恰好一个成功, 其余全部 409, 且最终状态与串行预期完全相同。
// 每个场景内部连续跑多轮, 配合 go test -count=N 实现跨进程多轮验证。
func TestConcurrentDuplicateSubmissions(t *testing.T) {
	ctx := context.Background()
	const (
		workers = 8
		rounds  = 3
	)

	t.Run("并发重复开工", func(t *testing.T) {
		for round := 0; round < rounds; round++ {
			h := newHarness(t)
			device := h.createLamp(t, fmt.Sprintf("LD-R-070-%d", round))
			entity := h.createFault(t, device.ID, fmt.Sprintf("并发开工%d", round))

			start := make(chan struct{})
			var wg sync.WaitGroup
			results := make([]error, workers)
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					_, results[i] = h.repairs.Create(ctx, repair.CreateRequest{
						FaultID:   entity.ID,
						Repairman: fmt.Sprintf("维修工%d", i),
					})
				}(i)
			}
			close(start)
			wg.Wait()

			success, conflicts := classifyResults(t, results)
			require.Equal(t, 1, success, "第 %d 轮: 恰好一次开工成功", round+1)
			require.Equal(t, workers-1, conflicts, "第 %d 轮: 其余全部 409", round+1)

			current, err := h.faults.GetByID(ctx, entity.ID)
			require.NoError(t, err)
			require.Equal(t, fault.StatusProcessing, current.Status)
			require.Equal(t, 1, current.RepairCount)
			require.NotNil(t, current.LatestRepairID)

			records, err := h.repairs.ListByFault(ctx, entity.ID)
			require.NoError(t, err)
			require.Len(t, records, 1)
			require.Equal(t, repair.StatusOngoing, records[0].Status)

			deviceAfter, err := h.lamps.Get(ctx, device.ID)
			require.NoError(t, err)
			require.Equal(t, lamp.RunStatusMaintenance, deviceAfter.RunStatus)
		}
	})

	t.Run("并发重复登记故障", func(t *testing.T) {
		for round := 0; round < rounds; round++ {
			h := newHarness(t)
			device := h.createLamp(t, fmt.Sprintf("LD-R-071-%d", round))

			start := make(chan struct{})
			var wg sync.WaitGroup
			results := make([]error, workers)
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					_, results[i] = h.faults.Create(ctx, fault.CreateRequest{
						LampID:      device.ID,
						FaultType:   "灯不亮",
						Description: fmt.Sprintf("并发登记%d", i),
					})
				}(i)
			}
			close(start)
			wg.Wait()

			success, conflicts := classifyResults(t, results)
			require.Equal(t, 1, success)
			require.Equal(t, workers-1, conflicts)

			list, err := h.faults.ListByLamp(ctx, device.ID)
			require.NoError(t, err)
			require.Len(t, list, 1)
			require.Equal(t, fault.StatusPending, list[0].Status)

			deviceAfter, err := h.lamps.Get(ctx, device.ID)
			require.NoError(t, err)
			require.Equal(t, lamp.RunStatusFault, deviceAfter.RunStatus)
		}
	})

	t.Run("并发重复完工", func(t *testing.T) {
		for round := 0; round < rounds; round++ {
			h := newHarness(t)
			device := h.createLamp(t, fmt.Sprintf("LD-R-072-%d", round))
			entity := h.createFault(t, device.ID, fmt.Sprintf("并发完工%d", round))
			record, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工甲"})
			require.NoError(t, err)

			start := make(chan struct{})
			var wg sync.WaitGroup
			results := make([]error, workers)
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					_, results[i] = h.repairs.Finish(ctx, record.ID, repair.FinishRequest{
						Result: repair.ResultFixed,
					})
				}(i)
			}
			close(start)
			wg.Wait()

			success, conflicts := classifyResults(t, results)
			require.Equal(t, 1, success)
			require.Equal(t, workers-1, conflicts)

			finished, err := h.repairs.Get(ctx, record.ID)
			require.NoError(t, err)
			require.Equal(t, repair.StatusFinished, finished.Status)
			require.Equal(t, repair.ResultFixed, finished.Result)
			require.NotNil(t, finished.FinishedAt)

			current, err := h.faults.GetByID(ctx, entity.ID)
			require.NoError(t, err)
			require.Equal(t, fault.StatusRepaired, current.Status)
			require.Equal(t, 1, current.RepairCount)

			deviceAfter, err := h.lamps.Get(ctx, device.ID)
			require.NoError(t, err)
			require.Equal(t, lamp.RunStatusNormal, deviceAfter.RunStatus)
		}
	})

	t.Run("并发重复关闭", func(t *testing.T) {
		for round := 0; round < rounds; round++ {
			h := newHarness(t)
			device := h.createLamp(t, fmt.Sprintf("LD-R-073-%d", round))
			entity := h.createFault(t, device.ID, fmt.Sprintf("并发关闭%d", round))

			start := make(chan struct{})
			var wg sync.WaitGroup
			results := make([]error, workers)
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					_, results[i] = h.faults.Close(ctx, entity.ID, fault.CloseRequest{
						Remark: fmt.Sprintf("关闭请求%d", i),
					})
				}(i)
			}
			close(start)
			wg.Wait()

			success, conflicts := classifyResults(t, results)
			require.Equal(t, 1, success)
			require.Equal(t, workers-1, conflicts)

			current, err := h.faults.GetByID(ctx, entity.ID)
			require.NoError(t, err)
			require.Equal(t, fault.StatusClosed, current.Status)
			require.NotNil(t, current.ClosedAt)
			require.Equal(t, 1, countClosedAtWriters(t, results))
		}
	})
}

// classifyResults 统计成功与 409 冲突的数量, 出现其它错误立即失败。
func classifyResults(t *testing.T, results []error) (success, conflicts int) {
	t.Helper()
	for _, err := range results {
		switch {
		case err == nil:
			success++
		default:
			businessErr, ok := apperr.As(err)
			require.True(t, ok, "期望业务错误, 实际: %v", err)
			require.Equal(t, http.StatusConflict, businessErr.Status, "期望 409, 实际: %s", businessErr.Message)
			conflicts++
		}
	}
	return success, conflicts
}

// countClosedAtWriters 占位: 并发关闭只允许一次写入, 成功数即写入次数。
func countClosedAtWriters(t *testing.T, results []error) int {
	t.Helper()
	success, _ := classifyResults(t, results)
	return success
}
