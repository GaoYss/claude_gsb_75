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
	"streetlight/internal/modules/status"
)

// ---------------------------------------------------------------------------
// 公共辅助: 状态快照, 用于证明任何失败操作之后系统状态与操作前完全一致
// ---------------------------------------------------------------------------

// stateSnapshot 记录一次操作前系统的关键状态: 路灯运行状态、故障状态、
// 维修次数与全局统计数字。失败操作之后再次采样并与操作前逐项比对。
type stateSnapshot struct {
	lampRunStatus  string
	faultStatus    string
	repairCount    int
	latestRepairID *uint
	overview       *status.Overview
}

// snapshot 采样指定路灯与故障的当前状态以及全局概览统计。
func (h *harness) snapshot(t *testing.T, lampID, faultID uint) stateSnapshot {
	t.Helper()
	ctx := context.Background()

	device, err := h.lamps.Get(ctx, lampID)
	require.NoError(t, err)
	entity, err := h.faults.GetByID(ctx, faultID)
	require.NoError(t, err)
	overview, err := h.status.Overview(ctx)
	require.NoError(t, err)

	return stateSnapshot{
		lampRunStatus:  device.RunStatus,
		faultStatus:    entity.Status,
		repairCount:    entity.RepairCount,
		latestRepairID: entity.LatestRepairID,
		overview:       overview,
	}
}

// requireSameState 断言两次采样完全一致, 即失败操作没有留下任何副作用。
// 概览中的 GeneratedAt / 等待时长等随墙钟变化的字段不参与比较。
func requireSameState(t *testing.T, before, after stateSnapshot) {
	t.Helper()
	require.Equal(t, before.lampRunStatus, after.lampRunStatus, "路灯运行状态不应变化")
	require.Equal(t, before.faultStatus, after.faultStatus, "故障状态不应变化")
	require.Equal(t, before.repairCount, after.repairCount, "故障维修次数不应变化")
	if before.latestRepairID == nil {
		require.Nil(t, after.latestRepairID, "最近维修记录指针不应变化")
	} else {
		require.NotNil(t, after.latestRepairID, "最近维修记录指针不应变化")
		require.Equal(t, *before.latestRepairID, *after.latestRepairID, "最近维修记录指针不应变化")
	}

	left, right := before.overview, after.overview
	require.Equal(t, left.Lamp.Total, right.Lamp.Total, "路灯总数不应变化")
	require.Equal(t, left.Lamp.ByRunStatus, right.Lamp.ByRunStatus, "路灯运行状态统计不应变化")
	require.Equal(t, left.Fault.Total, right.Fault.Total, "故障总数不应变化")
	require.Equal(t, left.Fault.OpenTotal, right.Fault.OpenTotal, "未闭环故障数不应变化")
	require.Equal(t, left.Fault.ByStatus, right.Fault.ByStatus, "故障状态统计不应变化")
	require.Equal(t, left.Fault.OverdueTotal, right.Fault.OverdueTotal, "逾期故障数不应变化")
	require.Equal(t, left.Repair.Total, right.Repair.Total, "维修记录总数不应变化")
	require.Equal(t, left.Repair.OngoingTotal, right.Repair.OngoingTotal, "进行中维修数不应变化")
	require.Equal(t, left.Repair.FinishedTotal, right.Repair.FinishedTotal, "已完成维修数不应变化")
	require.Equal(t, left.Repair.TotalCost, right.Repair.TotalCost, "维修费用合计不应变化")
}

// requireBadRequest 断言错误是 400 参数错误。
func requireBadRequest(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	businessErr, ok := apperr.As(err)
	require.True(t, ok, "期望业务错误, 实际: %v", err)
	require.Equal(t, http.StatusBadRequest, businessErr.Status, "错误信息: %s", businessErr.Message)
}

// mustStartRepair 开工并断言成功。
func (h *harness) mustStartRepair(t *testing.T, faultID uint, repairman string) *repair.Repair {
	t.Helper()
	record, err := h.repairs.Create(context.Background(), repair.CreateRequest{
		FaultID: faultID, Repairman: repairman,
	})
	require.NoError(t, err)
	return record
}

