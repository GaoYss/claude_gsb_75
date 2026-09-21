package fault

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"streetlight/internal/apperr"
	"streetlight/pkg/pagination"
)

// Filter 是仓储层使用的故障查询条件, 日期已在服务层解析为时间。
type Filter struct {
	Keyword      string
	Status       string
	FaultType    string
	FaultLevel   string
	Source       string
	LampID       uint
	RoadName     string
	ReportedFrom *time.Time
	ReportedTo   *time.Time
	OnlyOpen     bool
}

// Repository 负责故障登记的数据访问。
type Repository struct {
	db *gorm.DB
}

// NewRepository 构造故障登记仓储。
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) session(ctx context.Context) *gorm.DB {
	return r.db.WithContext(ctx)
}

// Create 新增故障记录。
func (r *Repository) Create(ctx context.Context, entity *Fault) error {
	if err := r.session(ctx).Create(entity).Error; err != nil {
		return fmt.Errorf("登记故障失败: %w", err)
	}
	return nil
}

// CreateWithUniqueNo 生成唯一故障单号并落库, 单号冲突时自动重试。
// "同一路灯仅允许一条未闭环故障"由部分唯一索引兜底, 命中时直接返回 409, 不参与单号重试。
func (r *Repository) CreateWithUniqueNo(ctx context.Context, entity *Fault, prefix string) error {
	for attempt := 0; attempt < 5; attempt++ {
		sequence, err := r.NextSequence(ctx, prefix)
		if err != nil {
			return err
		}
		entity.FaultNo = fmt.Sprintf("%s%04d", prefix, sequence+attempt)
		err = r.Create(ctx, entity)
		if err == nil {
			return nil
		}
		if isOpenFaultConflict(err) {
			return apperr.Conflict("该路灯已存在未闭环故障, 请先处理后再登记")
		}
		if !isFaultNoConflict(err) {
			return err
		}
	}
	return apperr.Conflict("故障单号生成冲突, 请稍后重试")
}

// NextSequence 返回指定前缀下可用的下一个流水号。
func (r *Repository) NextSequence(ctx context.Context, prefix string) (int, error) {
	var latest string
	err := r.session(ctx).Model(&Fault{}).
		Where("fault_no LIKE ?", prefix+"%").
		Order("fault_no DESC").
		Limit(1).
		Pluck("fault_no", &latest).Error
	if err != nil {
		return 0, fmt.Errorf("生成故障单号失败: %w", err)
	}
	if latest == "" {
		return 1, nil
	}
	value, convErr := strconv.Atoi(strings.TrimPrefix(latest, prefix))
	if convErr != nil {
		return 1, nil
	}
	return value + 1, nil
}

// Update 保存故障全部字段。
func (r *Repository) Update(ctx context.Context, entity *Fault) error {
	if err := r.session(ctx).Save(entity).Error; err != nil {
		return fmt.Errorf("更新故障失败: %w", err)
	}
	return nil
}

