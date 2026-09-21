package repair

import (
	"context"
	"strings"
	"time"

	"streetlight/internal/apperr"
	"streetlight/internal/modules/fault"
	"streetlight/pkg/pagination"
)

// repairSortSpec 定义维修记录列表允许的排序字段白名单。
var repairSortSpec = pagination.SortSpec{
	Allowed: map[string]string{
		"repair_no":   "repair_no",
		"started_at":  "started_at",
		"finished_at": "finished_at",
		"status":      "status",
		"result":      "result",
		"cost":        "cost",
		"created_at":  "created_at",
	},
	Default: "started_at",
}

// FaultPort 由故障登记模块实现, 维修模块通过它联动故障状态与路灯状态。
type FaultPort interface {
	GetByID(ctx context.Context, id uint) (*fault.Fault, error)
	// OnRepairStarted 在维修记录落库后驱动故障进入维修中, count/latestID 由维修侧统计。
	OnRepairStarted(ctx context.Context, faultID uint, repairCount int, latestRepairID *uint) error
	OnRepairFinished(ctx context.Context, faultID uint, fixed bool) error
	SyncRepairStats(ctx context.Context, faultID uint, repairCount int, latestRepairID *uint) error
}

// Service 承载维修记录录入的业务规则。
type Service struct {
	repo   *Repository
	faults FaultPort
}

// NewService 构造维修记录服务。
func NewService(repo *Repository, faults FaultPort) *Service {
	return &Service{repo: repo, faults: faults}
}

// Get 查询维修记录详情。
func (s *Service) Get(ctx context.Context, id uint) (*Repair, error) {
	return s.repo.GetByID(ctx, id)
}

// List 分页查询维修记录。
func (s *Service) List(ctx context.Context, query ListQuery) ([]Repair, int64, pagination.Query, error) {
	page := pagination.Parse(query.Params, repairSortSpec)
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

// ListByFault 查询某条故障的维修过程记录。
func (s *Service) ListByFault(ctx context.Context, faultID uint) ([]Repair, error) {
	if _, err := s.faults.GetByID(ctx, faultID); err != nil {
		return nil, err
	}
	return s.repo.ListByFault(ctx, faultID)
}

// Create 录入维修记录(维修开工), 并联动故障与路灯状态。
// 维修记录写入、故障状态推进、路灯状态联动在同一事务内完成,
// 部分唯一索引保证并发重复开工时只会有一条记录落库。
func (s *Service) Create(ctx context.Context, req CreateRequest) (entity *Repair, err error) {
	target, err := s.faults.GetByID(ctx, req.FaultID)
	if err != nil {
		return nil, err
	}
	if target.Status == fault.StatusClosed {
		return nil, apperr.Conflict("故障 %s 已关闭, 不允许再登记维修记录", target.FaultNo)
	}

	repairman := strings.TrimSpace(req.Repairman)
	if repairman == "" {
		return nil, apperr.BadRequest("维修人员不能为空")
	}

	startedAt, err := parseTime(req.StartedAt, time.Now())
	if err != nil {
		return nil, err
	}
	if startedAt.Before(target.ReportedAt) {
		return nil, apperr.BadRequest("开工时间不能早于故障上报时间 %s", target.ReportedAt.Format("2006-01-02 15:04:05"))
	}

	entity = &Repair{
		FaultID:      target.ID,
		FaultNo:      target.FaultNo,
		LampID:       target.LampID,
		LampCode:     target.LampCode,
		Repairman:    repairman,
		RepairTeam:   strings.TrimSpace(req.RepairTeam),
		ContactPhone: strings.TrimSpace(req.ContactPhone),
		StartedAt:    startedAt,
		Status:       StatusOngoing,
		Content:      strings.TrimSpace(req.Content),
		Materials:    strings.TrimSpace(req.Materials),
		Cost:         valueOrZero(req.Cost),
		Remark:       strings.TrimSpace(req.Remark),
	}

	err = s.repo.InTransaction(ctx, func(txCtx context.Context) error {
		// 事务内先给出友好的冲突提示; 真正的并发防护由部分唯一索引兜底。
		ongoing, err := s.repo.GetOngoingByFault(txCtx, target.ID)
		if err != nil {
			return err
		}
		if ongoing != nil {
			return apperr.Conflict("故障 %s 已有进行中的维修记录 %s, 请先完成后再录入", target.FaultNo, ongoing.RepairNo)
		}

		if err := s.repo.CreateWithUniqueNo(txCtx, entity, "WX"+startedAt.Format("20060102")); err != nil {
			return err
		}

		count, err := s.repo.CountByFault(txCtx, target.ID)
		if err != nil {
			return err
		}
		latest, err := s.repo.LatestByFault(txCtx, target.ID)
		if err != nil {
			return err
		}
		var latestID *uint
		if latest != nil {
			latestID = &latest.ID
		}
		// 开工后: 故障转为维修中(已修复状态则回退返修), 路灯转为维修状态。
		return s.faults.OnRepairStarted(txCtx, target.ID, int(count), latestID)
	})
	if err != nil {
		return nil, err
	}

	entity.FillDuration()
	return entity, nil
}

// Update 修改维修记录, 已完成的记录不允许修改。
// 条件更新保证: 读取后若记录被其它请求完工, 修改不会覆盖完工结果。
func (s *Service) Update(ctx context.Context, id uint, req UpdateRequest) (*Repair, error) {
	entity, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if entity.Status == StatusFinished {
		return nil, apperr.Conflict("维修记录 %s 已完成, 不允许修改", entity.RepairNo)
	}

	columns := map[string]any{}
	if req.Repairman != nil {
		repairman := strings.TrimSpace(*req.Repairman)
		if repairman == "" {
			return nil, apperr.BadRequest("维修人员不能为空")
		}
		columns["repairman"] = repairman
	}
	if req.RepairTeam != nil {
		columns["repair_team"] = strings.TrimSpace(*req.RepairTeam)
	}
	if req.ContactPhone != nil {
		columns["contact_phone"] = strings.TrimSpace(*req.ContactPhone)
	}
	if req.StartedAt != nil {
		startedAt, err := parseTime(*req.StartedAt, entity.StartedAt)
		if err != nil {
			return nil, err
		}
		columns["started_at"] = startedAt
	}
	if req.Content != nil {
		columns["content"] = strings.TrimSpace(*req.Content)
	}
	if req.Materials != nil {
		columns["materials"] = strings.TrimSpace(*req.Materials)
	}
	if req.Cost != nil {
		columns["cost"] = *req.Cost
	}
	if req.Remark != nil {
		columns["remark"] = strings.TrimSpace(*req.Remark)
	}

	if len(columns) > 0 {
		ok, err := s.repo.UpdateWithStatusGuard(ctx, id, StatusOngoing, columns)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, apperr.Conflict("维修记录 %s 已完成, 不允许修改", entity.RepairNo)
		}
	}
	return s.repo.GetByID(ctx, id)
}