// mustFinishRepair 完工并断言成功。
func (h *harness) mustFinishRepair(t *testing.T, repairID uint, result string) *repair.Repair {
	t.Helper()
	record, err := h.repairs.Finish(context.Background(), repairID, repair.FinishRequest{Result: result})
	require.NoError(t, err)
	return record
}

// mustGetFault 读取故障当前状态。
func (h *harness) mustGetFault(t *testing.T, id uint) *fault.Fault {
	t.Helper()
	entity, err := h.faults.GetByID(context.Background(), id)
	require.NoError(t, err)
	return entity
}

// mustGetLamp 读取路灯当前状态。
func (h *harness) mustGetLamp(t *testing.T, id uint) *lamp.Lamp {
	t.Helper()
	device, err := h.lamps.Get(context.Background(), id)
	require.NoError(t, err)
	return device
}

// ---------------------------------------------------------------------------
// 非法流转 / 重复提交 / 终态不可操作
// ---------------------------------------------------------------------------

// TestIllegalAndDuplicateTransitionsRejected 覆盖非法流转被拒与同一状态重复提交被拒,
// 每个用例都验证失败之后路灯运行状态、维修次数与统计数字与操作前完全一致。
func TestIllegalAndDuplicateTransitionsRejected(t *testing.T) {
	ctx := context.Background()

	// 每个用例独立装配数据, 返回操作涉及的路灯与故障以及被拒绝的操作本身。
	cases := []struct {
		name  string
		setup func(t *testing.T, h *harness) (lampID, faultID uint, op func() error)
	}{
		{
			name: "重复关闭同一故障被拒",
			setup: func(t *testing.T, h *harness) (uint, uint, func() error) {
				device := h.createLamp(t, "LD-R-001")
				entity := h.createFault(t, device.ID, "待关闭后重复关闭")
				_, err := h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "首次关闭"})
				require.NoError(t, err)
				return device.ID, entity.ID, func() error {
					_, err := h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "重复关闭"})
					return err
				}
			},
		},
		{
			name: "已关闭故障不允许登记维修",
			setup: func(t *testing.T, h *harness) (uint, uint, func() error) {
				device := h.createLamp(t, "LD-R-002")
				entity := h.createFault(t, device.ID, "关闭后登记维修")
				_, err := h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "误报作废"})
				require.NoError(t, err)
				return device.ID, entity.ID, func() error {
					_, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工"})
					return err
				}
			},
		},
		{
			name: "已修复故障不允许直接登记维修",
			setup: func(t *testing.T, h *harness) (uint, uint, func() error) {
				device := h.createLamp(t, "LD-R-003")
				entity := h.createFault(t, device.ID, "修复后重复登记维修")
				record := h.mustStartRepair(t, entity.ID, "维修工甲")
				h.mustFinishRepair(t, record.ID, repair.ResultFixed)
				return device.ID, entity.ID, func() error {
					_, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工乙"})
					return err
				}
			},
		},
		{
			name: "已关闭故障不允许修改",
			setup: func(t *testing.T, h *harness) (uint, uint, func() error) {
				device := h.createLamp(t, "LD-R-004")
				entity := h.createFault(t, device.ID, "关闭后修改")
				_, err := h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "闭环"})
				require.NoError(t, err)
				description := "试图修改已关闭故障"
				return device.ID, entity.ID, func() error {
					_, err := h.faults.Update(ctx, entity.ID, fault.UpdateRequest{Description: &description})
					return err
				}
			},
		},
		{
			name: "未闭环故障不允许删除",
			setup: func(t *testing.T, h *harness) (uint, uint, func() error) {
				device := h.createLamp(t, "LD-R-005")
				entity := h.createFault(t, device.ID, "待处理故障删除")
				return device.ID, entity.ID, func() error {
					return h.faults.Delete(ctx, entity.ID)
				}
			},
		},
		{
			name: "维修中故障不允许删除",
			setup: func(t *testing.T, h *harness) (uint, uint, func() error) {
				device := h.createLamp(t, "LD-R-006")
				entity := h.createFault(t, device.ID, "维修中故障删除")
				h.mustStartRepair(t, entity.ID, "维修工甲")
				return device.ID, entity.ID, func() error {
					return h.faults.Delete(ctx, entity.ID)
				}
			},
		},
		{
			name: "有维修记录的已关闭故障不允许删除",
			setup: func(t *testing.T, h *harness) (uint, uint, func() error) {
				device := h.createLamp(t, "LD-R-007")
				entity := h.createFault(t, device.ID, "闭环故障删除")
				record := h.mustStartRepair(t, entity.ID, "维修工甲")
				h.mustFinishRepair(t, record.ID, repair.ResultFixed)
				_, err := h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "闭环"})
				require.NoError(t, err)
				return device.ID, entity.ID, func() error {
					return h.faults.Delete(ctx, entity.ID)
				}
			},
		},
		{
			name: "已完成维修记录不允许修改",
			setup: func(t *testing.T, h *harness) (uint, uint, func() error) {
				device := h.createLamp(t, "LD-R-008")
				entity := h.createFault(t, device.ID, "完工后修改记录")
				record := h.mustStartRepair(t, entity.ID, "维修工甲")
				h.mustFinishRepair(t, record.ID, repair.ResultPendingParts)
				repairman := "维修工乙"
				return device.ID, entity.ID, func() error {
					_, err := h.repairs.Update(ctx, record.ID, repair.UpdateRequest{Repairman: &repairman})
					return err
				}
			},
		},
		{
			name: "已完成维修记录不允许重复完工",
			setup: func(t *testing.T, h *harness) (uint, uint, func() error) {
				device := h.createLamp(t, "LD-R-009")
				entity := h.createFault(t, device.ID, "重复完工")
				record := h.mustStartRepair(t, entity.ID, "维修工甲")
				h.mustFinishRepair(t, record.ID, repair.ResultFixed)
				return device.ID, entity.ID, func() error {
					_, err := h.repairs.Finish(ctx, record.ID, repair.FinishRequest{Result: repair.ResultFixed})
					return err
				}
			},
		},
		{
			name: "已关闭故障的维修记录不允许删除",
			setup: func(t *testing.T, h *harness) (uint, uint, func() error) {
				device := h.createLamp(t, "LD-R-010")
				entity := h.createFault(t, device.ID, "闭环后删除维修记录")
				record := h.mustStartRepair(t, entity.ID, "维修工甲")
				h.mustFinishRepair(t, record.ID, repair.ResultFixed)
				_, err := h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "闭环"})
				require.NoError(t, err)
				return device.ID, entity.ID, func() error {
					return h.repairs.Delete(ctx, record.ID)
				}
			},
		},
		{
			name: "同一路灯重复登记未闭环故障被拒(待处理)",
			setup: func(t *testing.T, h *harness) (uint, uint, func() error) {
				device := h.createLamp(t, "LD-R-011")
				entity := h.createFault(t, device.ID, "首条故障")
				return device.ID, entity.ID, func() error {
					_, err := h.faults.Create(ctx, fault.CreateRequest{
						LampID: device.ID, FaultType: "灯光闪烁", Description: "重复登记",
					})
					return err
				}
			},
		},
		{
			name: "同一路灯重复登记未闭环故障被拒(维修中)",
			setup: func(t *testing.T, h *harness) (uint, uint, func() error) {
				device := h.createLamp(t, "LD-R-012")
				entity := h.createFault(t, device.ID, "首条故障已开工")
				h.mustStartRepair(t, entity.ID, "维修工甲")
				return device.ID, entity.ID, func() error {
					_, err := h.faults.Create(ctx, fault.CreateRequest{
						LampID: device.ID, FaultType: "灯光闪烁", Description: "重复登记",
					})
					return err
				}
			},
		},
		{
			name: "同一故障重复开工被拒",
			setup: func(t *testing.T, h *harness) (uint, uint, func() error) {
				device := h.createLamp(t, "LD-R-013")
				entity := h.createFault(t, device.ID, "重复开工")
				h.mustStartRepair(t, entity.ID, "维修工甲")
				return device.ID, entity.ID, func() error {
					_, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工乙"})
					return err
				}
			},
		},
	}

	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			h := newHarness(t)
			lampID, faultID, op := item.setup(t, h)

			before := h.snapshot(t, lampID, faultID)
			requireConflict(t, op())
			after := h.snapshot(t, lampID, faultID)
			requireSameState(t, before, after)
		})
	}
}

