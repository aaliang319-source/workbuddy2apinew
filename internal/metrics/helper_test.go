// 持久化测试用的最小写文件助手（避免仅为测试引入 os直接依赖的重复代码）。
package metrics

import "os"

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
