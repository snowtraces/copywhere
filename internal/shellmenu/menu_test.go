package shellmenu

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

// useTempRoot 把注册表根切到一个临时分支，避免测试污染真实的右键菜单。
func useTempRoot(t *testing.T) string {
	t.Helper()
	root := fmt.Sprintf(`%s\copywhere-test-%d-%d`, classesRoot, os.Getpid(), time.Now().UnixNano())
	old := classesRootPath
	classesRootPath = root
	t.Cleanup(func() {
		classesRootPath = old
		removeTempRoot(root)
	})
	return root
}

// removeTempRoot 自底向上删除临时分支（DeleteKey 不能删仍有子键的键）。
func removeTempRoot(root string) {
	for _, target := range targets {
		registry.DeleteKey(registry.CURRENT_USER, root+`\`+target+`\shell\`+verbName+`\command`)
		registry.DeleteKey(registry.CURRENT_USER, root+`\`+target+`\shell\`+verbName)
		registry.DeleteKey(registry.CURRENT_USER, root+`\`+target+`\shell`)
		registry.DeleteKey(registry.CURRENT_USER, root+`\`+target)
	}
	registry.DeleteKey(registry.CURRENT_USER, root)
}

func TestInstallAndUninstall(t *testing.T) {
	useTempRoot(t)
	exe := filepath.Join(t.TempDir(), "copywhere.exe")

	if Installed("") {
		t.Fatal("初始状态不应已注册")
	}
	if err := Install(exe); err != nil {
		t.Fatalf("Install 失败: %v", err)
	}
	if !Installed("") {
		t.Fatal("Install 后 Installed(\"\") 应为 true")
	}
	if !Installed(exe) {
		t.Fatal("Install 后 Installed(exe) 应为 true")
	}
	// 程序被移动/升级后：菜单指向旧 exe，必须判为需要重新注册
	if Installed(filepath.Join(t.TempDir(), "moved.exe")) {
		t.Fatal("指向其他 exe 时不应报告为已注册")
	}

	// 校验菜单项内容：显示文字 + 命令行
	k, err := registry.OpenKey(registry.CURRENT_USER, verbKeyPath("*"), registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	text, _, err := k.GetStringValue("MUIVerb")
	k.Close()
	if err != nil || text != MenuText {
		t.Fatalf("MUIVerb 应为 %q，实际 %q（err=%v）", MenuText, text, err)
	}
	ck, err := registry.OpenKey(registry.CURRENT_USER, verbKeyPath("*")+`\command`, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	cmd, _, err := ck.GetStringValue("")
	ck.Close()
	if err != nil || cmd != Command(exe) {
		t.Fatalf("command 应为 %q，实际 %q（err=%v）", Command(exe), cmd, err)
	}

	// 幂等：重复注册仍然可用
	if err := Install(exe); err != nil {
		t.Fatalf("重复 Install 应成功: %v", err)
	}

	if err := Uninstall(); err != nil {
		t.Fatalf("Uninstall 失败: %v", err)
	}
	if Installed("") {
		t.Fatal("Uninstall 后不应再注册")
	}
	if err := Uninstall(); err != nil {
		t.Fatalf("重复 Uninstall 应成功: %v", err)
	}
}

func TestInstallRejectsEmptyExe(t *testing.T) {
	useTempRoot(t)
	if err := Install(""); err == nil {
		t.Fatal("空 exe 路径应报错")
	}
}

func TestCommandQuoting(t *testing.T) {
	exe := `C:\Program Files\copywhere\copywhere.exe`
	want := `"C:\Program Files\copywhere\copywhere.exe" shell-send "%1"`
	if got := Command(exe); got != want {
		t.Fatalf("命令行应为 %q，实际 %q", want, got)
	}
}
