package status

import (
	"bytes"
	"context"
	"encoding/csv"
	"strconv"
	"time"

	"streetlight/internal/modules/fault"
	"streetlight/internal/modules/lamp"
	"streetlight/internal/modules/repair"
)

// exportPageSize 是导出时分批读取的批次大小, 避免一次性加载全量台账。
const exportPageSize = 500

// lampStatusCSVHeader 是维修状态清单导出文件的表头, 与列表字段一一对应。
var lampStatusCSVHeader = []string{
	"路灯编号", "名称", "所在道路", "区域", "灯具类型", "运行状态",
	"故障总数", "未闭环故障数", "是否闭环",
	"当前故障单号", "当前故障类型", "当前故障等级", "当前故障状态", "故障上报时间", "是否逾期",
	"最近维修单号", "最近维修人", "最近维修状态", "最近维修结果", "最近完工时间",
}

// ExportLamps 导出维修状态清单 CSV。
// 与列表接口共用同一套过滤条件与逾期/闭环口径, 保证同一批数据下三者结论一致。
func (s *Service) ExportLamps(ctx context.Context, query LampQuery) ([]byte, error) {
	buffer := &bytes.Buffer{}
	// 写入 UTF-8 BOM, 保证 Excel 打开时中文不乱码。
	buffer.Write([]byte{0xEF, 0xBB, 0xBF})

	writer := csv.NewWriter(buffer)
	if err := writer.Write(lampStatusCSVHeader); err != nil {
		return nil, err
	}

	// 分批读取全量匹配数据, 复用列表的行组装逻辑保证口径一致。
	for offset := 0; ; offset += exportPageSize {
		devices := make([]lamp.Lamp, 0)
		err := s.lampFilter(ctx, query).
			Order("lamp.id").
			Offset(offset).Limit(exportPageSize).
			Find(&devices).Error
		if err != nil {
			return nil, err
		}
		if len(devices) == 0 {
			break
		}

		rows, err := s.buildLampRows(ctx, devices)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if err := writer.Write(lampStatusCSVRecord(row)); err != nil {
				return nil, err
			}
		}
		if len(devices) < exportPageSize {
			break
		}
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// lampStatusCSVRecord 将一行维修状态转换为 CSV 记录, 状态字段输出中文名称。
func lampStatusCSVRecord(row LampStatusRow) []string {
	closed := "已闭环"
	if row.OpenFaults > 0 {
		closed = "未闭环"
	}
	overdue := "否"
	if row.Overdue {
		overdue = "是"
	}
	return []string{
		row.LampCode,
		row.LampName,
		row.RoadName,
		row.District,
		row.LampType,
		lamp.RunStatusLabel(row.RunStatus),
		strconv.FormatInt(row.TotalFaults, 10),
		strconv.FormatInt(row.OpenFaults, 10),
		closed,
		row.FaultNo,
		row.FaultType,
		fault.LevelLabel(row.FaultLevel),
		fault.StatusLabel(row.FaultStatus),
		formatExportTime(row.FaultReported),
		overdue,
		row.RepairNo,
		row.Repairman,
		repair.StatusLabel(row.RepairStatus),
		repair.ResultLabel(row.RepairResult),
		formatExportTime(row.RepairedAt),
	}
}

// formatExportTime 格式化可空时间, 空值输出空串。
func formatExportTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.Format("2006-01-02 15:04:05")
}
