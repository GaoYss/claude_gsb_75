package status_test

import (
	"context"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"streetlight/internal/modules/fault"
	"streetlight/internal/modules/lamp"
	"streetlight/internal/modules/repair"
	"streetlight/internal/modules/status"
	"streetlight/pkg/pagination"
)

// ---------------------------------------------------------------------------
// 公共辅助
// ---------------------------------------------------------------------------

// createFaultReportedAt 登记一条指定上报时间的故障。
func (h *harness) createFaultReportedAt(t *testing.T, lampID uint, faultType string, reportedAt time.Time) *fault.Fault {
	t.Helper()
	entity, err := h.faults.Create(context.Background(), fault.CreateRequest{
		LampID:      lampID,
		FaultType:   faultType,
		FaultLevel:  fault.LevelHigh,
		Description: "口径一致性测试",
		Reporter:    "巡检员",
		ReportedAt:  reportedAt.Format("2006-01-02 15:04:05"),
	})
	require.NoError(t, err)
	return entity
}

// startRepairAt 在指定时间开工。
func (h *harness) startRepairAt(t *testing.T, faultID uint, repairman string, startedAt time.Time) *repair.Repair {
	t.Helper()
	record, err := h.repairs.Create(context.Background(), repair.CreateRequest{
		FaultID:   faultID,
		Repairman: repairman,
		StartedAt: startedAt.Format("2006-01-02 15:04:05"),
	})
	require.NoError(t, err)
	return record
}

// finishRepairAt 在指定时间以指定结果完工。
func (h *harness) finishRepairAt(t *testing.T, repairID uint, result string, finishedAt time.Time) *repair.Repair {
	t.Helper()
	record, err := h.repairs.Finish(context.Background(), repairID, repair.FinishRequest{
		Result:     result,
		FinishedAt: finishedAt.Format("2006-01-02 15:04:05"),
	})
	require.NoError(t, err)
	return record
}

// parseExportCSV 解析导出的 CSV 内容(含 UTF-8 BOM), 返回表头下标与数据行。
func parseExportCSV(t *testing.T, data []byte) (map[string]int, [][]string) {
	t.Helper()
	content := strings.TrimPrefix(string(data), "\uFEFF")
	reader := csv.NewReader(strings.NewReader(content))
	records, err := reader.ReadAll()
	require.NoError(t, err)
	require.NotEmpty(t, records, "导出内容至少应包含表头")

	header := make(map[string]int, len(records[0]))
	for index, name := range records[0] {
		header[name] = index
	}
	return header, records[1:]
}

// consistencyBatch 是一批覆盖全部故障状态的测试数据。
type consistencyBatch struct {
	overdueLamp   *lamp.Lamp // 待处理且超 24 小时: 逾期
	pendingLamp   *lamp.Lamp // 待处理但未超期
	repairingLamp *lamp.Lamp // 维修中: 未闭环但不计入逾期
	repairedLamp  *lamp.Lamp // 已修复: 已脱离未闭环口径
	closedLamp    *lamp.Lamp // 已关闭: 完整闭环
	cleanLamp     *lamp.Lamp // 无故障
}

