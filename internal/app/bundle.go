package app

import (
	"archive/zip"
	"crypto/sha256"
	"errors"
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

// unpackZip 把发送端合成的 bundle zip 解包到其所在目录（即本次接收的专属子目录），
// 解包成功后删除 zip 本体，返回解包出的顶层条目（文件或目录）路径列表。
// 仅用于发送端因多文件/目录自动打包的 zip；用户亲手发送的 zip 不经此路径。
func unpackZip(zipPath string) ([]string, error) {
	dir := filepath.Dir(zipPath)
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	// Windows 上打开中的文件无法删除：解包完成后先显式关闭再删 zip
	extract := func() error {
		for _, f := range zr.File {
			if err := extractZipEntry(dir, f); err != nil {
				return err
			}
		}
		return nil
	}()
	zr.Close()
	if extract != nil {
		return nil, extract
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var tops []string
	for _, e := range entries {
		if e.IsDir() {
			tops = append(tops, filepath.Join(dir, e.Name()))
			continue
		}
		// zip 本体尚未删除，从顶层条目中排除
		if e.Name() == filepath.Base(zipPath) {
			continue
		}
		tops = append(tops, filepath.Join(dir, e.Name()))
	}
	// 先确认解出了内容、再删 zip：保证"解包失败保留原 zip"
	if len(tops) == 0 {
		return nil, errors.New("zip 内没有可解出的内容")
	}
	if err := os.Remove(zipPath); err != nil {
		return nil, err
	}
	return tops, nil
}

// extractZipEntry 安全解出单个 zip 条目：逐段清理路径（防 zip-slip 与非法字符）。
func extractZipEntry(dir string, f *zip.File) error {
	name := f.Name
	if strings.HasPrefix(name, "/") || strings.Contains(name, ":") {
		return fmt.Errorf("zip 内存在非法路径 %q", name)
	}
	segs := strings.Split(filepath.ToSlash(name), "/")
	var clean []string
	for _, s := range segs {
		if s == "" || s == "." {
			continue
		}
		// 先清理（Windows 上仅尾部 . / 空格非法）再判断：
		// ".. "、". ." 这类伪装段会被剥光或还原成 ".."，统一在此拒绝
		s = sanitizeSegment(s)
		if s == "" || s == ".." {
			return fmt.Errorf("zip 内存在非法路径 %q", name)
		}
		clean = append(clean, s)
	}
	if len(clean) == 0 {
		return nil
	}
	target := filepath.Join(append([]string{dir}, clean...)...)
	if strings.HasSuffix(name, "/") {
		return os.MkdirAll(target, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(target)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// sanitizeSegment 清理 zip 条目单段路径中的 Windows 非法字符。
// 仅剥尾部 "."/" "（Windows 禁止，前导点合法，保留 .gitignore 类文件名）。
func sanitizeSegment(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"|?*`, r) {
			return '_'
		}
		return r
	}, strings.TrimRight(s, ". "))
}
