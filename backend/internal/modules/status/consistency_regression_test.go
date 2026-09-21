package status_test

import (
	"context"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"streetlight/internal/modules/fault"
	"streetlight/internal/modules/lamp"
	"streetlight/internal/modules/repair"
	"streetlight/internal/modules/status"
)

// overdueDataset 在同一批路灯/故障/维修数据上构造完整的逾期与闭环场景:
//
//	L1 待处理 30h 前上报          -> 逾期, 未闭环, 路灯故障
//	L2 维修中 30h 前上报(已开工)   -> 逾期, 未闭环, 路灯维修中
//	L3 待处理 6h 前上报           -> 未逾期, 未闭环
//	L4 维修中 2h 前上报(已开工)    -> 未逾期, 未闭环
//	L5 已修复 50h 前上报          -> 未逾期, 已闭环之外的终结中间态
//	L6 已关闭 100h 前上报         -> 已闭环
//	L7 无故障                     -> 正常台账
func overdueDataset(t *testing.T, h *harness) map[string]*lamp.Lamp {
	t.Helper()
	ctx := context.Background()
	now := time.Now()

	devices := map[string]*lamp.Lamp{}
	for _, item := range []struct{ code, road string }{
		{"LD-C-001", "中山路"}, {"LD-C-002", "中山路"},
		{"LD-C-003", "解放路"}, {"LD-C-004", "解放路"},
		{"LD-C-005", "滨江路"}, {"LD-C-006", "滨江路"},
		{"LD-C-007", "学院路"},
	} {
		devices[item.code] = h.createLamp(t, item.code, item.road)
	}

	h.createFaultAt(t, devices["LD-C-001"].ID, now.Add(-30*time.Hour), "逾期未开工")
	h.createFaultAt(t, devices["LD-C-003"].ID, now.Add(-6*time.Hour), "刚上报")

	l2 := h.createFaultAt(t, devices["LD-C-002"].ID, now.Add(-30*time.Hour), "逾期仍在修")
	r2 := h.startRepairAt(t, l2.ID, "维修工甲", now.Add(-20*time.Hour))

	l4 := h.createFaultAt(t, devices["LD-C-004"].ID, now.Add(-2*time.Hour), "刚开工")
	_ = h.startRepairAt(t, l4.ID, "维修工乙", now.Add(-time.Hour))

	l5 := h.createFaultAt(t, devices["LD-C-005"].ID, now.Add(-50*time.Hour), "已修复")
	r5 := h.startRepairAt(t, l5.ID, "维修工丙", now.Add(-48*time.Hour))
	h.finishRepairAt(t, r5, repair.ResultFixed, now.Add(-44*time.Hour))

	l6 := h.createFaultAt(t, devices["LD-C-006"].ID, now.Add(-100*time.Hour), "已关闭")
	r6 := h.startRepairAt(t, l6.ID, "维修工丁", now.Add(-98*time.Hour))
	h.finishRepairAt(t, r6, repair.ResultFixed, now.Add(-90*time.Hour))
	_, err := h.faults.Close(ctx, l6.ID, fault.CloseRequest{Remark: "闭环"})
	require.NoError(t, err)

	// L2/L4 的维修记录保持进行中(不完工), 正好覆盖"逾期但已开工"。
	_ = r2
	return devices
}

// rowByLamp 从状态行集合中按路灯编号取行。
func rowByLamp(t *testing.T, rows []status.LampStatusRow, code string) status.LampStatusRow {
	t.Helper()
	for _, row := range rows {
		if row.LampCode == code {
			return row
		}
	}
	t.Fatalf("状态行中找不到路灯 %s", code)
	return status.LampStatusRow{}
}