// buildConsistencyBatch 在同一批数据上构造六种典型状态的路灯。
func buildConsistencyBatch(t *testing.T, h *harness) *consistencyBatch {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	batch := &consistencyBatch{}

	// 逾期: 72 小时前上报, 仍待处理
	batch.overdueLamp = h.createLamp(t, "LD-X-001", "中山路")
	h.createFaultReportedAt(t, batch.overdueLamp.ID, "灯不亮", now.Add(-72*time.Hour))

	// 未逾期: 1 小时前上报, 待处理
	batch.pendingLamp = h.createLamp(t, "LD-X-002", "中山路")
	h.createFaultReportedAt(t, batch.pendingLamp.ID, "灯光闪烁", now.Add(-1*time.Hour))

	// 维修中: 30 小时前上报但已开工, 不计入逾期
	batch.repairingLamp = h.createLamp(t, "LD-X-003", "建设大道")
	repairingFault := h.createFaultReportedAt(t, batch.repairingLamp.ID, "线路故障", now.Add(-30*time.Hour))
	h.startRepairAt(t, repairingFault.ID, "维修工甲", now.Add(-2*time.Hour))

	// 已修复
	batch.repairedLamp = h.createLamp(t, "LD-X-004", "建设大道")
	repairedFault := h.createFaultReportedAt(t, batch.repairedLamp.ID, "灯具常亮", now.Add(-10*time.Hour))
	repairedRecord := h.startRepairAt(t, repairedFault.ID, "维修工乙", now.Add(-9*time.Hour))
	h.finishRepairAt(t, repairedRecord.ID, repair.ResultFixed, now.Add(-8*time.Hour))

	// 已关闭(完整闭环)
	batch.closedLamp = h.createLamp(t, "LD-X-005", "解放路")
	closedFault := h.createFaultReportedAt(t, batch.closedLamp.ID, "灯杆倾斜", now.Add(-50*time.Hour))
	closedRecord := h.startRepairAt(t, closedFault.ID, "维修工丙", now.Add(-49*time.Hour))
	h.finishRepairAt(t, closedRecord.ID, repair.ResultFixed, now.Add(-48*time.Hour))
	_, err := h.faults.Close(ctx, closedFault.ID, fault.CloseRequest{Remark: "复核闭环"})
	require.NoError(t, err)

	// 无故障
	batch.cleanLamp = h.createLamp(t, "LD-X-006", "解放路")

	return batch
}

// ---------------------------------------------------------------------------
// 概览 / 维修状态清单 / 导出 在同一批数据下的逾期与闭环口径一致
// ---------------------------------------------------------------------------

