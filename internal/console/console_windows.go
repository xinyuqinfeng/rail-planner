//go:build windows

// Package console 统一处理控制台输出编码。
package console

import "syscall"

var procSetConsoleOutputCP = syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleOutputCP")

// Init 将控制台输出代码页切到 UTF-8(65001)，避免中文在 cmd/PowerShell 下乱码。
func Init() {
	_, _, _ = procSetConsoleOutputCP.Call(uintptr(65001))
}
