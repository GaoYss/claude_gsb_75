package database

import (
	"context"

	"gorm.io/gorm"
)

// txKey 是事务句柄在 context 中的键。
type txKey struct{}

// WithTx 把事务句柄写入 context, 使各模块仓储在同一事务内协作。
func WithTx(ctx context.Context, tx *gorm.DB) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// TxFromContext 取出 context 中的事务句柄。
func TxFromContext(ctx context.Context) (*gorm.DB, bool) {
	tx, ok := ctx.Value(txKey{}).(*gorm.DB)
	return tx, ok
}
