// Package discovery 实现基于 UDP 广播的局域网节点自动发现。
//
// 每个节点周期性向本机所有网段的定向广播地址（以及 127.0.0.1，便于同机联调）
// 发送公告；同时监听本机发现端口，维护一张带 TTL 的在线节点表。
//
// 节点身份用稳定指纹（Windows MachineGuid 派生，见 app.nodeFingerprint）标识：
// 同一台机器无论从几个网卡/几个网络广播、重启多少次，指纹都不变，
// 因此对端能把它的全部来源地址合并到同一个节点上。
//
// 地址策略：节点的所有已知 IP 全部保留并全部展示（不做提前去重），
// 仅做排序——与本机同网段的直连地址排前面；发送方依次尝试，任一可达即可，
// 避免优选路径失效时与节点失联。长期（staleIPAfter）无公告的地址才剔除。
package discovery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sort"
	"sync"
	"time"
)

const announceMagic = "copywhere-a1"

// staleIPAfter 是节点地址自最近一次公告起的最长保留时间，
// 超过后从候选地址中剔除（防止一直往已经不通的地址上撞）。
const staleIPAfter = 5 * time.Minute

// Peer 是一个已发现节点的快照。
type Peer struct {
	ID       string    `json:"id"`   // 稳定节点指纹
	Name     string    `json:"name"` // 节点显示名
	IPs      []string  `json:"ips"`  // 全部已知地址，优选排序（同网段直连优先）
	TCPPort  int       `json:"tcp_port"`
	LastSeen time.Time `json:"-"`
}

// Primary 返回优选地址（IPs[0]）。
func (p Peer) Primary() string {
	if len(p.IPs) > 0 {
		return p.IPs[0]
	}
	return ""
}

// peerEntry 汇合同一节点的全部来源地址。
type peerEntry struct {
	name     string
	tcpPort  int
	ips      map[string]time.Time
	lastSeen time.Time
	online   bool
	session  string // 对端进程会话（每次启动随机），变化即对端重启
}

// Store 是线程安全的节点表。
type Store struct {
	mu        sync.Mutex
	self      string
	peers     map[string]*peerEntry
	localNets []*net.IPNet
	restartCb func(id, name string) // 节点重启（会话变化）回调
}

// NewStore 创建节点表；selfID（本机指纹）用于过滤自己的公告。
func NewStore(selfID string) *Store {
	return &Store{self: selfID, peers: make(map[string]*peerEntry)}
}

// SetLocalSubnets 设置本机网段，用于优选与本地直连同网段的节点地址。
func (s *Store) SetLocalSubnets(nets []*net.IPNet) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.localNets = nets
}

