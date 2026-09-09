package discovery

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"
)

func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func TestDiscoveryReceivesAnnounce(t *testing.T) {
	port := freeUDPPort(t)
	store := NewStore("self-id")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, port, 12345, time.Second, "self", "self-id", store) }()
	time.Sleep(100 * time.Millisecond) // 等监听就绪

	// 模拟另一个节点的公告
	msg, _ := json.Marshal(announceMsg{Magic: announceMagic, ID: "peer-1", Name: "PEER-PC", TCPPort: 47831})
	c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	c.Write(msg)
	c.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		peers := store.Alive(5 * time.Second)
		if len(peers) == 1 && peers[0].ID == "peer-1" && peers[0].Name == "PEER-PC" && peers[0].Primary() == "127.0.0.1" {
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("Run 未随 ctx 退出")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("3 秒内未发现模拟节点")
}

func TestSelfAnnounceIgnored(t *testing.T) {
	port := freeUDPPort(t)
	store := NewStore("self-id")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Run(ctx, port, 12345, time.Second, "self", "self-id", store) }()
	time.Sleep(100 * time.Millisecond)

	msg, _ := json.Marshal(announceMsg{Magic: announceMagic, ID: "self-id", Name: "SELF", TCPPort: 1})
	c, _ := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	c.Write(msg)
	c.Close()
	time.Sleep(300 * time.Millisecond)

	if peers := store.Alive(5 * time.Second); len(peers) != 0 {
		t.Fatalf("自己的公告应被过滤, got %v", peers)
	}
}

func TestMultiIPDedupAndPreference(t *testing.T) {
	store := NewStore("self")
	_, lanNet, _ := net.ParseCIDR("192.168.0.0/24")
	store.SetLocalSubnets([]*net.IPNet{lanNet})

	// 同一节点从 ZeroTier 和局域网两个来源广播，应合并为一个节点，
	// 全部地址保留且同网段地址排最前
	store.Upsert("peer-1", "PC", "10.147.17.171", 47831, "s1")
	store.Upsert("peer-1", "PC", "192.168.0.55", 47831, "s1")
	store.Upsert("peer-1", "PC", "10.147.17.171", 47831, "s1") // 重复地址再刷新

	peers := store.Alive(time.Minute)
	if len(peers) != 1 {
		t.Fatalf("多来源应合并为一个节点, got %d", len(peers))
	}
	p := peers[0]
	if len(p.IPs) != 2 {
		t.Fatalf("应保留全部 2 个地址, got %v", p.IPs)
	}
	if p.Primary() != "192.168.0.55" {
		t.Fatalf("优选地址应为同网段的 192.168.0.55, got %v", p.IPs)
	}
	if p.IPs[1] != "10.147.17.171" {
		t.Fatalf("备选地址应为 10.147.17.171, got %v", p.IPs)
	}
}

// 同一指纹的节点会话变化（重启）时应触发回调。
func TestRestartCallbackFires(t *testing.T) {
	store := NewStore("self")
	fired := make(chan string, 1)
	store.SetRestartCallback(func(id, name string) { fired <- id })

	store.Upsert("peer-1", "PC", "10.0.0.1", 47831, "s1")
	store.Upsert("peer-1", "PC", "10.0.0.1", 47831, "s1") // 同会话，不触发
	select {
	case id := <-fired:
		t.Fatalf("同会话不应触发重启回调, got %s", id)
	default:
	}
	store.Upsert("peer-1", "PC", "10.0.0.1", 47831, "s2") // 会话变化 → 触发
	select {
	case id := <-fired:
		if id != "peer-1" {
			t.Fatalf("回调 ID 不符: %s", id)
		}
	case <-time.After(time.Second):
		t.Fatal("会话变化未触发重启回调")
	}
}

func TestNoLocalSubnetFallsBackToRecent(t *testing.T) {
	store := NewStore("self") // 未设置本机网段
	store.Upsert("peer-1", "PC", "10.0.0.1", 47831, "s1")
	time.Sleep(5 * time.Millisecond)
	store.Upsert("peer-1", "PC", "10.0.0.2", 47831, "s1")

	peers := store.Alive(time.Minute)
	if len(peers) != 1 || len(peers[0].IPs) != 2 || peers[0].Primary() != "10.0.0.2" {
		t.Fatalf("无同网段候选时首选应为最近活跃地址且保留全部, got %+v", peers)
	}
}

func TestBadMagicIgnored(t *testing.T) {
	port := freeUDPPort(t)
	store := NewStore("self-id")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Run(ctx, port, 12345, time.Second, "self", "self-id", store) }()
	time.Sleep(100 * time.Millisecond)

	msg, _ := json.Marshal(announceMsg{Magic: "evil", ID: "peer-x", Name: "X", TCPPort: 1})
	c, _ := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	c.Write(msg)
	c.Close()
	time.Sleep(300 * time.Millisecond)

	if peers := store.Alive(5 * time.Second); len(peers) != 0 {
		t.Fatalf("非法 magic 应被忽略, got %v", peers)
	}
}