func TestOverviewLampsExportConsistency(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	batch := buildConsistencyBatch(t, h)

	// ---- 概览口径 ----
	overview, err := h.status.Overview(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(6), overview.Lamp.Total)
	require.Equal(t, int64(5), overview.Fault.Total)
	require.Equal(t, int64(3), overview.Fault.OpenTotal, "未闭环 = 待处理2 + 维修中1")
	require.Equal(t, int64(1), overview.Fault.OverdueTotal, "仅 72 小时前仍待处理的故障计入逾期")
	require.Equal(t, int64(2), overview.Fault.ByStatus[fault.StatusPending])
	require.Equal(t, int64(1), overview.Fault.ByStatus[fault.StatusProcessing])
	require.Equal(t, int64(1), overview.Fault.ByStatus[fault.StatusRepaired])
	require.Equal(t, int64(1), overview.Fault.ByStatus[fault.StatusClosed])

	// ---- 维修状态清单口径 ----
	rows, total, _, err := h.status.Lamps(ctx, status.LampQuery{
		Params: paginationParams(100),
	})
	require.NoError(t, err)
	require.Equal(t, int64(6), total)
	require.Len(t, rows, 6)

	rowsByLamp := make(map[uint]status.LampStatusRow, len(rows))
	var listOpenSum, listOverdueCount int64
	for _, row := range rows {
		rowsByLamp[row.LampID] = row
		listOpenSum += row.OpenFaults
		if row.Overdue {
			listOverdueCount++
		}
	}
	require.Equal(t, overview.Fault.OpenTotal, listOpenSum, "清单未闭环合计应与概览一致")
	require.Equal(t, overview.Fault.OverdueTotal, listOverdueCount, "清单逾期合计应与概览一致")

	// 逾期仅命中"待处理且超期"的路灯, 维修中不算逾期
	require.True(t, rowsByLamp[batch.overdueLamp.ID].Overdue)
	require.False(t, rowsByLamp[batch.pendingLamp.ID].Overdue)
	require.False(t, rowsByLamp[batch.repairingLamp.ID].Overdue, "维修中不计入逾期")
	require.Equal(t, fault.StatusProcessing, rowsByLamp[batch.repairingLamp.ID].FaultStatus)
	require.Equal(t, int64(1), rowsByLamp[batch.repairingLamp.ID].OpenFaults)
	require.Equal(t, int64(0), rowsByLamp[batch.closedLamp.ID].OpenFaults)

	// ---- 导出口径 ----
	data, err := h.status.ExportLamps(ctx, status.LampQuery{})
	require.NoError(t, err)
	header, records := parseExportCSV(t, data)
	require.Len(t, records, 6, "导出行数应与清单一致")

	var exportOpenSum, exportOverdueCount, exportUnclosedCount int64
	exportByLamp := make(map[string][]string, len(records))
	for _, record := range records {
		code := record[header["路灯编号"]]
		exportByLamp[code] = record
		openCount, err := strconv.ParseInt(record[header["未闭环故障数"]], 10, 64)
		require.NoError(t, err)
		exportOpenSum += openCount
		if record[header["是否逾期"]] == "是" {
			exportOverdueCount++
		}
		if record[header["是否闭环"]] == "未闭环" {
			exportUnclosedCount++
		}
	}
	require.Equal(t, overview.Fault.OpenTotal, exportOpenSum, "导出未闭环合计应与概览一致")
	require.Equal(t, overview.Fault.OverdueTotal, exportOverdueCount, "导出逾期合计应与概览一致")
	require.Equal(t, overview.Fault.OpenTotal, exportUnclosedCount, "导出未闭环行数应与概览一致")

	// 导出与清单逐行一致(按路灯编号对齐)
	for _, row := range rows {
		record, ok := exportByLamp[row.LampCode]
		require.True(t, ok, "导出缺少路灯 %s", row.LampCode)
		openCount, err := strconv.ParseInt(record[header["未闭环故障数"]], 10, 64)
		require.NoError(t, err)
		require.Equal(t, row.OpenFaults, openCount, "路灯 %s 未闭环数不一致", row.LampCode)
		totalCount, err := strconv.ParseInt(record[header["故障总数"]], 10, 64)
		require.NoError(t, err)
		require.Equal(t, row.TotalFaults, totalCount, "路灯 %s 故障总数不一致", row.LampCode)
		require.Equal(t, row.FaultNo, record[header["当前故障单号"]], "路灯 %s 当前故障不一致", row.LampCode)
		expectOverdue := "否"
		if row.Overdue {
			expectOverdue = "是"
		}
		require.Equal(t, expectOverdue, record[header["是否逾期"]], "路灯 %s 逾期标记不一致", row.LampCode)
		expectClosed := "已闭环"
		if row.OpenFaults > 0 {
			expectClosed = "未闭环"
		}
		require.Equal(t, expectClosed, record[header["是否闭环"]], "路灯 %s 闭环标记不一致", row.LampCode)
	}

	// 逾期行内容核对: 状态为待处理, 故障单号与清单一致
	overdueRecord := exportByLamp[batch.overdueLamp.Code]
	require.Equal(t, "是", overdueRecord[header["是否逾期"]])
	require.Equal(t, "待处理", overdueRecord[header["当前故障状态"]])
	require.Equal(t, rowsByLamp[batch.overdueLamp.ID].FaultNo, overdueRecord[header["当前故障单号"]])
}

// TestExportSharesListFilters 验证导出与清单使用同一套过滤条件。
func TestExportSharesListFilters(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	buildConsistencyBatch(t, h)

	// 仅看未闭环: 清单与导出行数一致
	_, openTotal, _, err := h.status.Lamps(ctx, status.LampQuery{OnlyOpen: true})
	require.NoError(t, err)
	require.Equal(t, int64(3), openTotal)

	data, err := h.status.ExportLamps(ctx, status.LampQuery{OnlyOpen: true})
	require.NoError(t, err)
	_, records := parseExportCSV(t, data)
	require.Len(t, records, int(openTotal), "导出应与清单共用 only_open 过滤")

	// 按道路过滤: 清单与导出覆盖相同的路灯编号(清单默认按 id 倒序, 导出按 id 正序)
	rows, roadTotal, _, err := h.status.Lamps(ctx, status.LampQuery{
		RoadName: "中山路",
		Params:   paginationParams(100),
	})
	require.NoError(t, err)
	require.Equal(t, int64(2), roadTotal)

	data, err = h.status.ExportLamps(ctx, status.LampQuery{RoadName: "中山路"})
	require.NoError(t, err)
	header, records := parseExportCSV(t, data)
	require.Len(t, records, len(rows))
	listCodes := make([]string, 0, len(rows))
	for _, row := range rows {
		listCodes = append(listCodes, row.LampCode)
	}
	exportCodes := make([]string, 0, len(records))
	for _, record := range records {
		exportCodes = append(exportCodes, record[header["路灯编号"]])
	}
	require.ElementsMatch(t, listCodes, exportCodes, "导出应与清单共用道路过滤条件")
}

