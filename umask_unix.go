//go:build !windows

package main

import "syscall"

// applyUmask 显式设置 umask 为 0002：新建目录默认 0775、文件默认 0664，组内可读写。
// 有些容器环境 umask 是 0777，不显式设置会把新建目录建成 0000，宿主机上就看不到内容。
func applyUmask() {
	syscall.Umask(0o002)
}
