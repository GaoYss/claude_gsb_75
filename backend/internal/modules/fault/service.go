package fault

import (
	"context"
	"strings"
	"time"

	"streetlight/internal/apperr"
	"streetlight/internal/modules/lamp"
	"streetlight/pkg/pagination"
)

// faultSortSpec 定义故障列表接口允许的排序字段白名单。
var faultSortSpec = pagination.SortSpec{
	Allowed: map[string]string{
		"fault_no":    "fault_no",
		"reported_at": "reported_at",
		"status":      "status",
		"fault_type":  "fault_type",
		"fault_level": "fault_level",
		"created_at":  "created_at",
		"updated_at":  "updated_at",
	},
	Default: "reported_at",
}

// LampPort 由路灯台账模块实现, 故障模块通过它读取路灯信息并联动运行状态。
type LampPort interface {
	Get(ctx context.Context, id uint) (*lamp.Lamp, error)
	UpdateRunStatus(ctx context.Context, id uint, status string) error
}

// Service 承载故障登记的业务规则, 并向维修模块提供故障状态流转能力。
type Service struct {
	repo  *Repository
	lamps LampPort
}

// NewService 构造故障登记服务。
func NewService(repo *Repository, lamps LampPort) *Service {
	return &Service{repo: repo, lamps: lamps}
}

// Repository 暴露仓储, 供 bootstrap 装配其它模块所需的端口。
func (s *Service) Repository() *Repository { return s.repo }

// GetByID 查询故障详情, 同时满足维修模块 FaultPort 端口定义。
func (s *Service) GetByID(ctx context.Context, id uint) (*Fault, error) {
	return s.repo.GetByID(ctx, id)
}

// GetByNo 按故障单号查询故障。
func (s *Service) GetByNo(ctx context.Context, faultNo string) (*Fault, error) {
	return s.repo.GetByNo(ctx, faultNo)
}

// List 分页查询故障列表, 同时返回归一化后的分页信息。
func (s *Service) List(ctx context.Context, query ListQuery) ([]Fault, int64, pagination.Query, error) {
	page := pagination.Parse(query.Params, faultSortSpec)
	filter, err := buildFilter(query)
	if err != nil {
		return nil, 0, page, err
	}
	items, total, err := s.repo.List(ctx, filter, page)
	if err != nil {
		return nil, 0, page, err
	}
	return items, total, page, nil
}

// ListByLamp 查询某盏路灯的故障历史。
func (s *Service) ListByLamp(ctx context.Context, lampID uint) ([]Fault, error) {
	return s.repo.ListByLamp(ctx, lampID)
}

// Create 登记故障: 校验路灯存在、无未闭环故障后落库, 并同步路灯运行状态。
// 写库与路灯状态联动在同一事务内完成, 任何一步失败都整体回滚。
func (s *Service) Create(ctx context.Context, req CreateRequest) (entity *Fault, err error) {
	device, err := s.lamps.Get(ctx, req.LampID)
	if err != nil {
		return nil, err
	}

	faultType := strings.TrimSpace(req.FaultType)
	if !isValidFaultType(faultType) {
		return nil, apperr.BadRequest("非法的故障类型: %s", faultType)
	}

	description := strings.TrimSpace(req.Description)
	if description == "" {
		return nil, apperr.BadRequest("故障描述不能为空")
	}

	level := strings.TrimSpace(req.FaultLevel)
	if level == "" {
		level = LevelNormal
	}
	source := strings.TrimSpace(req.Source)
	if source == "" {
		source = SourceInspection
	}

	reportedAt, err := parseReportedAt(req.ReportedAt)
	if err != nil {
		return nil, err
	}

	entity = &Fault{
		LampID:        device.ID,
		LampCode:      device.Code,
		RoadName:      device.RoadName,
		FaultType:     faultType,
		FaultLevel:    level,
		Source:        source,
		Description:   description,
		Reporter:      strings.TrimSpace(req.Reporter),
		ReporterPhone: strings.TrimSpace(req.ReporterPhone),
		ReportedAt:    reportedAt,
		Status:        StatusPending,
	}

	err = s.repo.InTransaction(ctx, func(txCtx context.Context) error {
		openCount, err := s.repo.CountOpenByLamp(txCtx, device.ID)
		if err != nil {
			return err
		}
		if openCount > 0 {
			return apperr.Conflict("路灯 %s 已存在 %d 条未闭环故障, 请先处理后再登记", device.Code, openCount)
		}

		if err := s.repo.CreateWithUniqueNo(txCtx, entity, faultNoPrefix(reportedAt)); err != nil {
			return err
		}
		return s.syncLampStatus(txCtx, device.ID)
	})
	if err != nil {
		return nil, err
	}
	return entity, nil
}