// UpdateColumns 局部更新故障字段。
func (r *Repository) UpdateColumns(ctx context.Context, id uint, columns map[string]any) error {
	if len(columns) == 0 {
		return nil
	}
	result := r.session(ctx).Model(&Fault{}).Where("id = ?", id).Updates(columns)
	if result.Error != nil {
		return fmt.Errorf("更新故障失败: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return apperr.NotFound("故障记录不存在: id=%d", id)
	}
	return nil
}

// CloseIfOpen 原子地将未关闭的故障置为已关闭, 返回受影响行数。
// 返回 0 表示故障不存在或已被并发关闭, 由服务层转换为 409。
func (r *Repository) CloseIfOpen(ctx context.Context, id uint, closedAt time.Time, remark string) (int64, error) {
	result := r.session(ctx).Model(&Fault{}).
		Where("id = ? AND status <> ?", id, StatusClosed).
		Updates(map[string]any{
			"status":       StatusClosed,
			"closed_at":    closedAt,
			"close_remark": remark,
		})
	if result.Error != nil {
		return 0, fmt.Errorf("关闭故障失败: %w", result.Error)
	}
	return result.RowsAffected, nil
}

// StartRepairIfAllowed 原子地将故障推进为维修中并累加维修次数, 返回受影响行数。
// 允许的来源状态与 canTransitTo(…, processing) 一致: 待处理 / 维修中 / 已修复(返修)。
func (r *Repository) StartRepairIfAllowed(ctx context.Context, id uint, repairID uint) (int64, error) {
	result := r.session(ctx).Model(&Fault{}).
		Where("id = ? AND status IN ?", id, []string{StatusPending, StatusProcessing, StatusRepaired}).
		Updates(map[string]any{
			"status":           StatusProcessing,
			"repair_count":     gorm.Expr("repair_count + 1"),
			"latest_repair_id": repairID,
		})
	if result.Error != nil {
		return 0, fmt.Errorf("联动故障开工失败: %w", result.Error)
	}
	return result.RowsAffected, nil
}

// MarkRepairedIfProcessing 原子地将维修中的故障推进为已修复, 返回受影响行数。
func (r *Repository) MarkRepairedIfProcessing(ctx context.Context, id uint) (int64, error) {
	result := r.session(ctx).Model(&Fault{}).
		Where("id = ? AND status = ?", id, StatusProcessing).
		Updates(map[string]any{"status": StatusRepaired})
	if result.Error != nil {
		return 0, fmt.Errorf("联动故障修复失败: %w", result.Error)
	}
	return result.RowsAffected, nil
}

// Delete 按主键删除故障。
func (r *Repository) Delete(ctx context.Context, id uint) error {
	if err := r.session(ctx).Delete(&Fault{}, id).Error; err != nil {
		return fmt.Errorf("删除故障失败: %w", err)
	}
	return nil
}

// GetByID 按主键查询故障。
func (r *Repository) GetByID(ctx context.Context, id uint) (*Fault, error) {
	var entity Fault
	err := r.session(ctx).First(&entity, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, apperr.NotFound("故障记录不存在: id=%d", id)
	}
	if err != nil {
		return nil, fmt.Errorf("查询故障失败: %w", err)
	}
	return &entity, nil
}

// GetByNo 按故障单号查询。
func (r *Repository) GetByNo(ctx context.Context, faultNo string) (*Fault, error) {
	var entity Fault
	err := r.session(ctx).Where("fault_no = ?", strings.TrimSpace(faultNo)).First(&entity).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, apperr.NotFound("故障记录不存在: fault_no=%s", faultNo)
	}
	if err != nil {
		return nil, fmt.Errorf("查询故障失败: %w", err)
	}
	return &entity, nil
}

// List 分页查询故障记录。
func (r *Repository) List(ctx context.Context, filter Filter, page pagination.Query) ([]Fault, int64, error) {
	base := func() *gorm.DB {
		return applyFilter(r.session(ctx).Model(&Fault{}), filter)
	}

	var total int64
	if err := base().Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("统计故障总数失败: %w", err)
	}

	entities := make([]Fault, 0)
	if err := base().Order(page.OrderClause()).Offset(page.Offset()).Limit(page.Limit()).Find(&entities).Error; err != nil {
		return nil, 0, fmt.Errorf("查询故障列表失败: %w", err)
	}
	return entities, total, nil
}

// ListByLamp 查询某盏路灯的全部故障, 按上报时间倒序。
func (r *Repository) ListByLamp(ctx context.Context, lampID uint) ([]Fault, error) {
	entities := make([]Fault, 0)
	err := r.session(ctx).Where("lamp_id = ?", lampID).Order("reported_at DESC, id DESC").Find(&entities).Error
	if err != nil {
		return nil, fmt.Errorf("查询路灯故障失败: %w", err)
	}
	return entities, nil
}