// SetRestartCallback 注册节点重启回调：同一指纹的节点以新会话出现（即对端
// 进程重启）时触发一次。发送方借此解除对该节点的暂停发送等状态。
func (s *Store) SetRestartCallback(fn func(id, name string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restartCb = fn
}

// Upsert 记录/刷新一个节点的某个来源地址；session 为对端进程会话标识。
func (s *Store) Upsert(id, name, ip string, port int, session string) {
	if id == "" || id == s.self {
		return
	}
	now := time.Now()
	s.mu.Lock()
	e := s.peers[id]
	if e == nil {
		e = &peerEntry{ips: make(map[string]time.Time), online: true}
		s.peers[id] = e
		log.Printf("发现新节点 %q（%s:%d，指纹 %s…）", name, ip, port, shortID(id))
	} else if ip != "" {
		if _, seen := e.ips[ip]; !seen {
			log.Printf("节点 %q 新增地址 %s（现共 %d 个地址）", e.name, ip, len(e.ips)+1)
		}
	}
	if name != "" && e.name != name {
		if e.name != "" {
			log.Printf("节点 %s 更名: %q → %q", shortID(id), e.name, name)
		}
		e.name = name
	}
	// 会话变化 = 同一指纹的节点进程重启
	if session != "" && e.session != "" && e.session != session {
		log.Printf("节点 %q 已重启（会话变化）", e.name)
		if s.restartCb != nil {
			cb := s.restartCb
			s.mu.Unlock() // 回调在锁外执行，避免死锁
			cb(id, e.name)
			s.mu.Lock()
		}
	}
	if session != "" {
		e.session = session
	}
	e.tcpPort = port
	e.lastSeen = now
	if ip != "" {
		e.ips[ip] = now
	}
	s.mu.Unlock()
}

type ipAt struct {
	ip string
	t  time.Time
}

// Alive 返回 TTL 内仍然在线的节点，按名称排序。
// 每个节点的全部地址都保留：同网段直连地址按最近活跃排前，其余地址随后；
// 超过 staleIPAfter 无公告的地址会被剔除并记录日志。
func (s *Store) Alive(ttl time.Duration) []Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var out []Peer
	for id, e := range s.peers {
		fresh := now.Sub(e.lastSeen) <= ttl
		if e.online && !fresh {
			log.Printf("节点 %q 已离线（超过 %s 未收到公告）", e.name, ttl.Round(time.Second))
		}
		if fresh && !e.online {
			log.Printf("节点 %q 重新上线（%s:%d）", e.name, e.Primary(), e.tcpPort)
		}
		e.online = fresh

		// 剔除长期无公告的地址
		for ip, t := range e.ips {
			if now.Sub(t) > staleIPAfter {
				delete(e.ips, ip)
				log.Printf("节点 %q 移除失效地址 %s（超过 %s 无公告）", e.name, ip, staleIPAfter)
			}
		}

		if !fresh || len(e.ips) == 0 {
			continue
		}

		// 排序：同网段直连地址优先，各组内按最近活跃倒序
		var lan, other []ipAt
		for ip, t := range e.ips {
			if s.inLocalSubnet(ip) {
				lan = append(lan, ipAt{ip, t})
			} else {
				other = append(other, ipAt{ip, t})
			}
		}
		byRecent := func(list []ipAt) {
			sort.Slice(list, func(i, j int) bool { return list[i].t.After(list[j].t) })
		}
		byRecent(lan)
		byRecent(other)
		ips := make([]string, 0, len(lan)+len(other))
		for _, x := range lan {
			ips = append(ips, x.ip)
		}
		for _, x := range other {
			ips = append(ips, x.ip)
		}

		out = append(out, Peer{
			ID:       id,
			Name:     e.name,
			IPs:      ips,
			TCPPort:  e.tcpPort,
			LastSeen: e.lastSeen,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *Store) inLocalSubnet(ipStr string) bool {
	ip := net.ParseIP(ipStr).To4()
	if ip == nil {
		return false
	}
	for _, n := range s.localNets {
		if n == nil || len(n.Mask) != 4 {
			continue
		}
		if ip.Mask(n.Mask).Equal(n.IP.To4()) {
			return true
		}
	}
	return false
}

// shortID 返回指纹的前 8 位，用于日志展示。
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Primary 返回节点最近一次使用的地址（旧字段兼容辅助）。
func (e *peerEntry) Primary() string {
	best, bestT := "", time.Time{}
	for ip, t := range e.ips {
		if t.After(bestT) {
			best, bestT = ip, t
		}
	}
	return best
}

type announceMsg struct {
	Magic   string `json:"magic"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	TCPPort int    `json:"tcp_port"`
	Session string `json:"session"` // 进程会话，重启即变化
}

// Run 启动发现服务：广播本机公告并接收其他节点的公告，直到 ctx 结束。
func Run(ctx context.Context, udpPort, tcpPort int, interval time.Duration, name, selfID string, store *Store) error {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	session, err := randSession()
	if err != nil {
		return fmt.Errorf("生成会话标识失败: %w", err)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: udpPort})
	if err != nil {
		return fmt.Errorf("监听发现端口 %d/udp 失败: %w", udpPort, err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	msg, _ := json.Marshal(announceMsg{
		Magic: announceMagic, ID: selfID, Name: name, TCPPort: tcpPort, Session: session,
	})
	sendAnnounce := func() {
		dsts := broadcastAddrs(udpPort)
		dsts = append(dsts, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: udpPort})
		for _, d := range dsts {
			c, err := net.DialUDP("udp4", nil, d)
			if err != nil {
				continue
			}
			c.Write(msg)
			c.Close()
		}
	}

	go func() {
		sendAnnounce()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sendAnnounce()
			}
		}
	}()

	buf := make([]byte, 4096)
	for {
		n, remote, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		var m announceMsg
		if json.Unmarshal(buf[:n], &m) != nil || m.Magic != announceMagic {
			log.Printf("忽略来自 %s 的非法公告包", remote)
			continue
		}
		if m.ID == selfID {
			continue
		}
		store.Upsert(m.ID, m.Name, remote.IP.String(), m.TCPPort, m.Session)
	}
}

// randSession 生成随机会话标识。
func randSession() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// broadcastAddrs 计算本机各 IPv4 网段的定向广播地址。
func broadcastAddrs(port int) []*net.UDPAddr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := make(map[string]bool)
	var out []*net.UDPAddr
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || len(ipn.Mask) != 4 {
				continue
			}
			b := make(net.IP, 4)
			for i := 0; i < 4; i++ {
				b[i] = ip4[i] | ^ipn.Mask[i]
			}
			key := b.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, &net.UDPAddr{IP: b, Port: port})
		}
	}
	return out
}
