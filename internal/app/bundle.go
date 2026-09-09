package app

import (
	"archive/zip"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// bundleZip 把多个文件/目录打包成一个临时 zip。
// 返回 zip 路径、对外文件名、清理函数与错误。调用方在发送完成后必须调用 cleanup。
func bundleZip(paths []string) (zipPath, sendName string, cleanup func(), err error) {
	tmp, err := os.CreateTemp("", "copywhere-bundle-*.zip")
	if err != nil {
		return "", "", nil, err
	}
	tmpName := tmp.Name()
	cleanup = func() { os.Remove(tmpName) }

	h := sha256.New()
	zw := zip.NewWriter(io.MultiWriter(tmp, h))
	used := make(map[string]bool)
	unique := func(n string) string {
		if !used[n] {
			used[n] = true
			return n
		}
		base, ext := n, ""
		if e := filepath.Ext(n); e != "" {
			base = strings.TrimSuffix(n, e)
			ext = e
		}
		for i := 1; ; i++ {
			cand := fmt.Sprintf("%s (%d)%s", base, i, ext)
			if !used[cand] {
				used[cand] = true
				return cand
			}
		}
	}
	addFile := func(name, fpath string, info os.FileInfo) error {
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = name
		hdr.Method = zip.Deflate
		hdr.Modified = info.ModTime()
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		f, err := os.Open(fpath)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	}

	var walkErr error
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			walkErr = err
			break
		}
		if !info.IsDir() {
			if err := addFile(unique(filepath.Base(p)), p, info); err != nil {
				walkErr = err
				break
			}
			continue
		}
		root := filepath.Base(filepath.Clean(p))
		we := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(p, path)
			if err != nil {
				return err
			}
			name := filepath.ToSlash(filepath.Join(root, rel))
			if d.IsDir() {
				if rel == "." {
					return nil
				}
				hdr := &zip.FileHeader{Name: unique(name) + "/", Method: zip.Deflate}
				if _, err := zw.CreateHeader(hdr); err != nil {
					return err
				}
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			return addFile(unique(name), path, info)
		})
		if we != nil {
			walkErr = we
			break
		}
	}
	if walkErr != nil {
		zw.Close()
		tmp.Close()
		cleanup()
		return "", "", nil, walkErr
	}
	if err := zw.Close(); err != nil {
		tmp.Close()
		cleanup()
		return "", "", nil, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", "", nil, err
	}
	fi, err := os.Stat(tmpName)
	if err != nil {
		cleanup()
		return "", "", nil, err
	}
	_ = h // 摘要由调用方 hashFile 重新计算
	_ = fi
	sendName = fmt.Sprintf("copywhere-bundle-%s.zip", time.Now().Format("0102-150405"))
	return tmpName, sendName, cleanup, nil
}
