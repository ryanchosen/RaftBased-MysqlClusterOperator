package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	databasev1 "github.com/ryansu/api/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// RaftMonitor 负责监控Raft集群状态和收集指标
type RaftMonitor struct {
	client      client.Client
	raftManager *RaftManager
	cluster     *databasev1.MysqlCluster
	logger      logr.Logger
}

// RaftMetrics Raft集群指标
type RaftMetrics struct {
	// 基础状态指标
	CurrentTerm     int64                    `json:"current_term"`
	CurrentState    databasev1.RaftState     `json:"current_state"`
	LeaderNodeId    string                   `json:"leader_node_id"`
	ClusterSize     int                      `json:"cluster_size"`
	ActiveNodes     int                      `json:"active_nodes"`
	
	// 选举相关指标
	ElectionCount   int64                    `json:"election_count"`
	LastElection    time.Time                `json:"last_election"`
	ElectionTimeout time.Duration            `json:"election_timeout"`
	
	// 日志复制指标
	LogIndex        int64                    `json:"log_index"`
	CommitIndex     int64                    `json:"commit_index"`
	AppliedIndex    int64                    `json:"applied_index"`
	
	// 网络状态指标
	HeartbeatCount  int64                    `json:"heartbeat_count"`
	LastHeartbeat   time.Time                `json:"last_heartbeat"`
	NetworkLatency  map[string]time.Duration `json:"network_latency"`
	
	// MySQL集成指标
	MasterNode      string                   `json:"master_node"`
	SlaveNodes      []string                 `json:"slave_nodes"`
	SemiSyncEnabled bool                     `json:"semi_sync_enabled"`
	ReplicationLag  map[string]time.Duration `json:"replication_lag"`
}

// NewRaftMonitor 创建新的Raft监控器
func NewRaftMonitor(client client.Client, raftManager *RaftManager, cluster *databasev1.MysqlCluster) *RaftMonitor {
	return &RaftMonitor{
		client:      client,
		raftManager: raftManager,
		cluster:     cluster,
		logger:      log.Log.WithName("raft-monitor"),
	}
}

// Start 启动监控
func (rm *RaftMonitor) Start(ctx context.Context) {
	rm.logger.Info("启动Raft监控器")
	
	// 启动指标收集协程
	go rm.collectMetrics(ctx)
	
	// 启动状态更新协程
	go rm.updateClusterStatus(ctx)
	
	// 启动健康检查协程
	go rm.healthCheck(ctx)
}

// collectMetrics 收集Raft指标
func (rm *RaftMonitor) collectMetrics(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second) // 每30秒收集一次指标
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			metrics := rm.gatherMetrics()
			rm.logMetrics(metrics)
			rm.updateMetricsConfigMap(ctx, metrics)
		}
	}
}

// gatherMetrics 收集当前指标
func (rm *RaftMonitor) gatherMetrics() *RaftMetrics {
	rm.raftManager.mu.RLock()
	defer rm.raftManager.mu.RUnlock()
	
	metrics := &RaftMetrics{
		CurrentTerm:     rm.raftManager.currentTerm,
		CurrentState:    rm.raftManager.state,
		LeaderNodeId:    rm.raftManager.leaderId,
		ClusterSize:     len(rm.raftManager.peers) + 1, // +1 for self
		ElectionCount:   rm.raftManager.electionCount,
		LastElection:    rm.raftManager.lastElection,
		ElectionTimeout: rm.raftManager.electionTimeout,
		LogIndex:        int64(len(rm.raftManager.log)),
		CommitIndex:     rm.raftManager.commitIndex,
		AppliedIndex:    rm.raftManager.lastApplied,
		HeartbeatCount:  rm.raftManager.heartbeatCount,
		LastHeartbeat:   rm.raftManager.lastHeartbeat,
		NetworkLatency:  make(map[string]time.Duration),
		MasterNode:      rm.cluster.Status.Master,
		SlaveNodes:      rm.cluster.Status.Slaves,
		ReplicationLag:  make(map[string]time.Duration),
	}
	
	// 计算活跃节点数
	activeNodes := 0
	for _, peer := range rm.raftManager.peers {
		if time.Since(peer.LastContact) < 2*rm.raftManager.heartbeatInterval {
			activeNodes++
		}
		metrics.NetworkLatency[peer.NodeId] = peer.LastLatency
	}
	metrics.ActiveNodes = activeNodes
	
	return metrics
}

