//go:build windows

package main

// applyUmask Windows 用 ACL 控制访问，没有 umask，这里保持空实现。
func applyUmask() {}