// TestOpenFaultBlocksReregistrationUntilClosed 验证"未闭环"口径:
// 待处理与维修中阻止再次登记, 已修复与已关闭后允许登记新故障。
func TestOpenFaultBlocksReregistrationUntilClosed(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-R-020")

	first := h.createFault(t, device.ID, "第一条故障")

	// 待处理状态阻止再次登记
	_, err := h.faults.Create(ctx, fault.CreateRequest{
		LampID: device.ID, FaultType: "灯不亮", Description: "重复登记",
	})
	requireConflict(t, err)

	// 维修中状态同样阻止再次登记
	record := h.mustStartRepair(t, first.ID, "维修工甲")
	_, err = h.faults.Create(ctx, fault.CreateRequest{
		LampID: device.ID, FaultType: "灯不亮", Description: "重复登记",
	})
	requireConflict(t, err)

	// 已修复不再属于未闭环, 允许登记新故障
	h.mustFinishRepair(t, record.ID, repair.ResultFixed)
	second := h.createFault(t, device.ID, "返修新故障")
	require.NotEqual(t, first.ID, second.ID)

	// 新故障关闭后同样允许登记
	_, err = h.faults.Close(ctx, second.ID, fault.CloseRequest{Remark: "误报"})
	require.NoError(t, err)
	third := h.createFault(t, device.ID, "关闭后再登记")
	require.NotEqual(t, second.ID, third.ID)

	// 统计口径: 三条故障, 一条未闭环(third), 路灯处于故障状态
	overview, err := h.status.Overview(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3), overview.Fault.Total)
	require.Equal(t, int64(1), overview.Fault.OpenTotal)
	require.Equal(t, lamp.RunStatusFault, h.mustGetLamp(t, device.ID).RunStatus)
}

