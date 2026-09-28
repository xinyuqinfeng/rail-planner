//go:build !windows

// Package console 统一处理控制台输出编码（非 Windows 平台为空实现）。
package console

// Init 非 Windows 平台无需处理。
func Init() {}