// logMetrics 记录指标日志
func (rm *RaftMonitor) logMetrics(metrics *RaftMetrics) {
	rm.logger.Info("Raft集群指标",
		"term", metrics.CurrentTerm,
		"state", metrics.CurrentState,
		"leader", metrics.LeaderNodeId,
		"cluster_size", metrics.ClusterSize,
		"active_nodes", metrics.ActiveNodes,
		"log_index", metrics.LogIndex,
		"commit_index", metrics.CommitIndex,
	)
}

// updateMetricsConfigMap 更新指标ConfigMap
func (rm *RaftMonitor) updateMetricsConfigMap(ctx context.Context, metrics *RaftMetrics) {
	configMapName := fmt.Sprintf("%s-raft-metrics", rm.cluster.Name)
	
	// 将指标序列化为JSON
	metricsData := map[string]string{
		"current_term":     fmt.Sprintf("%d", metrics.CurrentTerm),
		"current_state":    string(metrics.CurrentState),
		"leader_node_id":   metrics.LeaderNodeId,
		"cluster_size":     fmt.Sprintf("%d", metrics.ClusterSize),
		"active_nodes":     fmt.Sprintf("%d", metrics.ActiveNodes),
		"election_count":   fmt.Sprintf("%d", metrics.ElectionCount),
		"log_index":        fmt.Sprintf("%d", metrics.LogIndex),
		"commit_index":     fmt.Sprintf("%d", metrics.CommitIndex),
		"heartbeat_count":  fmt.Sprintf("%d", metrics.HeartbeatCount),
		"master_node":      metrics.MasterNode,
		"last_updated":     time.Now().Format(time.RFC3339),
	}
	
	// 创建或更新ConfigMap
	err := rm.createOrUpdateConfigMap(ctx, configMapName, metricsData)
	if err != nil {
		rm.logger.Error(err, "更新指标ConfigMap失败", "configmap", configMapName)
	}
}

// updateClusterStatus 更新集群状态
func (rm *RaftMonitor) updateClusterStatus(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second) // 每10秒更新一次状态
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rm.syncClusterStatus(ctx)
		}
	}
}

// syncClusterStatus 同步集群状态到CRD
func (rm *RaftMonitor) syncClusterStatus(ctx context.Context) {
	rm.raftManager.mu.RLock()
	
	// 更新Raft相关状态
	rm.cluster.Status.RaftNodes = make(map[string]databasev1.RaftNodeInfo)
	
	// 添加当前节点信息
	rm.cluster.Status.RaftNodes[rm.raftManager.nodeId] = databasev1.RaftNodeInfo{
		NodeId:      rm.raftManager.nodeId,
		State:       rm.raftManager.state,
		Term:        rm.raftManager.currentTerm,
		LastContact: metav1.NewTime(time.Now()),
		IsActive:    true,
	}
	
	// 添加其他节点信息
	for _, peer := range rm.raftManager.peers {
		isActive := time.Since(peer.LastContact) < 2*rm.raftManager.heartbeatInterval
		rm.cluster.Status.RaftNodes[peer.NodeId] = databasev1.RaftNodeInfo{
			NodeId:      peer.NodeId,
			State:       databasev1.RaftStateFollower, // 假设其他节点都是Follower
			Term:        rm.raftManager.currentTerm,
			LastContact: metav1.NewTime(peer.LastContact),
			IsActive:    isActive,
		}
	}
	
	rm.cluster.Status.RaftLeader = rm.raftManager.leaderId
	rm.cluster.Status.RaftTerm = rm.raftManager.currentTerm
	rm.cluster.Status.ClusterSize = len(rm.raftManager.peers) + 1
	
	rm.raftManager.mu.RUnlock()
	
	// 更新CRD状态
	err := rm.client.Status().Update(ctx, rm.cluster)
	if err != nil {
		rm.logger.Error(err, "更新集群状态失败")
	}
}