// Finish 完成维修: 记录结果与完工时间, 结果为已修复时联动故障转为已修复。
// 维修记录更新与故障推进在同一事务内, 任一步失败都回滚, 不会出现
// "接口报错但维修记录已完工" 的半成品状态。
func (s *Service) Finish(ctx context.Context, id uint, req FinishRequest) (entity *Repair, err error) {
	entity, err = s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if entity.Status == StatusFinished {
		return nil, apperr.Conflict("维修记录 %s 已完成, 不允许重复提交", entity.RepairNo)
	}

	result := strings.TrimSpace(req.Result)
	if !IsValidResult(result) {
		return nil, apperr.BadRequest("非法的维修结果: %s", result)
	}

	finishedAt, err := parseTime(req.FinishedAt, time.Now())
	if err != nil {
		return nil, err
	}
	if finishedAt.Before(entity.StartedAt) {
		return nil, apperr.BadRequest("完工时间不能早于开工时间")
	}

	columns := map[string]any{
		"finished_at": finishedAt,
		"status":      StatusFinished,
		"result":      result,
	}
	if content := strings.TrimSpace(req.Content); content != "" {
		columns["content"] = content
	}
	if materials := strings.TrimSpace(req.Materials); materials != "" {
		columns["materials"] = materials
	}
	if req.Cost != nil {
		columns["cost"] = *req.Cost
	}
	if remark := strings.TrimSpace(req.Remark); remark != "" {
		columns["remark"] = remark
	}

	err = s.repo.InTransaction(ctx, func(txCtx context.Context) error {
		ok, err := s.repo.UpdateWithStatusGuard(txCtx, id, StatusOngoing, columns)
		if err != nil {
			return err
		}
		if !ok {
			current, getErr := s.repo.GetByID(txCtx, id)
			if getErr != nil {
				return getErr
			}
			return apperr.Conflict("维修记录 %s 已完成, 不允许重复提交", current.RepairNo)
		}
		// 已修复: 故障推进到已修复; 其它结果: 故障保持维修中, 等待再次维修。
		return s.faults.OnRepairFinished(txCtx, entity.FaultID, result == ResultFixed)
	})
	if err != nil {
		return nil, err
	}

	return s.repo.GetByID(ctx, id)
}

