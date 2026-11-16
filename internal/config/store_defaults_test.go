package config

import "testing"

// TestStoreBuildDefaults 验证 MSIX 商店构建（storeBuild=1）只影响首次生成的
// 默认配置，GitHub Release 版保持原有缺省行为。
func TestStoreBuildDefaults(t *testing.T) {
	old := storeBuild
	defer func() { storeBuild = old }()

	storeBuild = "0"
	plain, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if !plain.KVMOn() || !plain.ShellMenuOn() {
		t.Fatal("普通构建的缺省配置应开启 KVM 与右键菜单")
	}

	storeBuild = "1"
	store, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if store.KVMOn() || store.ShellMenuOn() {
		t.Fatal("商店构建的缺省配置应关闭 KVM 与右键菜单")
	}
}
