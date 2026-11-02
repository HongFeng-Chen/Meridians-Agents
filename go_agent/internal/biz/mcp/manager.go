package mcp

// Manager 维护 MCP 运行时实例（内存态）。
//
// 为什么实例不放数据库：http.Client（超时、代理、TLS、连接池）和子进程句柄
// 都是「这台进程此刻」的属性，无法序列化；数据库只存可复述的配置。
// 同一个服务换一套 client，只需 SetOptions 重连，不改任何一行 SQL。
//
// 并发模型：实例表用一把互斥锁保护，连接/关闭在锁外进行，
// 同一服务的并发连接由 dialMu 串行化（避免出现两个子进程）。

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"time"
)

// instance 单个运行时实例
type instance struct {
	server      Server
	opts        ClientOptions
	client      Client
	httpClient  *http.Client
	info        ServerInfo
	connectedAt time.Time
}

// Manager MCP 实例管理器
type Manager struct {
	dialer  Dialer
	clients ClientProvider

	mu        sync.RWMutex
	instances map[uint64]*instance

	dialMu sync.Mutex
}

// NewManager 创建管理器
func NewManager(dialer Dialer, clients ClientProvider) *Manager {
	return &Manager{
		dialer:    dialer,
		clients:   clients,
		instances: make(map[uint64]*instance),
	}
}

// Connect 建立连接并完成 initialize 握手。
//
// 已存在同配置的实例时直接复用（返回上次握手信息），
// 配置或运行时 client 参数变化则先断开再重连。
func (m *Manager) Connect(ctx context.Context, s Server, opts ClientOptions) (ServerInfo, error) {
	if cur, ok := m.get(s.ID); ok && cur.sameConfig(s, opts) {
		return cur.info, nil
	}
	m.dialMu.Lock()
	defer m.dialMu.Unlock()

	// 双重检查：等锁期间可能已被其它请求连上
	if cur, ok := m.get(s.ID); ok && cur.sameConfig(s, opts) {
		return cur.info, nil
	}
	if _, ok := m.get(s.ID); ok {
		_ = m.Disconnect(context.Background(), s.ID)
	}

	var hc *http.Client
	if s.Transport == TransportHTTP && m.clients != nil {
		hc = m.clients.Client(s, opts)
	}
	cli, err := m.dialer.Dial(ctx, s, hc)
	if err != nil {
		return ServerInfo{}, err
	}
	info, err := cli.Initialize(ctx)
	if err != nil {
		_ = cli.Close()
		return ServerInfo{}, err
	}
	inst := &instance{
		server:      s,
		opts:        opts,
		client:      cli,
		httpClient:  hc,
		info:        info,
		connectedAt: time.Now(),
	}
	m.mu.Lock()
	m.instances[s.ID] = inst
	m.mu.Unlock()
	return info, nil
}

// Ensure 返回可用客户端；未连接时按传入配置连接（工具列表/调用都用它）
func (m *Manager) Ensure(ctx context.Context, s Server, opts ClientOptions) (Client, error) {
	if cur, ok := m.get(s.ID); ok && cur.sameConfig(s, opts) {
		return cur.client, nil
	}
	if _, err := m.Connect(ctx, s, opts); err != nil {
		return nil, err
	}
	cur, ok := m.get(s.ID)
	if !ok {
		return nil, fmt.Errorf("%w: 实例创建后立即丢失", ErrNotConnected)
	}
	return cur.client, nil
}

// Disconnect 断开并释放实例（stdio 会结束子进程）
func (m *Manager) Disconnect(ctx context.Context, id uint64) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	if ok {
		delete(m.instances, id)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	if err := inst.client.Close(); err != nil {
		return fmt.Errorf("关闭 MCP 实例: %w", err)
	}
	return nil
}

// Ping 探活；未连接则立即返回错误（由用例层决定是否自动重连）
func (m *Manager) Ping(ctx context.Context, s Server, opts ClientOptions) error {
	cli, ok := m.Client(s.ID)
	if !ok {
		return ErrNotConnected
	}
	_ = opts
	return cli.Ping(ctx)
}

// Client 取已连接实例的客户端
func (m *Manager) Client(id uint64) (Client, bool) {
	inst, ok := m.get(id)
	if !ok {
		return nil, false
	}
	return inst.client, true
}

// Running 运行中的实例数
func (m *Manager) Running() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return int64(len(m.instances))
}

// Options 当前实例的运行时参数
func (m *Manager) Options(id uint64) (ClientOptions, bool) {
	inst, ok := m.get(id)
	if !ok {
		return ClientOptions{}, false
	}
	return inst.opts, true
}

// Info 当前实例的握手信息
func (m *Manager) Info(id uint64) (ServerInfo, bool) {
	inst, ok := m.get(id)
	if !ok {
		return ServerInfo{}, false
	}
	return inst.info, true
}

// ConnectedAt 实例建立时间
func (m *Manager) ConnectedAt(id uint64) (time.Time, bool) {
	inst, ok := m.get(id)
	if !ok {
		return time.Time{}, false
	}
	return inst.connectedAt, true
}

// CloseAll 进程退出时统一关闭
func (m *Manager) CloseAll() {
	m.mu.Lock()
	ids := make([]uint64, 0, len(m.instances))
	for id := range m.instances {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.Disconnect(context.Background(), id)
	}
}

func (m *Manager) get(id uint64) (*instance, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inst, ok := m.instances[id]
	return inst, ok
}

// sameConfig 判断配置与运行时参数是否与实例一致
func (i *instance) sameConfig(s Server, opts ClientOptions) bool {
	return i.server.Transport == s.Transport &&
		i.server.Endpoint == s.Endpoint &&
		i.server.Command == s.Command &&
		reflect.DeepEqual(i.server.Args, s.Args) &&
		reflect.DeepEqual(i.server.Env, s.Env) &&
		reflect.DeepEqual(i.server.Headers, s.Headers) &&
		i.opts == opts
}
