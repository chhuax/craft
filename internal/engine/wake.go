package engine

import (
	"encoding/json/jsontext"
	"fmt"
)

// wake 只登记投递请求，供 Intake 和 Works 在保存 Work 的同一写事务内调用。
// 调用方传入本事务已读且已更新的聚合，避免重读；入队失败必须传回以撤销整条命令。
func wake(u Unit, w *Work) error {
	if len(w.Undelivered) == 0 || w.Delivery != nil || w.Mandate.Basis.Via == ViaHumanDirect {
		return nil
	}
	payload, err := jsontext.AppendQuote([]byte(`{"workId":`), w.ID)
	if err != nil {
		return fmt.Errorf("编码投递对象: %w", err)
	}
	if err := u.Effects().Enqueue("delivery", w.ID, append(payload, '}')); err != nil {
		return fmt.Errorf("为 Work %q 登记投递: %w", w.ID, err)
	}
	return nil
}
