package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// 1、定制CRD必须要有的字段：CR文件中引用的字段是json后的字段
// 子结构体：用于定制存储
type StorageConfig struct {
	StorageClassName string `json:"storageClassName"`
	Size             string `json:"size"`
}

// 子结构体：用于资源限制
// 资源请求定义
type ResourceRequests struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// 资源限制定义
type ResourceLimits struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// 资源要求定义
type ResourceRequirements struct {
	Requests ResourceRequests `json:"requests"`
	Limits   ResourceLimits   `json:"limits"`
}

type MysqlClusterSpec struct {
	//MasterConfig  string `json:"masterConfig"`
	//SlaveConfig   string `json:"slaveConfig"`
	Image         string               `json:"image"`
	Replicas      int32                `json:"replicas"`
	MasterService string               `json:"masterService"`
	SlaveService  string               `json:"slaveService"`
	Storage       StorageConfig        `json:"storage"`
	Resources     ResourceRequirements `json:"resources"`
}

// Raft状态枚举
type RaftState string

const (
	RaftStateFollower  RaftState = "follower"
	RaftStateCandidate RaftState = "candidate"
	RaftStateLeader    RaftState = "leader"
)

// Raft日志条目
type RaftLogEntry struct {
	Term    int64  `json:"term"`
	Index   int64  `json:"index"`
	Command string `json:"command"`
}

// Raft节点信息
type RaftNodeInfo struct {
	NodeId string    `json:"nodeId"`
	State  RaftState `json:"state"`
	Term   int64     `json:"term"`
}

// 2、定制status信息
type MysqlClusterStatus struct {
	Master      string                  `json:"master"`
	Slaves      []string                `json:"slaves"`
	RaftNodes   map[string]RaftNodeInfo `json:"raftNodes,omitempty"`
	RaftLeader  string                  `json:"raftLeader,omitempty"`
	RaftTerm    int64                   `json:"raftTerm,omitempty"`
	ClusterSize int32                   `json:"clusterSize,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

type MysqlCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MysqlClusterSpec   `json:"spec,omitempty"`
	Status MysqlClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type MysqlClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MysqlCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MysqlCluster{}, &MysqlClusterList{})
}
