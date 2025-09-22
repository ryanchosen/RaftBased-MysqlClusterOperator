package controller

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	databasev1 "github.com/ryansu/api/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// PeerInfo 存储对等节点信息
type PeerInfo struct {
	NodeId      string
	LastContact time.Time
	LastLatency time.Duration
	IsActive    bool
}

// Raft管理器
type RaftManager struct {
	client            client.Client
	nodeId            string
	cluster           *databasev1.MysqlCluster
	
	// Raft状态
	mu                sync.RWMutex
	state             databasev1.RaftState
	currentTerm       int64
	votedFor          string
	log               []databasev1.RaftLogEntry
	
	// 领导者状态
	leaderId          string
	nextIndex         map[string]int64
	matchIndex        map[string]int64
	
	// 选举相关
	electionTimeout   time.Duration
	heartbeatInterval time.Duration
	lastHeartbeat     time.Time
	electionCount     int64
	lastElection      time.Time
	
	// 日志复制
	commitIndex       int64
	lastApplied       int64
	
	// 网络通信
	peers             map[string]*PeerInfo
	heartbeatCount    int64
	
	// 控制通道
	stopCh            chan struct{}
	logger            logr.Logger
}

// 创建新的Raft管理器
func NewRaftManager(client client.Client, nodeId string, cluster *databasev1.MysqlCluster) *RaftManager {
	return &RaftManager{
		Client:           client,
		nodeId:           nodeId,
		currentTerm:      0,
		votedFor:         "",
		state:            databasev1.RaftStateFollower,
		log:              make([]databasev1.RaftLogEntry, 0),
		commitIndex:      0,
		lastApplied:      0,
		nextIndex:        make(map[string]int64),
		matchIndex:       make(map[string]int64),
		electionTimeout:  time.Duration(150+rand.Intn(150)) * time.Millisecond, // 150-300ms随机
		heartbeatTimeout: 50 * time.Millisecond,
		lastHeartbeat:    time.Now(),
		cluster:          cluster,
	}
}

// 启动Raft节点
func (rm *RaftManager) Start(ctx context.Context) {
	rm.ctx = ctx
	go rm.runElectionTimer()
	go rm.runHeartbeatTimer()
}

// 选举定时器
func (rm *RaftManager) runElectionTimer() {
	for {
		select {
		case <-rm.ctx.Done():
			return
		case <-time.After(rm.electionTimeout):
			rm.mu.Lock()
			if rm.state != databasev1.RaftStateLeader && time.Since(rm.lastHeartbeat) > rm.electionTimeout {
				rm.startElection()
			}
			rm.mu.Unlock()
		}
	}
}

// 心跳定时器
func (rm *RaftManager) runHeartbeatTimer() {
	for {
		select {
		case <-rm.ctx.Done():
			return
		case <-time.After(rm.heartbeatTimeout):
			rm.mu.Lock()
			if rm.state == databasev1.RaftStateLeader {
				rm.sendHeartbeats()
			}
			rm.mu.Unlock()
		}
	}
}

// 开始选举
func (rm *RaftManager) startElection() {
	log := log.FromContext(rm.ctx)
	
	rm.state = databasev1.RaftStateCandidate
	rm.currentTerm++
	rm.votedFor = rm.nodeId
	rm.lastHeartbeat = time.Now()
	
	// 重置选举超时
	rm.electionTimeout = time.Duration(150+rand.Intn(150)) * time.Millisecond
	
	log.Info("开始选举", "nodeId", rm.nodeId, "term", rm.currentTerm)
	
	// 获取集群中的所有节点
	nodes, err := rm.getClusterNodes()
	if err != nil {
		log.Error(err, "获取集群节点失败")
		return
	}
	
	votes := 1 // 投票给自己
	majority := len(nodes)/2 + 1
	
	// 向其他节点发送投票请求
	for _, node := range nodes {
		if node != rm.nodeId {
			go func(nodeId string) {
				if rm.sendVoteRequest(nodeId) {
					rm.mu.Lock()
					votes++
					if votes >= majority && rm.state == databasev1.RaftStateCandidate {
						rm.becomeLeader()
					}
					rm.mu.Unlock()
				}
			}(node)
		}
	}
}

// 成为领导者
func (rm *RaftManager) becomeLeader() {
	log := log.FromContext(rm.ctx)
	
	rm.state = databasev1.RaftStateLeader
	log.Info("成为领导者", "nodeId", rm.nodeId, "term", rm.currentTerm)
	
	// 初始化nextIndex和matchIndex
	nodes, _ := rm.getClusterNodes()
	for _, node := range nodes {
		if node != rm.nodeId {
			rm.nextIndex[node] = int64(len(rm.log)) + 1
			rm.matchIndex[node] = 0
		}
	}
	
	// 立即发送心跳
	rm.sendHeartbeats()
	
	// 更新集群状态
	rm.updateClusterStatus()
}

// 发送心跳
func (rm *RaftManager) sendHeartbeats() {
	nodes, err := rm.getClusterNodes()
	if err != nil {
		return
	}
	
	for _, node := range nodes {
		if node != rm.nodeId {
			go rm.sendAppendEntries(node, []databasev1.RaftLogEntry{})
		}
	}
}