// ---------------------------------------------------------------------------
// 不同维修结果对故障状态的推进差异
// ---------------------------------------------------------------------------

// TestRepairResultDrivesFaultStatus 验证四种维修结果对故障状态与路灯状态的不同推进效果。
func TestRepairResultDrivesFaultStatus(t *testing.T) {
	cases := []struct {
		result           string
		expectFault      string
		expectLampStatus string
	}{
		{repair.ResultFixed, fault.StatusRepaired, lamp.RunStatusNormal},
		{repair.ResultPendingParts, fault.StatusProcessing, lamp.RunStatusMaintenance},
		{repair.ResultObserving, fault.StatusProcessing, lamp.RunStatusMaintenance},
		{repair.ResultUnfixable, fault.StatusProcessing, lamp.RunStatusMaintenance},
	}

	for index, item := range cases {
		t.Run(item.result, func(t *testing.T) {
			h := newHarness(t)
			device := h.createLamp(t, fmt.Sprintf("LD-R-1%02d", index))
			entity := h.createFault(t, device.ID, "结果推进差异")

			record := h.mustStartRepair(t, entity.ID, "维修工甲")
			finished := h.mustFinishRepair(t, record.ID, item.result)
			require.Equal(t, repair.StatusFinished, finished.Status)
			require.Equal(t, item.result, finished.Result)

			updated := h.mustGetFault(t, entity.ID)
			require.Equal(t, item.expectFault, updated.Status, "维修结果 %s 对故障状态的推进不符合预期", item.result)
			require.Equal(t, 1, updated.RepairCount)
			require.Equal(t, item.expectLampStatus, h.mustGetLamp(t, device.ID).RunStatus,
				"维修结果 %s 对路灯状态的推进不符合预期", item.result)
		})
	}
}

// ---------------------------------------------------------------------------
// 删除维修记录后的回落
// ---------------------------------------------------------------------------

