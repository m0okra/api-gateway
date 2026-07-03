package main

import (
	"encoding/json"
	"log"
	"os"
)

// loadAliases 从指定路径加载模型别名配置。
//
// 文件不存在时视为禁用别名功能（aliases 保持 nil），返回 nil。
// 文件存在但解析失败时记录错误日志并禁用别名功能（aliases 置 nil），返回 nil，
// 不阻断启动——别名只是请求转发的辅助映射，不应让网关整体不可用。
//
// 加载完成后 aliases 为非 nil map（即使为空 {} 也视为已启用）。
// 启动后只读，无锁；请求路径以 `aliases != nil` 判定是否启用。
func loadAliases(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// 文件不存在：禁用别名功能（默认行为）
		return nil
	} else if err != nil {
		// 其他 stat 错误（如权限不足）：记录但不阻断
		log.Printf("[ALIAS] stat %s failed: %v (aliases disabled)", path, err)
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("[ALIAS] read %s failed: %v (aliases disabled)", path, err)
		return nil
	}

	m := make(map[string]string)
	if err := json.Unmarshal(data, &m); err != nil {
		// 解析失败：记录但不阻断启动，别名功能保持禁用
		log.Printf("[ALIAS] parse %s failed: %v (aliases disabled, no substitution will occur)", path, err)
		return nil
	}

	aliases = m
	return nil
}