// 发送投票请求
func (rm *RaftManager) sendVoteRequest(nodeId string) bool {
	// 通过ConfigMap发送投票请求消息
	message := map[string]string{
		"type":         "vote_request",
		"from":         rm.nodeId,
		"to":           nodeId,
		"term":         fmt.Sprintf("%d", rm.currentTerm),
		"candidateId":  rm.nodeId,
		"lastLogIndex": fmt.Sprintf("%d", int64(len(rm.log))),
		"lastLogTerm":  "0",
	}
	
	if len(rm.log) > 0 {
		message["lastLogTerm"] = fmt.Sprintf("%d", rm.log[len(rm.log)-1].Term)
	}
	
	return rm.sendMessage(nodeId, message)
}

// 处理投票请求
func (rm *RaftManager) HandleVoteRequest(candidateId string, term int64, lastLogIndex int64, lastLogTerm int64) bool {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	
	// 如果请求的任期小于当前任期，拒绝投票
	if term < rm.currentTerm {
		return false
	}
	
	// 如果请求的任期大于当前任期，更新当前任期并转为Follower
	if term > rm.currentTerm {
		rm.currentTerm = term
		rm.votedFor = ""
		rm.state = databasev1.RaftStateFollower
	}
	
	// 检查是否已经投票
	if rm.votedFor != "" && rm.votedFor != candidateId {
		return false
	}
	
	// 检查候选者的日志是否至少和自己的一样新
	myLastLogIndex := int64(len(rm.log))
	myLastLogTerm := int64(0)
	if len(rm.log) > 0 {
		myLastLogTerm = rm.log[len(rm.log)-1].Term
	}
	
	if lastLogTerm < myLastLogTerm || (lastLogTerm == myLastLogTerm && lastLogIndex < myLastLogIndex) {
		return false
	}
	
	// 投票给候选者
	rm.votedFor = candidateId
	rm.lastHeartbeat = time.Now()
	
	return true
}

// 发送AppendEntries
func (rm *RaftManager) sendAppendEntries(nodeId string, entries []databasev1.RaftLogEntry) bool {
	prevLogIndex := rm.nextIndex[nodeId] - 1
	prevLogTerm := int64(0)
	if prevLogIndex > 0 && int(prevLogIndex) <= len(rm.log) {
		prevLogTerm = rm.log[prevLogIndex-1].Term
	}
	
	message := map[string]string{
		"type":         "append_entries",
		"from":         rm.nodeId,
		"to":           nodeId,
		"term":         fmt.Sprintf("%d", rm.currentTerm),
		"leaderId":     rm.nodeId,
		"prevLogIndex": fmt.Sprintf("%d", prevLogIndex),
		"prevLogTerm":  fmt.Sprintf("%d", prevLogTerm),
		"leaderCommit": fmt.Sprintf("%d", rm.commitIndex),
		"entriesCount": fmt.Sprintf("%d", len(entries)),
	}
	
	return rm.sendMessage(nodeId, message)
}

// 处理AppendEntries
func (rm *RaftManager) HandleAppendEntries(leaderId string, term int64, prevLogIndex int64, prevLogTerm int64, entries []databasev1.RaftLogEntry, leaderCommit int64) bool {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	
	rm.lastHeartbeat = time.Now()
	
	// 如果请求的任期小于当前任期，拒绝
	if term < rm.currentTerm {
		return false
	}
	
	// 如果请求的任期大于等于当前任期，转为Follower
	if term >= rm.currentTerm {
		rm.currentTerm = term
		rm.state = databasev1.RaftStateFollower
		rm.votedFor = ""
	}
	
	// 检查日志一致性
	if prevLogIndex > 0 {
		if int64(len(rm.log)) < prevLogIndex {
			return false
		}
		if prevLogIndex > 0 && rm.log[prevLogIndex-1].Term != prevLogTerm {
			return false
		}
	}
	
	// 追加新的日志条目
	if len(entries) > 0 {
		rm.log = append(rm.log[:prevLogIndex], entries...)
	}
	
	// 更新commitIndex
	if leaderCommit > rm.commitIndex {
		rm.commitIndex = min(leaderCommit, int64(len(rm.log)))
	}
	
	return true
}

// 获取集群节点列表
func (rm *RaftManager) getClusterNodes() ([]string, error) {
	var nodes []string
	for i := int32(0); i < rm.cluster.Spec.Replicas; i++ {
		nodeName := fmt.Sprintf("%s-%d", rm.cluster.Name, i)
		nodes = append(nodes, nodeName)
	}
	return nodes, nil
}

// 通过ConfigMap发送消息
func (rm *RaftManager) sendMessage(nodeId string, message map[string]string) bool {
	configMapName := fmt.Sprintf("raft-message-%s-%d", nodeId, time.Now().UnixNano())
	
	configMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: rm.cluster.Namespace,
			Labels: map[string]string{
				"raft-message": "true",
				"target-node":  nodeId,
			},
		},
		Data: message,
	}
	
	err := rm.Create(rm.ctx, configMap)
	return err == nil
}

// 更新集群状态
func (rm *RaftManager) updateClusterStatus() {
	if rm.cluster.Status.RaftNodes == nil {
		rm.cluster.Status.RaftNodes = make(map[string]databasev1.RaftNodeInfo)
	}
	
	rm.cluster.Status.RaftNodes[rm.nodeId] = databasev1.RaftNodeInfo{
		NodeId: rm.nodeId,
		State:  rm.state,
		Term:   rm.currentTerm,
	}
	
	if rm.state == databasev1.RaftStateLeader {
		rm.cluster.Status.RaftLeader = rm.nodeId
		rm.cluster.Status.RaftTerm = rm.currentTerm
	}
	
	rm.cluster.Status.ClusterSize = rm.cluster.Spec.Replicas
	
	// 更新集群状态到Kubernetes
	rm.Status().Update(rm.ctx, rm.cluster)
}

// 辅助函数
func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}