// GetOpenByLamp 查询某盏路灯当前未闭环的故障, 不存在时返回 nil。
func (r *Repository) GetOpenByLamp(ctx context.Context, lampID uint) (*Fault, error) {
	var entity Fault
	err := r.session(ctx).
		Where("lamp_id = ? AND status IN ?", lampID, []string{StatusPending, StatusProcessing}).
		Order("reported_at DESC, id DESC").
		First(&entity).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询路灯未闭环故障失败: %w", err)
	}
	return &entity, nil
}

// CountOpenByLamp 统计某盏路灯未闭环故障数量, 实现路灯模块的 OpenFaultCounter 端口。
func (r *Repository) CountOpenByLamp(ctx context.Context, lampID uint) (int64, error) {
	var count int64
	err := r.session(ctx).Model(&Fault{}).
		Where("lamp_id = ? AND status IN ?", lampID, []string{StatusPending, StatusProcessing}).
		Count(&count).Error
	if err != nil {
		return 0, fmt.Errorf("统计路灯未闭环故障失败: %w", err)
	}
	return count, nil
}

// StatusCountsForLamp 统计某盏路灯各状态的故障数量, 用于推算运行状态。
func (r *Repository) StatusCountsForLamp(ctx context.Context, lampID uint) (map[string]int64, error) {
	type row struct {
		Label string
		Total int64
	}
	rows := make([]row, 0)
	err := r.session(ctx).Model(&Fault{}).
		Select("status AS label, COUNT(*) AS total").
		Where("lamp_id = ?", lampID).
		Group("status").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("统计路灯故障状态失败: %w", err)
	}

	result := make(map[string]int64, len(rows))
	for _, item := range rows {
		result[item.Label] = item.Total
	}
	return result, nil
}

// applyFilter 统一拼装故障列表查询条件。
func applyFilter(statement *gorm.DB, filter Filter) *gorm.DB {
	if keyword := strings.TrimSpace(filter.Keyword); keyword != "" {
		like := "%" + keyword + "%"
		statement = statement.Where(
			"fault_no LIKE ? OR lamp_code LIKE ? OR road_name LIKE ? OR description LIKE ?",
			like, like, like, like,
		)
	}
	if filter.Status != "" {
		statement = statement.Where("status = ?", filter.Status)
	}
	if filter.FaultType != "" {
		statement = statement.Where("fault_type = ?", filter.FaultType)
	}
	if filter.FaultLevel != "" {
		statement = statement.Where("fault_level = ?", filter.FaultLevel)
	}
	if filter.Source != "" {
		statement = statement.Where("source = ?", filter.Source)
	}
	if filter.LampID > 0 {
		statement = statement.Where("lamp_id = ?", filter.LampID)
	}
	if value := strings.TrimSpace(filter.RoadName); value != "" {
		statement = statement.Where("road_name = ?", value)
	}
	if filter.ReportedFrom != nil {
		statement = statement.Where("reported_at >= ?", *filter.ReportedFrom)
	}
	if filter.ReportedTo != nil {
		statement = statement.Where("reported_at < ?", *filter.ReportedTo)
	}
	if filter.OnlyOpen {
		statement = statement.Where("status IN ?", []string{StatusPending, StatusProcessing})
	}
	return statement
}

// isFaultNoConflict 判断唯一约束冲突是否来自故障单号, 此类冲突可通过换号重试解决。
// sqlite 报告 "UNIQUE constraint failed: fault.fault_no", postgres 报告索引名 "idx_fault_fault_no"。
func isFaultNoConflict(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "fault_no")
}

// isOpenFaultConflict 判断唯一约束冲突是否来自"同一路灯仅一条未闭环故障"的部分索引。
// sqlite 报告 "UNIQUE constraint failed: fault.lamp_id", postgres 报告索引名。
func isOpenFaultConflict(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "idx_fault_open_per_lamp") {
		return true
	}
	return strings.Contains(message, "unique") && strings.Contains(message, "fault.lamp_id")
}