// TestOverdueAndClosureConsistency 验证概览 / 状态清单 / 导出三处
// 在同一批数据下对逾期与闭环的判定完全一致。
func TestOverdueAndClosureConsistency(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	devices := overdueDataset(t, h)

	overview, err := h.status.Overview(ctx)
	require.NoError(t, err)

	// 概览: 逾期 = 2(L1 待处理 + L2 维修中), 未闭环 = 4, 已关闭 = 1。
	require.Equal(t, int64(2), overview.Fault.OverdueTotal,
		"逾期口径为上报超 24h 仍未闭环, 应包含维修中的 L2")
	require.Equal(t, int64(4), overview.Fault.OpenTotal)
	require.Equal(t, int64(1), overview.Fault.ClosedTotal)
	require.Equal(t, int64(7), overview.Lamp.Total)
	require.Equal(t, int64(2), overview.Lamp.ByRunStatus[lamp.RunStatusFault])
	require.Equal(t, int64(2), overview.Lamp.ByRunStatus[lamp.RunStatusMaintenance])
	require.Equal(t, int64(3), overview.Lamp.ByRunStatus[lamp.RunStatusNormal])
	require.Equal(t, status.OverdueThreshold.Hours(), overview.OverdueHours)

	// 概览逾期清单: 恰好 L1/L2, 且都打了逾期标记。
	require.Len(t, overview.OverdueFaults, 2)
	overdueCodes := map[string]bool{}
	for _, item := range overview.OverdueFaults {
		require.True(t, item.Overdue, "逾期清单中的 %s 必须带逾期标记", item.FaultNo)
		overdueCodes[item.LampCode] = true
	}
	require.ElementsMatch(t,
		[]string{devices["LD-C-001"].Code, devices["LD-C-002"].Code},
		keys(overdueCodes))

	// 清单全量(page_size=200)。
	rows, total, _, err := h.status.Lamps(ctx, status.LampQuery{
		Page: 1, PageSize: 200,
	})
	require.NoError(t, err)
	require.Equal(t, int64(7), total)
	require.Len(t, rows, 7)

	// 每盏灯在清单上的逾期/闭环标记。
	require.True(t, rowByLamp(t, rows, "LD-C-001").FaultOverdue, "L1 待处理逾期")
	require.True(t, rowByLamp(t, rows, "LD-C-002").FaultOverdue, "L2 维修中逾期")
	require.False(t, rowByLamp(t, rows, "LD-C-003").FaultOverdue, "L3 刚上报不逾期")
	require.False(t, rowByLamp(t, rows, "LD-C-004").FaultOverdue, "L4 维修中但未超 24h")
	require.False(t, rowByLamp(t, rows, "LD-C-005").FaultOverdue, "L5 已修复不算逾期")
	require.False(t, rowByLamp(t, rows, "LD-C-006").FaultOverdue, "L6 已闭环不算逾期")

	// 闭环口径: 未闭环计数与概览一致。
	var openFromRows, overdueFromRows int64
	for _, row := range rows {
		openFromRows += row.OpenFaults
		if row.FaultOverdue {
			overdueFromRows++
		}
	}
	require.Equal(t, overview.Fault.OpenTotal, openFromRows, "清单未闭环计数合计 = 概览 open_total")
	require.Equal(t, overview.Fault.OverdueTotal, overdueFromRows, "清单逾期行数 = 概览 overdue_total")

	// L6 已闭环: 未闭环数为 0, 不出现在 only_open 列表。
	l6 := rowByLamp(t, rows, "LD-C-006")
	require.Equal(t, int64(0), l6.OpenFaults)
	require.Equal(t, int64(1), l6.TotalFaults)
	require.Equal(t, "", l6.FaultNo, "闭环后没有当前故障")
	openRows, openTotal, _, err := h.status.Lamps(ctx, status.LampQuery{OnlyOpen: true, PageSize: 200})
	require.NoError(t, err)
	require.Equal(t, int64(4), openTotal)
	for _, row := range openRows {
		require.NotEqual(t, "LD-C-006", row.LampCode)
	}

	// 导出: 与清单同源, 逾期行数、未闭环行数、闭环灯标记全部一致。
	exportRows, err := h.status.ExportRows(ctx, status.LampQuery{})
	require.NoError(t, err)
	require.Len(t, exportRows, 7)
	var overdueFromExport, openFromExport int
	for _, row := range exportRows {
		if row.FaultOverdue {
			overdueFromExport++
		}
		if row.OpenFaults > 0 {
			openFromExport++
		}
		if row.LampCode == "LD-C-006" {
			require.False(t, row.FaultOverdue)
			require.Equal(t, int64(0), row.OpenFaults)
			require.Empty(t, row.FaultNo)
		}
	}
	require.Equal(t, int(overview.Fault.OverdueTotal), overdueFromExport, "导出逾期行数 = 概览逾期数")
	require.Equal(t, int(overview.Fault.OpenTotal), openFromExport, "导出未闭环行数 = 概览未闭环数")

	// only_open 过滤条件下导出与清单 total 一致。
	openExportRows, err := h.status.ExportRows(ctx, status.LampQuery{OnlyOpen: true})
	require.NoError(t, err)
	require.Len(t, openExportRows, int(openTotal))
}