// Delete 删除维修记录, 已关闭故障的维修记录不允许删除。
// 删除与故障维修次数/状态回退在同一事务内完成。
func (s *Service) Delete(ctx context.Context, id uint) error {
	entity, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	target, err := s.faults.GetByID(ctx, entity.FaultID)
	if err != nil {
		return err
	}
	if target.Status == fault.StatusClosed {
		return apperr.Conflict("故障 %s 已关闭, 不允许删除其维修记录", target.FaultNo)
	}

	return s.repo.InTransaction(ctx, func(txCtx context.Context) error {
		if err := s.repo.Delete(txCtx, id); err != nil {
			return err
		}

		count, err := s.repo.CountByFault(txCtx, entity.FaultID)
		if err != nil {
			return err
		}
		latest, err := s.repo.LatestByFault(txCtx, entity.FaultID)
		if err != nil {
			return err
		}
		var latestID *uint
		if latest != nil {
			latestID = &latest.ID
		}
		return s.faults.SyncRepairStats(txCtx, entity.FaultID, int(count), latestID)
	})
}

// Metadata 返回维修模块字典。
func (s *Service) Metadata(ctx context.Context) (*Meta, error) {
	repairmen, err := s.repo.DistinctValues(ctx, "repairman")
	if err != nil {
		return nil, err
	}
	teams, err := s.repo.DistinctValues(ctx, "repair_team")
	if err != nil {
		return nil, err
	}
	return &Meta{
		Statuses:  Statuses(),
		Results:   Results(),
		Repairmen: repairmen,
		Teams:     teams,
	}, nil
}

// Statistics 汇总维修统计信息。
func (s *Service) Statistics(ctx context.Context) (*Statistics, error) {
	total, err := s.repo.Count(ctx)
	if err != nil {
		return nil, err
	}
	byStatus, err := s.repo.CountByColumn(ctx, "status")
	if err != nil {
		return nil, err
	}
	totalCost, err := s.repo.SumCost(ctx)
	if err != nil {
		return nil, err
	}
	averageDuration, err := s.repo.AverageDurationHours(ctx)
	if err != nil {
		return nil, err
	}

	result := &Statistics{
		Total:             total,
		OngoingTotal:      byStatus[StatusOngoing],
		FinishedTotal:     byStatus[StatusFinished],
		TotalCost:         totalCost,
		AverageDurationHr: averageDuration,
	}
	if result.FinishedTotal > 0 {
		result.AverageCost = totalCost / float64(result.FinishedTotal)
	}
	return result, nil
}

// buildFilter 将查询参数转换为仓储条件并解析日期区间。
func buildFilter(query ListQuery) (Filter, error) {
	filter := Filter{
		Keyword:    strings.TrimSpace(query.Keyword),
		FaultID:    query.FaultID,
		LampID:     query.LampID,
		Repairman:  strings.TrimSpace(query.Repairman),
		RepairTeam: strings.TrimSpace(query.RepairTeam),
		Status:     strings.TrimSpace(query.Status),
		Result:     strings.TrimSpace(query.Result),
	}
	if filter.Status != "" && filter.Status != StatusOngoing && filter.Status != StatusFinished {
		return filter, apperr.BadRequest("非法的维修状态: %s", filter.Status)
	}
	if filter.Result != "" && !IsValidResult(filter.Result) {
		return filter, apperr.BadRequest("非法的维修结果: %s", filter.Result)
	}

	if value := strings.TrimSpace(query.StartDate); value != "" {
		from, err := parseDay(value)
		if err != nil {
			return filter, err
		}
		filter.StartedFrom = &from
	}
	if value := strings.TrimSpace(query.EndDate); value != "" {
		to, err := parseDay(value)
		if err != nil {
			return filter, err
		}
		to = to.AddDate(0, 0, 1)
		filter.StartedTo = &to
	}
	if filter.StartedFrom != nil && filter.StartedTo != nil && filter.StartedTo.Before(*filter.StartedFrom) {
		return filter, apperr.BadRequest("结束日期不能早于开始日期")
	}
	return filter, nil
}

// parseTime 解析时间字符串, 为空时返回 fallback。
func parseTime(value string, fallback time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02"} {
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, apperr.BadRequest("时间格式不正确, 建议使用 YYYY-MM-DD HH:mm:ss: %s", value)
}

// parseDay 解析 YYYY-MM-DD 日期, 返回当天零点。
func parseDay(value string) (time.Time, error) {
	date, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(value), time.Local)
	if err != nil {
		return time.Time{}, apperr.BadRequest("日期格式应为 YYYY-MM-DD, 当前值: %s", value)
	}
	return date, nil
}

func valueOrZero(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}
