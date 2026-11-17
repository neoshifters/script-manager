package main

import (
	"io/fs"
	"os"
	"path/filepath"
)

// 权限约定：目录 0775、文件 0664（属主与同组可读写，其他人只读）。
//
// 容器里以 root 运行、且部分环境把 umask 设成 0777 时，MkdirAll / CreateTemp 传的 perm
// 会被 umask 再裁一次，结果目录变成 0000、文件变成 000。宿主机通过 SMB 映射盘看，
// 目录就是「空的」——数据其实都在，只是没权限列出来。
// 因此这里的做法是：建完统一 Chmod 一次（chmod 不受 umask 影响），并把 umask 显式设成 0002。
const (
	dirPerm  os.FileMode = 0o775
	filePerm os.FileMode = 0o664
)

// ensureDir 建目录（含父目录）并把最终权限固定为 0775。
func ensureDir(path string) error {
	if err := os.MkdirAll(path, dirPerm); err != nil {
		return err
	}
	return os.Chmod(path, dirPerm)
}

// chmodFile 把刚生成的文件权限固定为 0664。
func chmodFile(path string) error {
	return os.Chmod(path, filePerm)
}

// fixZeroPerms 修掉历史上被建成 0 权限的条目：只处理权限为 0 的目录/文件，改成 0775 / 0664，
// 不动正常权限的条目，也不改属主、不递归 chown 别人的文件。返回修复的目录数与文件数。
func fixZeroPerms(root string) (dirs, files int) {
	if info, err := os.Stat(root); err == nil && info.IsDir() && info.Mode().Perm() == 0 {
		if os.Chmod(root, dirPerm) == nil {
			dirs++
		}
	}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0 {
			return nil
		}
		want := filePerm
		if d.IsDir() {
			want = dirPerm
		}
		if os.Chmod(path, want) == nil {
			if d.IsDir() {
				dirs++
			} else {
				files++
			}
		}
		return nil
	})
	return dirs, files
}