func keys(m map[string]bool) []string {
	result := make([]string, 0, len(m))
	for k := range m {
		result = append(result, k)
	}
	return result
}

// TestOverdueConsistencyAfterClosure 在同一批数据上做闭环操作,
// 三处视图的逾期/闭环数字必须同步回落。
func TestOverdueConsistencyAfterClosure(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	devices := overdueDataset(t, h)

	l1Fault, err := h.faults.ListByLamp(ctx, devices["LD-C-001"].ID)
	require.NoError(t, err)
	require.Len(t, l1Fault, 1)

	before, err := h.status.Overview(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), before.Fault.OverdueTotal)
	require.Equal(t, int64(4), before.Fault.OpenTotal)

	// 闭环一笔逾期的待处理故障(待处理允许直接关闭/作废)。
	_, err = h.faults.Close(ctx, l1Fault[0].ID, fault.CloseRequest{Remark: "现场确认误报, 作废"})
	require.NoError(t, err)

	after, err := h.status.Overview(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), after.Fault.OverdueTotal, "闭环后逾期数回落到 1(仅剩 L2)")
	require.Equal(t, int64(3), after.Fault.OpenTotal)
	require.Equal(t, int64(2), after.Fault.ClosedTotal)
	require.Equal(t, int64(4), after.Lamp.ByRunStatus[lamp.RunStatusNormal], "L1 恢复正常")

	rows, _, _, err := h.status.Lamps(ctx, status.LampQuery{PageSize: 200})
	require.NoError(t, err)
	l1 := rowByLamp(t, rows, "LD-C-001")
	require.False(t, l1.FaultOverdue)
	require.Equal(t, int64(0), l1.OpenFaults)
	require.Equal(t, lamp.RunStatusNormal, l1.RunStatus)

	exportRows, err := h.status.ExportRows(ctx, status.LampQuery{})
	require.NoError(t, err)
	var overdueFromExport int
	for _, row := range exportRows {
		if row.FaultOverdue {
			overdueFromExport++
		}
	}
	require.Equal(t, 1, overdueFromExport)

	// 逾期清单里不再有 L1。
	for _, item := range after.OverdueFaults {
		require.NotEqual(t, devices["LD-C-001"].Code, item.LampCode)
	}
}