// Update 修改故障登记信息, 已关闭的故障不允许修改。
// 条件更新保证: 读取后若故障被其它请求关闭, 修改不会覆盖终态。
func (s *Service) Update(ctx context.Context, id uint, req UpdateRequest) (*Fault, error) {
	entity, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if entity.Status == StatusClosed {
		return nil, apperr.Conflict("故障 %s 已关闭, 不允许修改", entity.FaultNo)
	}

	columns := map[string]any{}
	if req.FaultType != nil {
		value := strings.TrimSpace(*req.FaultType)
		if !isValidFaultType(value) {
			return nil, apperr.BadRequest("非法的故障类型: %s", value)
		}
		columns["fault_type"] = value
	}
	if req.FaultLevel != nil {
		columns["fault_level"] = strings.TrimSpace(*req.FaultLevel)
	}
	if req.Source != nil {
		columns["source"] = strings.TrimSpace(*req.Source)
	}
	if req.Description != nil {
		value := strings.TrimSpace(*req.Description)
		if value == "" {
			return nil, apperr.BadRequest("故障描述不能为空")
		}
		columns["description"] = value
	}
	if req.Reporter != nil {
		columns["reporter"] = strings.TrimSpace(*req.Reporter)
	}
	if req.ReporterPhone != nil {
		columns["reporter_phone"] = strings.TrimSpace(*req.ReporterPhone)
	}
	if req.ReportedAt != nil {
		value, err := parseReportedAt(*req.ReportedAt)
		if err != nil {
			return nil, err
		}
		columns["reported_at"] = value
	}

	if len(columns) > 0 {
		ok, err := s.repo.UpdateFieldsUnlessStatus(ctx, id, StatusClosed, columns)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, apperr.Conflict("故障 %s 已关闭, 不允许修改", entity.FaultNo)
		}
	}
	return s.repo.GetByID(ctx, id)
}

// Close 关闭故障, 用于确认闭环或作废处理。
// 使用条件更新守卫当前状态: 同一故障被并发/重复提交关闭时, 只有一次生效。
func (s *Service) Close(ctx context.Context, id uint, req CloseRequest) (entity *Fault, err error) {
	entity, err = s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if entity.Status == StatusClosed {
		return nil, apperr.Conflict("故障 %s 已关闭, 无需重复操作", entity.FaultNo)
	}
	if !canTransitTo(entity.Status, StatusClosed) {
		return nil, apperr.Conflict("故障 %s 当前状态为 %s, 不允许关闭", entity.FaultNo, StatusLabel(entity.Status))
	}

	now := time.Now()
	columns := map[string]any{
		"status":       StatusClosed,
		"closed_at":    now,
		"close_remark": strings.TrimSpace(req.Remark),
	}
	err = s.repo.InTransaction(ctx, func(txCtx context.Context) error {
		ok, err := s.repo.TransitionStatus(txCtx, id, validSourceStatuses(StatusClosed), columns)
		if err != nil {
			return err
		}
		if !ok {
			// 状态在读取后被其它请求改变, 重新读取以给出准确的冲突原因。
			current, getErr := s.repo.GetByID(txCtx, id)
			if getErr != nil {
				return getErr
			}
			if current.Status == StatusClosed {
				return apperr.Conflict("故障 %s 已关闭, 无需重复操作", current.FaultNo)
			}
			return apperr.Conflict("故障 %s 当前状态为 %s, 不允许关闭", current.FaultNo, StatusLabel(current.Status))
		}
		return s.syncLampStatus(txCtx, entity.LampID)
	})
	if err != nil {
		return nil, err
	}
	entity.Status = StatusClosed
	entity.ClosedAt = &now
	entity.CloseRemark = columns["close_remark"].(string)
	return entity, nil
}