// TestDeleteRepairRollsBackStats 验证删除维修记录后维修次数、最近维修指针、
// 故障状态与路灯状态按设计回落。
func TestDeleteRepairRollsBackStats(t *testing.T) {
	ctx := context.Background()

	t.Run("删除唯一进行中维修后故障回到待处理", func(t *testing.T) {
		h := newHarness(t)
		device := h.createLamp(t, "LD-R-030")
		entity := h.createFault(t, device.ID, "删除开工记录")

		record := h.mustStartRepair(t, entity.ID, "维修工甲")
		require.Equal(t, fault.StatusProcessing, h.mustGetFault(t, entity.ID).Status)
		require.Equal(t, lamp.RunStatusMaintenance, h.mustGetLamp(t, device.ID).RunStatus)

		require.NoError(t, h.repairs.Delete(ctx, record.ID))

		updated := h.mustGetFault(t, entity.ID)
		require.Equal(t, 0, updated.RepairCount, "删除后维修次数应回落为 0")
		require.Nil(t, updated.LatestRepairID, "删除后最近维修指针应清空")
		require.Equal(t, fault.StatusPending, updated.Status, "删除后故障应回落为待处理")
		require.Equal(t, lamp.RunStatusFault, h.mustGetLamp(t, device.ID).RunStatus,
			"删除后路灯应回落为故障状态")

		overview, err := h.status.Overview(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(0), overview.Repair.Total, "删除后维修记录总数应回落")
		require.Equal(t, int64(1), overview.Fault.OpenTotal)
	})

	t.Run("删除多条维修中的最新一条后指针回落", func(t *testing.T) {
		h := newHarness(t)
		device := h.createLamp(t, "LD-R-031")
		entity := h.createFault(t, device.ID, "两次维修后删除")

		first := h.mustStartRepair(t, entity.ID, "维修工甲")
		h.mustFinishRepair(t, first.ID, repair.ResultPendingParts)
		second := h.mustStartRepair(t, entity.ID, "维修工乙")
		h.mustFinishRepair(t, second.ID, repair.ResultFixed)

		updated := h.mustGetFault(t, entity.ID)
		require.Equal(t, 2, updated.RepairCount)
		require.Equal(t, fault.StatusRepaired, updated.Status)
		require.Equal(t, second.ID, *updated.LatestRepairID)

		// 删除最近一次维修: 次数回落为 1, 最近维修指针回落到第一条
		require.NoError(t, h.repairs.Delete(ctx, second.ID))
		updated = h.mustGetFault(t, entity.ID)
		require.Equal(t, 1, updated.RepairCount)
		require.NotNil(t, updated.LatestRepairID)
		require.Equal(t, first.ID, *updated.LatestRepairID)
		require.Equal(t, lamp.RunStatusNormal, h.mustGetLamp(t, device.ID).RunStatus)

		// 再删除第一条: 次数回落为 0, 指针清空
		require.NoError(t, h.repairs.Delete(ctx, first.ID))
		updated = h.mustGetFault(t, entity.ID)
		require.Equal(t, 0, updated.RepairCount)
		require.Nil(t, updated.LatestRepairID)

		overview, err := h.status.Overview(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(0), overview.Repair.Total)
		require.Equal(t, int64(0), overview.Repair.FinishedTotal)
	})
}

// ---------------------------------------------------------------------------
// 并发与重复提交: 连续多轮运行必须得到相同结论
// ---------------------------------------------------------------------------