// TestBackfillEarlyRepairUpdatesLatestAndTimeline 补录早期维修记录后,
// 最近维修仍指向开工最晚的记录, 时间线按业务时间排序。
func TestBackfillEarlyRepairUpdatesLatestAndTimeline(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	now := time.Now()
	device := h.createLamp(t, "LD-C-080", "园区北路")

	entity := h.createFaultAt(t, device.ID, now.Add(-72*time.Hour), "补录时间线验证")

	// 先有一条较近的维修并修复。
	late := h.startRepairAt(t, entity.ID, "维修工甲", now.Add(-10*time.Hour))
	h.finishRepairAt(t, late, repair.ResultFixed, now.Add(-9*time.Hour))

	// 再补录一条更早的维修(也已修复), 故障经历 repaired -> processing -> repaired。
	early := h.startRepairAt(t, entity.ID, "维修工乙", now.Add(-24*time.Hour))
	h.finishRepairAt(t, early, repair.ResultFixed, now.Add(-20*time.Hour))

	// 清单上的"最近维修"必须是开工最晚的 late, 而不是后补录的 early。
	rows, total, _, err := h.status.Lamps(ctx, status.LampQuery{Keyword: device.Code})
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, rows, 1)
	require.Equal(t, late.RepairNo, rows[0].RepairNo)
	require.Equal(t, repair.ResultFixed, rows[0].RepairResult)

	// 追踪时间线严格按业务时间升序: 上报 -> 早开工 -> 早完工 -> 晚开工 -> 晚完工。
	track, err := h.status.Track(ctx, status.TrackQuery{FaultNo: entity.FaultNo})
	require.NoError(t, err)
	require.Len(t, track.Timeline, 5)
	expectedStages := []string{
		"reported", "repair_started", "repair_finished", "repair_started", "repair_finished",
	}
	for i, stage := range expectedStages {
		require.Equal(t, stage, track.Timeline[i].Stage, "时间线第 %d 个节点", i+1)
		if i > 0 {
			require.False(t, track.Timeline[i].Timestamp.Before(track.Timeline[i-1].Timestamp),
				"时间线必须按时间升序")
		}
	}
	require.Equal(t, early.RepairNo, track.Repairs[0].RepairNo, "维修记录按开工时间正序")
	require.Equal(t, late.RepairNo, track.Repairs[1].RepairNo)

	// 删除"最晚"的维修后, 最近维修回退为补录的早期记录。
	require.NoError(t, h.repairs.Delete(ctx, late.ID))
	rowsAfter, _, _, err := h.status.Lamps(ctx, status.LampQuery{Keyword: device.Code})
	require.NoError(t, err)
	require.Equal(t, early.RepairNo, rowsAfter[0].RepairNo)
	trackAfter, err := h.status.Track(ctx, status.TrackQuery{FaultNo: entity.FaultNo})
	require.NoError(t, err)
	require.Len(t, trackAfter.Timeline, 3, "删除后剩上报/早开工/早完工三个节点")
}

// TestExportHTTPEndpoint 通过 HTTP 调用导出端点, 校验状态码、文件名、BOM、
// 表头与行数, 并解析 CSV 验证逾期列与概览一致。
func TestExportHTTPEndpoint(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	overdueDataset(t, h)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	handler := status.NewHandler(h.status)
	engine.GET("/status/lamps/export", handler.Export)

	req := httptest.NewRequest(http.MethodGet, "/status/lamps/export?only_open=true", nil)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "text/csv; charset=utf-8", recorder.Header().Get("Content-Type"))
	require.Contains(t, recorder.Header().Get("Content-Disposition"),
		"attachment; filename=streetlight-status-")
	require.True(t, strings.HasPrefix(recorder.Body.String(), "\xEF\xBB\xBF"), "CSV 需要 UTF-8 BOM")

	reader := csv.NewReader(recorder.Body)
	records, err := reader.ReadAll()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(records), 2)

	header := records[0]
	header[0] = strings.TrimPrefix(header[0], "\xEF\xBB\xBF")
	index := map[string]int{}
	for i, name := range header {
		index[name] = i
	}
	for _, column := range []string{"路灯编号", "未闭环故障数", "是否逾期", "当前故障状态"} {
		_, ok := index[column]
		require.True(t, ok, "导出缺少列: %s", column)
	}

	// only_open=true: 4 行数据(L1/L2/L3/L4), 其中逾期 2 行(L1 待处理, L2 维修中)。
	require.Len(t, records, 5, "表头 + 4 条未闭环路灯")
	var overdueYes int
	overdueStatuses := map[string]int{}
	for _, record := range records[1:] {
		require.NotEmpty(t, record[index["路灯编号"]])
		require.NotEqual(t, "0", record[index["未闭环故障数"]])
		if record[index["是否逾期"]] == "是" {
			overdueYes++
			overdueStatuses[record[index["当前故障状态"]]]++
		}
	}
	require.Equal(t, 1, overdueStatuses["待处理"], "逾期清单含 1 条待处理(L1)")
	require.Equal(t, 1, overdueStatuses["维修中"], "逾期清单含 1 条维修中(L2)")
	overview, err := h.status.Overview(ctx)
	require.NoError(t, err)
	require.Equal(t, int(overview.Fault.OverdueTotal), overdueYes,
		"导出中的逾期行数必须与概览 overdue_total 一致")
}