// validSourceStatuses 返回状态机中允许流转到 target 的全部源状态(不含 target 自身),
// 供条件更新构造 WHERE status IN (...) 守卫; 停留在 target 上的重复提交会被挡下。
func validSourceStatuses(target string) []string {
	result := make([]string, 0, 4)
	for _, from := range Statuses() {
		if from != target && canTransitTo(from, target) {
			result = append(result, from)
		}
	}
	return result
}

// Delete 删除故障, 仅允许删除已关闭且没有维修记录的故障。
func (s *Service) Delete(ctx context.Context, id uint) error {
	entity, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if entity.Status != StatusClosed {
		return apperr.Conflict("仅已关闭的故障允许删除, 当前状态: %s", StatusLabel(entity.Status))
	}
	if entity.RepairCount > 0 {
		return apperr.Conflict("该故障已产生 %d 条维修记录, 不允许删除", entity.RepairCount)
	}
	return s.repo.InTransaction(ctx, func(txCtx context.Context) error {
		if err := s.repo.Delete(txCtx, id); err != nil {
			return err
		}
		return s.syncLampStatus(txCtx, entity.LampID)
	})
}

// Metadata 返回故障模块字典。
func (s *Service) Metadata() *Meta {
	return &Meta{
		Statuses:   Statuses(),
		Levels:     Levels(),
		Sources:    Sources(),
		FaultTypes: FaultTypes(),
	}
}

// OnRepairStarted 维修开工: 故障进入维修中, 并写入维修次数与最近维修记录。
// repairCount / latestRepairID 由维修模块在同一事务内落库后重新统计得出,
// 因此补录较早的开工记录不会把 latest_repair_id 错误指向旧记录。
// 条件更新保证: 终态(已关闭)故障不会被驱动, 并发重复开工只有一次生效。
func (s *Service) OnRepairStarted(ctx context.Context, faultID uint, repairCount int, latestRepairID *uint) error {
	entity, err := s.repo.GetByID(ctx, faultID)
	if err != nil {
		return err
	}

	columns := map[string]any{
		"status":           StatusProcessing,
		"repair_count":     repairCount,
		"latest_repair_id": latestRepairID,
	}
	// pending 首次开工 / processing 返修(上一次结果非已修复) / repaired 回退返修 都合法。
	ok, err := s.repo.TransitionStatus(ctx, faultID,
		[]string{StatusPending, StatusProcessing, StatusRepaired}, columns)
	if err != nil {
		return err
	}
	if !ok {
		current, getErr := s.repo.GetByID(ctx, faultID)
		if getErr != nil {
			return getErr
		}
		return apperr.Conflict("故障 %s 当前状态为 %s, 不允许开工维修", current.FaultNo, StatusLabel(current.Status))
	}
	return s.syncLampStatus(ctx, entity.LampID)
}

// OnRepairFinished 维修完成: 结果为已修复时故障转为已修复, 否则保持维修中。
func (s *Service) OnRepairFinished(ctx context.Context, faultID uint, fixed bool) error {
	entity, err := s.repo.GetByID(ctx, faultID)
	if err != nil {
		return err
	}
	if fixed {
		ok, err := s.repo.TransitionStatus(ctx, faultID,
			[]string{StatusProcessing}, map[string]any{"status": StatusRepaired})
		if err != nil {
			return err
		}
		if !ok {
			current, getErr := s.repo.GetByID(ctx, faultID)
			if getErr != nil {
				return getErr
			}
			if current.Status == StatusRepaired {
				return apperr.Conflict("故障 %s 已修复, 请勿重复提交完工结果", current.FaultNo)
			}
			return apperr.Conflict("故障 %s 当前状态为 %s, 无法标记为已修复", current.FaultNo, StatusLabel(current.Status))
		}
	}
	return s.syncLampStatus(ctx, entity.LampID)
}

