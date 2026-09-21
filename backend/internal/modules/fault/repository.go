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
	"streetlight/internal/database"
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
	if tx, ok := database.TxFromContext(ctx); ok {
		return tx.WithContext(ctx)
	}
	return r.db.WithContext(ctx)
}

// InTransaction 在事务中执行 fn; 若 ctx 已携带事务则直接复用, 支持嵌套调用。
func (r *Repository) InTransaction(ctx context.Context, fn func(ctx context.Context) error) error {
	if tx, ok := database.TxFromContext(ctx); ok {
		_ = tx
		return fn(ctx)
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return fn(database.WithTx(ctx, tx))
	})
}

// Create 新增故障记录。
func (r *Repository) Create(ctx context.Context, entity *Fault) error {
	if err := r.session(ctx).Create(entity).Error; err != nil {
		return fmt.Errorf("登记故障失败: %w", err)
	}
	return nil
}

// CreateWithUniqueNo 生成唯一故障单号并落库, 单号冲突时自动重试。
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
		if isOpenFaultDuplicate(err) {
			// 命中 "同一路灯仅一条未闭环故障" 的部分唯一索引, 属于业务冲突, 不再重试单号。
			return apperr.Conflict("路灯 %s 已存在未闭环故障, 请先闭环后再登记", entity.LampCode)
		}
		if !isUniqueViolation(err) {
			return err
		}
	}
	return apperr.Conflict("故障单号生成冲突, 请稍后重试")
}

// isOpenFaultDuplicate 判断唯一约束冲突是否来自 idx_fault_lamp_open(同灯未闭环唯一)。
// sqlite 报错携带列名(lamp_id), postgres 报错携带索引名, 两者都需要兼容。
func isOpenFaultDuplicate(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "idx_fault_lamp_open") ||
		(strings.Contains(message, "unique") && strings.Contains(message, "lamp_id"))
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

// TransitionStatus 在 WHERE 中带当前状态守卫地更新故障, 返回是否真的更新了一行。
// 用于状态流转类操作, 保证并发/重复提交时只有一个请求能成功, 其余拿到 false。
func (r *Repository) TransitionStatus(ctx context.Context, id uint, expectStatuses []string, columns map[string]any) (bool, error) {
	if len(columns) == 0 {
		return false, nil
	}
	result := r.session(ctx).Model(&Fault{}).
		Where("id = ? AND status IN ?", id, expectStatuses).
		Updates(columns)
	if result.Error != nil {
		return false, fmt.Errorf("更新故障状态失败: %w", result.Error)
	}
	return result.RowsAffected > 0, nil
}

// UpdateFieldsUnlessStatus 带状态排除守卫地更新字段, 用于编辑类操作:
// 故障在读取后被关闭时, 更新命中 0 行并返回 false, 避免旧数据覆盖终态。
func (r *Repository) UpdateFieldsUnlessStatus(ctx context.Context, id uint, forbiddenStatus string, columns map[string]any) (bool, error) {
	result := r.session(ctx).Model(&Fault{}).
		Where("id = ? AND status <> ?", id, forbiddenStatus).
		Updates(columns)
	if result.Error != nil {
		return false, fmt.Errorf("更新故障失败: %w", result.Error)
	}
	return result.RowsAffected > 0, nil
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

// isUniqueViolation 兼容 sqlite 与 postgres 的唯一约束冲突判断。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint failed") ||
		strings.Contains(message, "duplicate key") ||
		strings.Contains(message, "unique violation")
}