// TestConcurrentDuplicateSubmissionDeterministic 对同一组并发场景连续运行多轮,
// 每一轮都必须得到完全相同的结论: 同类操作并发提交时仅一个成功, 其余全部被 409 拒绝,
// 且事后状态与单次顺序执行一致。
func TestConcurrentDuplicateSubmissionDeterministic(t *testing.T) {
	const (
		rounds  = 5
		workers = 8
	)
	ctx := context.Background()

	// runConcurrent 并发执行 workers 个相同操作, 返回成功数与 409 冲突数。
	runConcurrent := func(t *testing.T, op func(worker int) error) (succeeded, conflicted int) {
		t.Helper()
		var wg sync.WaitGroup
		results := make(chan error, workers)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				results <- op(worker)
			}(i)
		}
		wg.Wait()
		close(results)
		for err := range results {
			if err == nil {
				succeeded++
				continue
			}
			businessErr, ok := apperr.As(err)
			require.True(t, ok, "并发失败应返回业务错误, 实际: %v", err)
			require.Equal(t, http.StatusConflict, businessErr.Status, "并发失败应为 409, 实际: %v", err)
			conflicted++
		}
		return succeeded, conflicted
	}

	for round := 0; round < rounds; round++ {
		t.Run(fmt.Sprintf("第%d轮", round+1), func(t *testing.T) {
			h := newHarness(t)

			// 场景一: 同一路灯并发重复登记故障, 仅一条成功
			device := h.createLamp(t, fmt.Sprintf("LD-C-%03d", round))
			succeeded, conflicted := runConcurrent(t, func(worker int) error {
				_, err := h.faults.Create(ctx, fault.CreateRequest{
					LampID: device.ID, FaultType: "灯不亮", Description: fmt.Sprintf("并发登记%d", worker),
				})
				return err
			})
			require.Equal(t, 1, succeeded, "并发登记故障仅允许一条成功")
			require.Equal(t, workers-1, conflicted, "其余并发登记必须全部 409")

			overview, err := h.status.Overview(ctx)
			require.NoError(t, err)
			require.Equal(t, int64(1), overview.Fault.Total)
			require.Equal(t, int64(1), overview.Fault.OpenTotal)

			faultsOfLamp, err := h.faults.ListByLamp(ctx, device.ID)
			require.NoError(t, err)
			require.Len(t, faultsOfLamp, 1, "并发登记后同一路灯应只有一条故障")
			entity := &faultsOfLamp[0]
			require.Equal(t, fault.StatusPending, entity.Status)

			// 场景二: 同一故障并发重复开工, 仅一条成功
			succeeded, conflicted = runConcurrent(t, func(worker int) error {
				_, err := h.repairs.Create(ctx, repair.CreateRequest{
					FaultID: entity.ID, Repairman: fmt.Sprintf("维修工%d", worker),
				})
				return err
			})
			require.Equal(t, 1, succeeded, "并发开工仅允许一条成功")
			require.Equal(t, workers-1, conflicted, "其余并发开工必须全部 409")

			entity = h.mustGetFault(t, entity.ID)
			require.Equal(t, fault.StatusProcessing, entity.Status)
			require.Equal(t, 1, entity.RepairCount, "并发开工后维修次数必须恰好为 1")
			require.Equal(t, lamp.RunStatusMaintenance, h.mustGetLamp(t, device.ID).RunStatus)

			overview, err = h.status.Overview(ctx)
			require.NoError(t, err)
			require.Equal(t, int64(1), overview.Repair.Total)
			require.Equal(t, int64(1), overview.Repair.OngoingTotal)

			// 场景三: 同一维修记录并发重复完工, 仅一次成功
			repairID := *entity.LatestRepairID
			succeeded, conflicted = runConcurrent(t, func(worker int) error {
				_, err := h.repairs.Finish(ctx, repairID, repair.FinishRequest{Result: repair.ResultFixed})
				return err
			})
			require.Equal(t, 1, succeeded, "并发完工仅允许一次成功")
			require.Equal(t, workers-1, conflicted, "其余并发完工必须全部 409")

			entity = h.mustGetFault(t, entity.ID)
			require.Equal(t, fault.StatusRepaired, entity.Status)
			require.Equal(t, 1, entity.RepairCount, "重复完工不得改变维修次数")
			require.Equal(t, lamp.RunStatusNormal, h.mustGetLamp(t, device.ID).RunStatus)

			overview, err = h.status.Overview(ctx)
			require.NoError(t, err)
			require.Equal(t, int64(1), overview.Repair.Total)
			require.Equal(t, int64(0), overview.Repair.OngoingTotal)
			require.Equal(t, int64(1), overview.Repair.FinishedTotal)

			// 场景四: 同一故障并发重复关闭, 仅一次成功
			succeeded, conflicted = runConcurrent(t, func(worker int) error {
				_, err := h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "并发关闭"})
				return err
			})
			require.Equal(t, 1, succeeded, "并发关闭仅允许一次成功")
			require.Equal(t, workers-1, conflicted, "其余并发关闭必须全部 409")

			entity = h.mustGetFault(t, entity.ID)
			require.Equal(t, fault.StatusClosed, entity.Status)
			require.NotNil(t, entity.ClosedAt)

			overview, err = h.status.Overview(ctx)
			require.NoError(t, err)
			require.Equal(t, int64(0), overview.Fault.OpenTotal)
			require.Equal(t, int64(1), overview.Fault.ByStatus[fault.StatusClosed])
		})
	}
}

