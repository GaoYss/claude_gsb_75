package status

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"streetlight/internal/httpx"
	"streetlight/internal/response"
)

// exportColumns 定义导出 CSV 的表头与取值顺序, 与状态清单页面字段一一对应。
var exportColumns = []struct {
	header string
	value  func(*ExportRow) string
}{
	{"路灯编号", func(r *ExportRow) string { return r.LampCode }},
	{"名称", func(r *ExportRow) string { return r.LampName }},
	{"所在道路", func(r *ExportRow) string { return r.RoadName }},
	{"运行状态", func(r *ExportRow) string { return r.RunStatus }},
	{"累计故障数", func(r *ExportRow) string { return strconv.FormatInt(r.TotalFaults, 10) }},
	{"未闭环故障数", func(r *ExportRow) string { return strconv.FormatInt(r.OpenFaults, 10) }},
	{"当前故障单号", func(r *ExportRow) string { return r.FaultNo }},
	{"当前故障状态", func(r *ExportRow) string { return r.FaultStatus }},
	{"当前故障类型", func(r *ExportRow) string { return r.FaultType }},
	{"上报时间", func(r *ExportRow) string { return formatTime(r.FaultReported) }},
	{"是否逾期", func(r *ExportRow) string { return boolText(r.FaultOverdue) }},
	{"最近维修单号", func(r *ExportRow) string { return r.RepairNo }},
	{"最近维修人员", func(r *ExportRow) string { return r.Repairman }},
	{"最近维修状态", func(r *ExportRow) string { return r.RepairStatus }},
	{"最近维修结果", func(r *ExportRow) string { return r.RepairResult }},
	{"最近完工时间", func(r *ExportRow) string { return formatTime(r.RepairedAt) }},
}

// Handler 处理维修状态查询相关的 HTTP 请求。
type Handler struct {
	service *Service
}

// NewHandler 构造维修状态查询处理器。
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Overview 维修状态总览看板。
func (h *Handler) Overview(c *gin.Context) {
	result, err := h.service.Overview(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// Lamps 路灯维修状态列表。
func (h *Handler) Lamps(c *gin.Context) {
	var query LampQuery
	if err := httpx.BindQuery(c, &query); err != nil {
		response.Fail(c, err)
		return
	}
	items, total, page, err := h.service.Lamps(c.Request.Context(), query)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, response.NewPageData(items, total, page.Page, page.PageSize))
}

// Export 按与 Lamps 相同的过滤条件导出全部路灯状态行(CSV),
// 数据直接来自状态行组装逻辑, 保证导出与清单、概览口径一致。
func (h *Handler) Export(c *gin.Context) {
	var query LampQuery
	if err := httpx.BindQuery(c, &query); err != nil {
		response.Fail(c, err)
		return
	}
	rows, err := h.service.ExportRows(c.Request.Context(), query)
	if err != nil {
		response.Fail(c, err)
		return
	}

	buffer := &bytes.Buffer{}
	// UTF-8 BOM, 保证 Excel 直接打开时中文不乱码。
	buffer.WriteString("\xEF\xBB\xBF")
	writer := csv.NewWriter(buffer)

	header := make([]string, 0, len(exportColumns))
	for _, column := range exportColumns {
		header = append(header, column.header)
	}
	if err := writer.Write(header); err != nil {
		response.Fail(c, err)
		return
	}
	for index := range rows {
		record := make([]string, 0, len(exportColumns))
		for _, column := range exportColumns {
			record = append(record, column.value(&rows[index]))
		}
		if err := writer.Write(record); err != nil {
			response.Fail(c, err)
			return
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		response.Fail(c, err)
		return
	}

	filename := fmt.Sprintf("streetlight-status-%s.csv", time.Now().Format("20060102-150405"))
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", "attachment; filename="+filename)
	c.Data(http.StatusOK, "text/csv; charset=utf-8", buffer.Bytes())
}

// Track 维修状态全链路追踪。
func (h *Handler) Track(c *gin.Context) {
	var query TrackQuery
	if err := httpx.BindQuery(c, &query); err != nil {
		response.Fail(c, err)
		return
	}
	result, err := h.service.Track(c.Request.Context(), query)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// formatTime 按统一格式输出时间, nil 时返回空串。
func formatTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.Local().Format("2006-01-02 15:04:05")
}

func boolText(value bool) string {
	if value {
		return "是"
	}
	return "否"
}
