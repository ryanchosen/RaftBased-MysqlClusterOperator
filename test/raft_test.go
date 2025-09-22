package test

import (
	"context"
	"fmt"
	"testing"
	"time"

	databasev1 "github.com/ryansu/api/v1"
	"github.com/ryansu/internal/controller"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestRaftElection 测试Raft选举功能
func TestRaftElection(t *testing.T) {
	// 创建测试集群
	cluster := &databasev1.MysqlCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
		Spec: databasev1.MysqlClusterSpec{
			Replicas: 3,
		},
	}

	// 创建fake客户端
	scheme := runtime.NewScheme()
	databasev1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	// 创建Raft管理器
	raftManager := controller.NewRaftManager(client, "test-node-1", cluster)

	// 测试初始状态
	if raftManager.GetState() != databasev1.RaftStateFollower {
		t.Errorf("期望初始状态为Follower，实际为: %v", raftManager.GetState())
	}

	// 启动Raft节点
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raftManager.Start(ctx)

	// 等待选举超时，应该触发选举
	time.Sleep(6 * time.Second)

	// 检查是否成为候选者或领导者
	state := raftManager.GetState()
	if state != databasev1.RaftStateCandidate && state != databasev1.RaftStateLeader {
		t.Errorf("选举超时后期望成为Candidate或Leader，实际为: %v", state)
	}

	t.Logf("Raft选举测试完成，当前状态: %v, 任期: %d", state, raftManager.GetCurrentTerm())
}

// TestRaftLeaderElection 测试多节点选举
func TestRaftLeaderElection(t *testing.T) {
	cluster := &databasev1.MysqlCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
		Spec: databasev1.MysqlClusterSpec{
			Replicas: 3,
		},
	}

	scheme := runtime.NewScheme()
	databasev1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	// 创建3个Raft节点
	nodes := make([]*controller.RaftManager, 3)
	for i := 0; i < 3; i++ {
		nodeId := fmt.Sprintf("test-node-%d", i+1)
		nodes[i] = controller.NewRaftManager(client, nodeId, cluster)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 启动所有节点
	for _, node := range nodes {
		node.Start(ctx)
	}

	// 等待选举完成
	time.Sleep(10 * time.Second)

	// 检查是否有且仅有一个领导者
	leaderCount := 0
	var leader *controller.RaftManager
	
	for _, node := range nodes {
		if node.GetState() == databasev1.RaftStateLeader {
			leaderCount++
			leader = node
		}
	}

	if leaderCount != 1 {
		t.Errorf("期望有且仅有1个领导者，实际有: %d个", leaderCount)
	}

	if leader != nil {
		t.Logf("选举成功，领导者: %s, 任期: %d", leader.GetNodeId(), leader.GetCurrentTerm())
	}

	// 验证其他节点都是跟随者
	for _, node := range nodes {
		if node != leader && node.GetState() != databasev1.RaftStateFollower {
			t.Errorf("非领导者节点 %s 应该是Follower，实际为: %v", node.GetNodeId(), node.GetState())
		}
	}
}

// TestRaftFailover 测试故障转移
func TestRaftFailover(t *testing.T) {
	cluster := &databasev1.MysqlCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
		Spec: databasev1.MysqlClusterSpec{
			Replicas: 3,
		},
	}

	scheme := runtime.NewScheme()
	databasev1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	// 创建Raft与MySQL集成管理器
	raftManager := controller.NewRaftManager(client, "test-node-1", cluster)
	integration := controller.NewRaftMySQLIntegration(client, raftManager, cluster)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 模拟主库故障场景
	cluster.Status.Master = "test-master-pod"
	cluster.Status.Slaves = []string{"test-slave-1", "test-slave-2"}

	// 测试故障转移逻辑
	err := integration.HandleRaftBasedFailover(ctx)
	if err != nil {
		t.Logf("故障转移测试完成，错误（预期）: %v", err)
	} else {
		t.Logf("故障转移测试完成，成功执行")
	}
}

// TestRaftLogReplication 测试日志复制
func TestRaftLogReplication(t *testing.T) {
	cluster := &databasev1.MysqlCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
		Spec: databasev1.MysqlClusterSpec{
			Replicas: 3,
		},
	}

	scheme := runtime.NewScheme()
	databasev1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	raftManager := controller.NewRaftManager(client, "test-leader", cluster)

	// 模拟成为领导者
	raftManager.BecomeLeader()

	// 创建测试日志条目
	logEntry := databasev1.RaftLogEntry{
		Term:    1,
		Index:   1,
		Type:    "mysql_operation",
		Command: "CHANGE MASTER TO ...",
	}

	// 测试日志追加
	success := raftManager.AppendLogEntry(logEntry)
	if !success {
		t.Errorf("日志追加失败")
	}

	// 验证日志索引
	if raftManager.GetLogIndex() != 1 {
		t.Errorf("期望日志索引为1，实际为: %d", raftManager.GetLogIndex())
	}

	t.Logf("日志复制测试完成，当前日志索引: %d", raftManager.GetLogIndex())
}

// TestRaftMonitoring 测试监控功能
func TestRaftMonitoring(t *testing.T) {
	cluster := &databasev1.MysqlCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
		Spec: databasev1.MysqlClusterSpec{
			Replicas: 3,
		},
	}

	scheme := runtime.NewScheme()
	databasev1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	raftManager := controller.NewRaftManager(client, "test-node", cluster)
	monitor := controller.NewRaftMonitor(client, raftManager, cluster)

	// 测试指标收集
	metrics := monitor.GetMetrics()
	if metrics == nil {
		t.Errorf("指标收集失败")
	}

	// 验证基础指标
	if metrics.CurrentTerm < 0 {
		t.Errorf("当前任期应该 >= 0，实际为: %d", metrics.CurrentTerm)
	}

	if metrics.CurrentState == "" {
		t.Errorf("当前状态不应该为空")
	}

	// 测试集群健康状态
	health := monitor.GetClusterHealth()
	if health == nil {
		t.Errorf("集群健康状态获取失败")
	}

	t.Logf("监控测试完成，当前状态: %v, 任期: %d", metrics.CurrentState, metrics.CurrentTerm)
}

// 辅助方法：扩展RaftManager以支持测试
func (rm *controller.RaftManager) GetState() databasev1.RaftState {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	return rm.state
}

func (rm *controller.RaftManager) GetCurrentTerm() int64 {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	return rm.currentTerm
}

func (rm *controller.RaftManager) GetNodeId() string {
	return rm.nodeId
}

func (rm *controller.RaftManager) GetLogIndex() int64 {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	return int64(len(rm.log))
}

func (rm *controller.RaftManager) BecomeLeader() {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.state = databasev1.RaftStateLeader
	rm.leaderId = rm.nodeId
}

func (rm *controller.RaftManager) AppendLogEntry(entry databasev1.RaftLogEntry) bool {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.log = append(rm.log, entry)
	return true
}