// TestExportEndpoint 通过 HTTP 层验证导出接口的响应头与内容。
func TestExportEndpoint(t *testing.T) {
	h := newHarness(t)
	buildConsistencyBatch(t, h)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := status.NewHandler(h.status)
	router.GET("/api/v1/status/lamps/export", handler.ExportLamps)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/status/lamps/export?only_open=true", nil)
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Header().Get("Content-Type"), "text/csv")
	require.Contains(t, recorder.Header().Get("Content-Disposition"), "attachment")
	require.True(t, strings.HasPrefix(recorder.Body.String(), "\uFEFF"), "导出内容应以 UTF-8 BOM 开头")

	_, records := parseExportCSV(t, recorder.Body.Bytes())
	require.Len(t, records, 3, "only_open 导出应只包含三盏有未闭环故障的路灯")
}

// ---------------------------------------------------------------------------
// 补录早期记录后, 最近维修与时间线的顺序
// ---------------------------------------------------------------------------

// TestBackfilledRepairKeepsOrdering 先登记一条较晚的维修, 再补录一条更早的维修,
// 最近维修必须仍按开工时间取最新一条, 时间线必须按时间先后排列。
func TestBackfilledRepairKeepsOrdering(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	device := h.createLamp(t, "LD-X-101", "学院路")
	base := time.Now().Add(-10 * time.Hour)
	entity := h.createFaultReportedAt(t, device.ID, "灯不亮", base)

	// 先登记较晚的维修: T+2h 开工, T+3h 完工
	later := h.startRepairAt(t, entity.ID, "维修工甲", base.Add(2*time.Hour))
	h.finishRepairAt(t, later.ID, repair.ResultPendingParts, base.Add(3*time.Hour))

	// 补录更早的维修: T+1h 开工, T+1.5h 完工
	earlier := h.startRepairAt(t, entity.ID, "维修工乙", base.Add(1*time.Hour))
	h.finishRepairAt(t, earlier.ID, repair.ResultObserving, base.Add(90*time.Minute))

	// 维修次数累计为 2
	updated, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, 2, updated.RepairCount)

	// 最近维修: 按开工时间取最新, 补录的早期记录不应覆盖它
	rows, total, _, err := h.status.Lamps(ctx, status.LampQuery{Params: paginationParams(100)})
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, rows, 1)
	require.Equal(t, later.RepairNo, rows[0].RepairNo, "补录早期记录后最近维修仍应是开工时间最新的一条")
	require.Equal(t, repair.ResultPendingParts, rows[0].RepairResult)

	// 维修记录列表按开工时间正序: 补录的记录排在前面
	track, err := h.status.Track(ctx, status.TrackQuery{FaultNo: entity.FaultNo})
	require.NoError(t, err)
	require.Len(t, track.Repairs, 2)
	require.Equal(t, earlier.RepairNo, track.Repairs[0].RepairNo)
	require.Equal(t, later.RepairNo, track.Repairs[1].RepairNo)

	// 时间线按时间先后排列: 上报 -> 补录开工 -> 补录完工 -> 较晚开工 -> 较晚完工
	require.Len(t, track.Timeline, 5)
	stages := make([]string, 0, len(track.Timeline))
	for _, event := range track.Timeline {
		stages = append(stages, event.Stage)
	}
	require.Equal(t, []string{
		"reported", "repair_started", "repair_finished", "repair_started", "repair_finished",
	}, stages)
	for i := 1; i < len(track.Timeline); i++ {
		require.False(t, track.Timeline[i].Timestamp.Before(track.Timeline[i-1].Timestamp),
			"时间线必须按时间先后排列: 第 %d 个事件早于前一个", i)
	}
	require.Equal(t, "维修工乙", track.Timeline[1].Operator, "补录的早期维修应排在时间线第二位")
	require.Equal(t, "维修工甲", track.Timeline[3].Operator)
}

// paginationParams 构造指定页大小的分页参数。
func paginationParams(pageSize int) pagination.Params {
	return pagination.Params{Page: 1, PageSize: pageSize}
}
