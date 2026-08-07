package builtin

import "aiops/internal/provider"

// Registry 内置 provider 的包级注册表。
// 各 provider 在 init() 中调用 Registry.Register 完成自注册；
// main 通过 import _ "aiops/internal/provider/builtin" 触发全部注册。
var Registry = provider.NewRegistry()