// healthCheck 健康检查
func (rm *RaftMonitor) healthCheck(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second) // 每5秒检查一次健康状态
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rm.performHealthCheck()
		}
	}
}

// performHealthCheck 执行健康检查
func (rm *RaftMonitor) performHealthCheck() {
	rm.raftManager.mu.RLock()
	defer rm.raftManager.mu.RUnlock()
	
	// 检查选举超时
	if rm.raftManager.state == databasev1.RaftStateFollower {
		if time.Since(rm.raftManager.lastHeartbeat) > rm.raftManager.electionTimeout {
			rm.logger.Warn("检测到选举超时，可能需要触发新选举",
				"last_heartbeat", rm.raftManager.lastHeartbeat,
				"election_timeout", rm.raftManager.electionTimeout,
			)
		}
	}
	
	// 检查网络分区
	activeNodes := 0
	totalNodes := len(rm.raftManager.peers) + 1
	
	for _, peer := range rm.raftManager.peers {
		if time.Since(peer.LastContact) < 2*rm.raftManager.heartbeatInterval {
			activeNodes++
		}
	}
	
	if activeNodes < totalNodes/2 {
		rm.logger.Warn("检测到可能的网络分区",
			"active_nodes", activeNodes,
			"total_nodes", totalNodes,
			"majority_required", totalNodes/2+1,
		)
	}
	
	// 检查日志复制延迟
	if rm.raftManager.commitIndex > rm.raftManager.lastApplied {
		rm.logger.Warn("检测到日志应用延迟",
			"commit_index", rm.raftManager.commitIndex,
			"applied_index", rm.raftManager.lastApplied,
		)
	}
}

// createOrUpdateConfigMap 创建或更新ConfigMap
func (rm *RaftMonitor) createOrUpdateConfigMap(ctx context.Context, name string, data map[string]string) error {
	configMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: rm.cluster.Namespace,
			Labels: map[string]string{
				"app":     "mysql-cluster",
				"cluster": rm.cluster.Name,
				"type":    "raft-metrics",
			},
		},
		Data: data,
	}
	
	// 尝试创建ConfigMap
	err := rm.client.Create(ctx, configMap)
	if err != nil {
		// 如果已存在，则更新
		existingConfigMap := &v1.ConfigMap{}
		err = rm.client.Get(ctx, client.ObjectKey{
			Name:      name,
			Namespace: rm.cluster.Namespace,
		}, existingConfigMap)
		if err != nil {
			return err
		}
		
		existingConfigMap.Data = data
		return rm.client.Update(ctx, existingConfigMap)
	}
	
	return nil
}

// GetMetrics 获取当前指标
func (rm *RaftMonitor) GetMetrics() *RaftMetrics {
	return rm.gatherMetrics()
}

// GetClusterHealth 获取集群健康状态
func (rm *RaftMonitor) GetClusterHealth() map[string]interface{} {
	rm.raftManager.mu.RLock()
	defer rm.raftManager.mu.RUnlock()
	
	activeNodes := 0
	totalNodes := len(rm.raftManager.peers) + 1
	
	for _, peer := range rm.raftManager.peers {
		if time.Since(peer.LastContact) < 2*rm.raftManager.heartbeatInterval {
			activeNodes++
		}
	}
	
	hasQuorum := activeNodes >= totalNodes/2
	
	return map[string]interface{}{
		"total_nodes":    totalNodes,
		"active_nodes":   activeNodes,
		"has_quorum":     hasQuorum,
		"current_leader": rm.raftManager.leaderId,
		"current_term":   rm.raftManager.currentTerm,
		"cluster_state":  rm.raftManager.state,
		"last_election":  rm.raftManager.lastElection,
	}
}