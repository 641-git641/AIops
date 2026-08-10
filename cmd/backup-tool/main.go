// Command backup-tool 数据恢复辅助工具：解密加密备份镜像（KEEPON 4.1）。
// 恢复演练与真实恢复见 scripts/restore.sh（本工具是其解密子步骤，也可单独使用）。
//
// 用法：
//
//	backup-tool decrypt <input.aes> <output.dump> --passphrase-file <file>
package main

import (
	"fmt"
	"os"
	"strings"

	"aiops/internal/backup"
)

const usageText = `用法: backup-tool decrypt <input.aes> <output.dump> --passphrase-file <file>
  --passphrase-file <file>  口令文件路径（加密镜像解密必填）`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usageText)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "decrypt":
	case "-h", "--help", "help":
		fmt.Println(usageText)
		return
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n%s\n", os.Args[1], usageText)
		os.Exit(2)
	}
	// 手动解析（Go flag 包遇首个非 flag 参数即停止，这里让 flag 位置无关）
	rest := os.Args[2:]
	var args []string
	passFile := ""
	for i := 0; i < len(rest); i++ {
		switch {
		case rest[i] == "--passphrase-file" && i+1 < len(rest):
			passFile = rest[i+1]
			i++
		case strings.HasPrefix(rest[i], "--passphrase-file="):
			passFile = strings.TrimPrefix(rest[i], "--passphrase-file=")
		case rest[i] == "-h" || rest[i] == "--help":
			fmt.Println(usageText)
			return
		default:
			args = append(args, rest[i])
		}
	}
	if len(args) != 2 || passFile == "" {
		fmt.Fprintln(os.Stderr, usageText)
		os.Exit(2)
	}
	pass, err := os.ReadFile(passFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读口令文件失败: %v\n", err)
		os.Exit(1)
	}
	if err := backup.DecryptFile(args[0], args[1], pass); err != nil {
		fmt.Fprintf(os.Stderr, "解密失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("解密完成: %s → %s（可执行恢复/校验）\n", args[0], args[1])
}