// ---------------------------------------------------------------------------
// 任何失败操作之后, 系统状态与操作前完全一致
// ---------------------------------------------------------------------------

// TestFailedOperationLeavesStateUntouched 在同一批数据上连续执行各类被拒绝的操作,
// 每一次失败之后, 路灯运行状态、维修次数与统计数字都必须与操作前完全一致。
func TestFailedOperationLeavesStateUntouched(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	// 数据准备:
	//   L1/F1 待处理; L2/F2 维修中(有进行中维修 R2);
	//   L3/F3 已修复(有已完成维修 R3); L4/F4 已关闭(有已完成维修 R4); L5 无故障。
	device1 := h.createLamp(t, "LD-F-001")
	fault1 := h.createFault(t, device1.ID, "待处理故障")

	device2 := h.createLamp(t, "LD-F-002")
	fault2 := h.createFault(t, device2.ID, "维修中故障")
	repair2 := h.mustStartRepair(t, fault2.ID, "维修工甲")

	device3 := h.createLamp(t, "LD-F-003")
	fault3 := h.createFault(t, device3.ID, "已修复故障")
	repair3 := h.mustStartRepair(t, fault3.ID, "维修工乙")
	h.mustFinishRepair(t, repair3.ID, repair.ResultFixed)

	device4 := h.createLamp(t, "LD-F-004")
	fault4 := h.createFault(t, device4.ID, "已关闭故障")
	repair4 := h.mustStartRepair(t, fault4.ID, "维修工丙")
	h.mustFinishRepair(t, repair4.ID, repair.ResultFixed)
	_, err := h.faults.Close(ctx, fault4.ID, fault.CloseRequest{Remark: "闭环"})
	require.NoError(t, err)

	device5 := h.createLamp(t, "LD-F-005")

	description := "试图修改"
	repairman := "维修工丁"
	earlyStart := time.Now().Add(-2 * time.Hour).Format("2006-01-02 15:04:05")

	// 每个用例: 涉及的路灯与故障 + 一个必然失败的操作。
	cases := []struct {
		name    string
		lampID  uint
		faultID uint
		op      func() error
		assert  func(t *testing.T, err error)
	}{
		{"重复登记未闭环故障(待处理)", device1.ID, fault1.ID, func() error {
			_, err := h.faults.Create(ctx, fault.CreateRequest{LampID: device1.ID, FaultType: "灯不亮", Description: "重复"})
			return err
		}, requireConflict},
		{"重复登记未闭环故障(维修中)", device2.ID, fault2.ID, func() error {
			_, err := h.faults.Create(ctx, fault.CreateRequest{LampID: device2.ID, FaultType: "灯不亮", Description: "重复"})
			return err
		}, requireConflict},
		{"非法故障类型", device5.ID, fault1.ID, func() error {
			_, err := h.faults.Create(ctx, fault.CreateRequest{LampID: device5.ID, FaultType: "不存在", Description: "非法"})
			return err
		}, requireBadRequest},
		{"故障描述为空", device5.ID, fault1.ID, func() error {
			_, err := h.faults.Create(ctx, fault.CreateRequest{LampID: device5.ID, FaultType: "灯不亮", Description: "  "})
			return err
		}, requireBadRequest},
		{"维修人员为空", device1.ID, fault1.ID, func() error {
			_, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: fault1.ID, Repairman: "  "})
			return err
		}, requireBadRequest},
		{"开工时间早于上报时间", device1.ID, fault1.ID, func() error {
			_, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: fault1.ID, Repairman: "维修工", StartedAt: earlyStart})
			return err
		}, requireBadRequest},
		{"同一故障重复开工", device2.ID, fault2.ID, func() error {
			_, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: fault2.ID, Repairman: "维修工"})
			return err
		}, requireConflict},
		{"已修复故障登记维修", device3.ID, fault3.ID, func() error {
			_, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: fault3.ID, Repairman: "维修工"})
			return err
		}, requireConflict},
		{"已关闭故障登记维修", device4.ID, fault4.ID, func() error {
			_, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: fault4.ID, Repairman: "维修工"})
			return err
		}, requireConflict},
		{"非法维修结果", device2.ID, fault2.ID, func() error {
			_, err := h.repairs.Finish(ctx, repair2.ID, repair.FinishRequest{Result: "不存在的结果"})
			return err
		}, requireBadRequest},
		{"完工时间早于开工时间", device2.ID, fault2.ID, func() error {
			_, err := h.repairs.Finish(ctx, repair2.ID, repair.FinishRequest{Result: repair.ResultFixed, FinishedAt: earlyStart})
			return err
		}, requireBadRequest},
		{"重复完工", device3.ID, fault3.ID, func() error {
			_, err := h.repairs.Finish(ctx, repair3.ID, repair.FinishRequest{Result: repair.ResultFixed})
			return err
		}, requireConflict},
		{"修改已完成维修记录", device3.ID, fault3.ID, func() error {
			_, err := h.repairs.Update(ctx, repair3.ID, repair.UpdateRequest{Repairman: &repairman})
			return err
		}, requireConflict},
		{"删除已关闭故障的维修记录", device4.ID, fault4.ID, func() error {
			return h.repairs.Delete(ctx, repair4.ID)
		}, requireConflict},
		{"重复关闭故障", device4.ID, fault4.ID, func() error {
			_, err := h.faults.Close(ctx, fault4.ID, fault.CloseRequest{Remark: "重复关闭"})
			return err
		}, requireConflict},
		{"修改已关闭故障", device4.ID, fault4.ID, func() error {
			_, err := h.faults.Update(ctx, fault4.ID, fault.UpdateRequest{Description: &description})
			return err
		}, requireConflict},
		{"删除未闭环故障", device1.ID, fault1.ID, func() error {
			return h.faults.Delete(ctx, fault1.ID)
		}, requireConflict},
		{"删除有维修记录的已关闭故障", device4.ID, fault4.ID, func() error {
			return h.faults.Delete(ctx, fault4.ID)
		}, requireConflict},
		{"删除有未闭环故障的路灯", device2.ID, fault2.ID, func() error {
			return h.lamps.Delete(ctx, device2.ID)
		}, requireConflict},
	}

	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			before := h.snapshot(t, item.lampID, item.faultID)
			item.assert(t, item.op())
			after := h.snapshot(t, item.lampID, item.faultID)
			requireSameState(t, before, after)
		})
	}

	// 终态联动原子性: 故障关闭后, 对其进行中维修记录的完工操作必须整体失败并回滚,
	// 维修记录保持进行中, 统计数字不变。
	t.Run("关闭故障后完工其进行中维修被拒且回滚", func(t *testing.T) {
		_, err := h.faults.Close(ctx, fault2.ID, fault.CloseRequest{Remark: "维修中作废"})
		require.NoError(t, err)

		before := h.snapshot(t, device2.ID, fault2.ID)
		_, err = h.repairs.Finish(ctx, repair2.ID, repair.FinishRequest{Result: repair.ResultFixed})
		requireConflict(t, err)
		after := h.snapshot(t, device2.ID, fault2.ID)
		requireSameState(t, before, after)

		record, err := h.repairs.Get(ctx, repair2.ID)
		require.NoError(t, err)
		require.Equal(t, repair.StatusOngoing, record.Status, "失败的完工必须回滚, 记录保持进行中")
		require.Nil(t, record.FinishedAt)
		require.Empty(t, record.Result)
	})
}