// SyncRepairStats 同步维修次数与最新维修记录, 删除维修记录后回退未开工状态。
// 次数归零时: 维修中/已修复的故障都回退到待处理(唯一能把它推到这两个状态的
// 维修记录已不存在); 已关闭终态不受影响(其维修记录本来就禁止删除)。
func (s *Service) SyncRepairStats(ctx context.Context, faultID uint, repairCount int, latestRepairID *uint) error {
	entity, err := s.repo.GetByID(ctx, faultID)
	if err != nil {
		return err
	}

	columns := map[string]any{
		"repair_count":     repairCount,
		"latest_repair_id": latestRepairID,
	}
	if repairCount == 0 {
		switch entity.Status {
		case StatusProcessing, StatusRepaired:
			columns["status"] = StatusPending
		}
	}

	if err := s.repo.UpdateColumns(ctx, faultID, columns); err != nil {
		return err
	}
	return s.syncLampStatus(ctx, entity.LampID)
}

// syncLampStatus 依据该路灯的故障分布重新计算并写回运行状态。
func (s *Service) syncLampStatus(ctx context.Context, lampID uint) error {
	counts, err := s.repo.StatusCountsForLamp(ctx, lampID)
	if err != nil {
		return err
	}

	status := lamp.RunStatusNormal
	switch {
	case counts[StatusProcessing] > 0:
		status = lamp.RunStatusMaintenance
	case counts[StatusPending] > 0:
		status = lamp.RunStatusFault
	}
	return s.lamps.UpdateRunStatus(ctx, lampID, status)
}

// buildFilter 将列表查询参数转换为仓储条件, 并解析日期区间。
func buildFilter(query ListQuery) (Filter, error) {
	filter := Filter{
		Keyword:    strings.TrimSpace(query.Keyword),
		Status:     strings.TrimSpace(query.Status),
		FaultType:  strings.TrimSpace(query.FaultType),
		FaultLevel: strings.TrimSpace(query.FaultLevel),
		Source:     strings.TrimSpace(query.Source),
		LampID:     query.LampID,
		RoadName:   strings.TrimSpace(query.RoadName),
		OnlyOpen:   query.OnlyOpen,
	}

	if filter.Status != "" && !IsValidStatus(filter.Status) {
		return filter, apperr.BadRequest("非法的故障状态: %s", filter.Status)
	}

	if strings.TrimSpace(query.StartDate) != "" {
		from, err := parseDay(query.StartDate)
		if err != nil {
			return filter, err
		}
		filter.ReportedFrom = &from
	}
	if strings.TrimSpace(query.EndDate) != "" {
		to, err := parseDay(query.EndDate)
		if err != nil {
			return filter, err
		}
		to = to.AddDate(0, 0, 1)
		filter.ReportedTo = &to
	}
	if filter.ReportedFrom != nil && filter.ReportedTo != nil && filter.ReportedTo.Before(*filter.ReportedFrom) {
		return filter, apperr.BadRequest("结束日期不能早于开始日期")
	}
	return filter, nil
}

// faultNoPrefix 生成故障单号前缀, 例如 GD20260913。
func faultNoPrefix(reportedAt time.Time) string {
	return "GD" + reportedAt.Format("20060102")
}

// parseReportedAt 解析上报时间, 支持 RFC3339 与常见的日期时间格式, 为空时取当前时间。
func parseReportedAt(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Now(), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02"} {
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, apperr.BadRequest("上报时间格式不正确, 建议使用 YYYY-MM-DD HH:mm:ss: %s", value)
}

// parseDay 解析 YYYY-MM-DD 日期, 返回当天零点。
func parseDay(value string) (time.Time, error) {
	date, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(value), time.Local)
	if err != nil {
		return time.Time{}, apperr.BadRequest("日期格式应为 YYYY-MM-DD, 当前值: %s", value)
	}
	return date, nil
}
